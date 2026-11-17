//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
)

// liveSeedTasks are DocType rows read live by measures with filters. Only the
// "Paid" rows count toward paid_amount.
var liveSeedTasks = []struct {
	name, date, status, customer string
	amount                       float64
}{
	{"T1", "2026-01-05", "Paid", "CUST-1", 100},
	{"T2", "2026-01-06", "Paid", "CUST-2", 50},
	{"T3", "2026-01-07", "Open", "CUST-1", 70},
	{"T4", "2026-01-08", "Paid", "CUST-3", 300},
	{"T5", "2026-02-01", "Paid", "CUST-1", 999},
}

// liveSeedRollups are stored task_count_by_customer rows. CUST-1 counts 3 in
// January, CUST-2 and CUST-3 count 1 each.
var liveSeedRollups = []struct {
	dimension, date string
	value           float64
}{
	{"customer=CUST-1", "2026-01-05", 2},
	{"customer=CUST-1", "2026-01-07", 1},
	{"customer=CUST-2", "2026-01-06", 1},
	{"customer=CUST-3", "2026-01-08", 1},
}

func TestLiveMeasureIntegrationMySQL(t *testing.T) {
	runLiveMeasureIntegration(t, "KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN", "mysql", "mysql")
}

func TestLiveMeasureIntegrationPostgres(t *testing.T) {
	runLiveMeasureIntegration(t, "KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN", "postgres", "postgres")
}

func runLiveMeasureIntegration(t *testing.T, dsnVar, driver, dialectName string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(dsnVar))
	if dsn == "" {
		t.Skipf("set %s to a disposable %s database", dsnVar, dialectName)
	}
	database, cleanup := openIsolatedCompareDatabase(t, dsn, driver, dialectName)
	defer cleanup()
	dialect := db.Resolve(dialectName)
	if err := BootstrapTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap analytics tables:", err)
	}
	seedLiveTables(t, database, dialect)
	site := "live-" + dialectName
	for _, seed := range liveSeedRollups {
		insertLiveRollup(t, database, dialect, site, seed.dimension, seed.date, seed.value)
	}
	engine := &QueryEngine{DB: database, SiteName: site, Dialect: dialect}
	catalog := &SemanticCatalog{Models: []SemanticModel{liveTaskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})}}
	run := func(request SemanticRequest) *SemanticQueryResult {
		t.Helper()
		response, err := engine.ResolveSemanticQuery(catalog, request, liveTaskRegistry())
		if err != nil {
			t.Fatal("resolve live query:", err)
		}
		return &response.Queries[0]
	}
	jan := QueryTime{From: "2026-01-01", To: "2026-01-31"}

	t.Run("filtered measure by linked label", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}, Dimensions: []string{"customer"}}}, Time: jan})
		if got := labelledNumbers(result.Rows, "customer", "paid_amount"); fmt.Sprint(got) != "map[Acme Ltd:100 Beta Co:50 Gamma Ltd:300]" {
			t.Fatalf("paid by customer = %v", got)
		}
		if names := rowLabelsOf(result.Rows, "customer"); fmt.Sprint(names) != "[Acme Ltd Beta Co Gamma Ltd]" {
			t.Fatalf("labels = %v, want alphabetical", names)
		}
	})

	t.Run("measure_desc keeps rank and limit across rollup and live measures", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount", "count"}, Dimensions: []string{"customer"}, Order: OrderMeasureDesc}}, Time: jan, Limit: 2})
		if names := rowLabelsOf(result.Rows, "customer"); fmt.Sprint(names) != "[Gamma Ltd Acme Ltd]" {
			t.Fatalf("ranked labels = %v, want [Gamma Ltd Acme Ltd]", names)
		}
		if got := labelledNumbers(result.Rows, "customer", "count"); fmt.Sprint(got) != "map[Acme Ltd:3 Gamma Ltd:1]" {
			t.Fatalf("rollup counts = %v", got)
		}
	})

	t.Run("query filter on the grouping dimension", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}, Dimensions: []string{"customer"}, Filters: []SemanticFilter{{Field: "customer", Operator: "in", Values: []string{"CUST-1"}}}}}, Time: jan})
		if got := labelledNumbers(result.Rows, "customer", "paid_amount"); fmt.Sprint(got) != "map[Acme Ltd:100]" {
			t.Fatalf("filtered rows = %v", got)
		}
	})

	t.Run("time buckets apply the measure filter", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}}}, Time: QueryTime{From: "2026-01-01", To: "2026-02-28", Granularity: "month"}})
		if got := valueColumn(result.Rows, "paid_amount"); fmt.Sprint(got) != "[450 999]" {
			t.Fatalf("monthly paid = %v, want [450 999]", got)
		}
	})

	t.Run("compare includes the live measure", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}}}, Time: QueryTime{From: "2026-01-05", To: "2026-01-08"}, Compare: CompareModePreviousPeriod})
		if got := *result.Comparison.Totals.Current["paid_amount"]; got != 450 {
			t.Fatalf("current total = %v, want 450", got)
		}
		if got := *result.Comparison.Totals.Previous["paid_amount"]; got != 0 {
			t.Fatalf("previous total = %v, want 0", got)
		}
	})
}

func seedLiveTables(t *testing.T, database *sql.DB, dialect db.Dialect) {
	t.Helper()
	q := dialect.QuoteIdent
	statements := []string{
		"CREATE TABLE " + q("tabTask") + " (" + q("name") + " VARCHAR(64) PRIMARY KEY, " + q("transaction_date") + " DATE, " + q("status") + " VARCHAR(32), " + q("customer") + " VARCHAR(64), " + q("amount") + " DECIMAL(12,2))",
		"CREATE TABLE " + q("tabCustomer") + " (" + q("name") + " VARCHAR(64) PRIMARY KEY, " + q("customer_name") + " VARCHAR(128))",
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal("create live tables:", err)
		}
	}
	taskInsert := "INSERT INTO " + q("tabTask") + " (" + q("name") + ", " + q("transaction_date") + ", " + q("status") + ", " + q("customer") + ", " + q("amount") + ") VALUES (?, ?, ?, ?, ?)"
	for _, task := range liveSeedTasks {
		if _, err := database.Exec(db.Rebind(dialect, taskInsert), task.name, task.date, task.status, task.customer, task.amount); err != nil {
			t.Fatal("insert task:", err)
		}
	}
	customerInsert := "INSERT INTO " + q("tabCustomer") + " (" + q("name") + ", " + q("customer_name") + ") VALUES (?, ?)"
	for id, label := range map[string]string{"CUST-1": "Acme Ltd", "CUST-2": "Beta Co", "CUST-3": "Gamma Ltd"} {
		if _, err := database.Exec(db.Rebind(dialect, customerInsert), id, label); err != nil {
			t.Fatal("insert customer:", err)
		}
	}
}

func insertLiveRollup(t *testing.T, database *sql.DB, dialect db.Dialect, site, dimension, date string, value float64) {
	t.Helper()
	query := "INSERT INTO _kora_analytics_daily (site, scope_kind, legal_entity_id, doctype, metric, dimension, date, value) VALUES (?, 'tenant', '', 'Task', 'task_count_by_customer', ?, ?, ?)"
	if _, err := database.Exec(db.Rebind(dialect, query), site, dimension, date, value); err != nil {
		t.Fatal("insert rollup:", err)
	}
}

// labelledNumbers maps the label column to a numeric measure, for order-free checks.
func labelledNumbers(rows []map[string]any, label, measure string) map[string]float64 {
	out := map[string]float64{}
	for _, row := range rows {
		value, _ := semanticNumber(row[measure])
		out[fmt.Sprint(row[label])] = value
	}
	return out
}

func rowLabelsOf(rows []map[string]any, label string) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprint(row[label]))
	}
	return out
}
