//go:build integration

package analytics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
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

// liveEntities assigns each task to a legal entity. LE-1 owns T1-T3 and LE-2
// owns T4-T5, so an entity's January paid total is its own rows only.
var liveEntities = map[string]string{"T1": "LE-1", "T2": "LE-1", "T3": "LE-1", "T4": "LE-2", "T5": "LE-2"}

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
	catalog := &SemanticCatalog{Models: []SemanticModel{taskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})}}
	run := func(request SemanticRequest) *SemanticQueryResult {
		t.Helper()
		response, err := engine.ResolveSemanticQuery(catalog, request, taskRegistry())
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

	t.Run("non-link category rows hold the plain value", func(t *testing.T) {
		statusCategoryCheck(t, database, dialect, site)
	})

	t.Run("time buckets apply the measure filter", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}}}, Time: QueryTime{From: "2026-01-01", To: "2026-02-28", Granularity: "month"}})
		if got := valueColumn(result.Rows, "paid_amount"); fmt.Sprint(got) != "[450 999]" {
			t.Fatalf("monthly paid = %v, want [450 999]", got)
		}
	})

	scoped := func(kind contract.BusinessScopeKind, entity string) *QueryEngine {
		scope := contract.BusinessScope{TenantID: contract.TenantID(site), Kind: kind, LegalEntityID: entity}
		return &QueryEngine{DB: database, SiteName: site, Dialect: dialect, BusinessScope: scope}
	}
	runScoped := func(e *QueryEngine, registry *doctype.Registry, request SemanticRequest) *SemanticQueryResult {
		t.Helper()
		response, err := e.ResolveSemanticQuery(catalog, request, registry)
		if err != nil {
			t.Fatal("resolve scoped live query:", err)
		}
		return &response.Queries[0]
	}
	paidByCustomer := SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_amount"}, Dimensions: []string{"customer"}}}, Time: jan}

	t.Run("legal entity scope sees only its own totals", func(t *testing.T) {
		result := runScoped(scoped(contract.ScopeKindLegalEntity, "LE-1"), entityTaskRegistry(), paidByCustomer)
		if got := labelledNumbers(result.Rows, "customer", "paid_amount"); fmt.Sprint(got) != "map[Acme Ltd:100 Beta Co:50]" {
			t.Fatalf("LE-1 paid by customer = %v, want only LE-1 rows (not the 450 tenant total)", got)
		}
	})

	t.Run("each legal entity sees its own rows", func(t *testing.T) {
		result := runScoped(scoped(contract.ScopeKindLegalEntity, "LE-2"), entityTaskRegistry(), paidByCustomer)
		if got := labelledNumbers(result.Rows, "customer", "paid_amount"); fmt.Sprint(got) != "map[Gamma Ltd:300]" {
			t.Fatalf("LE-2 paid by customer = %v", got)
		}
	})

	t.Run("tenant scope on an entity-owned source has no rows, as rollups", func(t *testing.T) {
		result := runScoped(scoped(contract.ScopeKindTenant, ""), entityTaskRegistry(), paidByCustomer)
		if len(result.Rows) != 0 {
			t.Fatalf("tenant scope rows = %v, want none", result.Rows)
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
		"CREATE TABLE " + q("tabTask") + " (" + q("name") + " VARCHAR(64) PRIMARY KEY, " + q("transaction_date") + " DATE, " + q("status") + " VARCHAR(32), " + q("customer") + " VARCHAR(64), " + q("amount") + " DECIMAL(12,2), " + q("legal_entity") + " VARCHAR(64))",
		"CREATE TABLE " + q("tabCustomer") + " (" + q("name") + " VARCHAR(64) PRIMARY KEY, " + q("customer_name") + " VARCHAR(128))",
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal("create live tables:", err)
		}
	}
	taskInsert := "INSERT INTO " + q("tabTask") + " (" + q("name") + ", " + q("transaction_date") + ", " + q("status") + ", " + q("customer") + ", " + q("amount") + ", " + q("legal_entity") + ") VALUES (?, ?, ?, ?, ?, ?)"
	for _, task := range liveSeedTasks {
		if _, err := database.Exec(db.Rebind(dialect, taskInsert), task.name, task.date, task.status, task.customer, task.amount, liveEntities[task.name]); err != nil {
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

// statusCategoryCheck proves a non-link category returns its plain value from
// both the live path (filtered measure) and the rollup path.
func statusCategoryCheck(t *testing.T, database *sql.DB, dialect db.Dialect, site string) {
	t.Helper()
	model := taskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})
	model.Dimensions = append(model.Dimensions, SemanticDimension{Name: "status", Field: "status", Type: "category"})
	engine := &QueryEngine{DB: database, SiteName: site, Dialect: dialect}
	query := func(measure string) map[string]any {
		t.Helper()
		request := SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{measure}, Dimensions: []string{"status"}}}, Time: QueryTime{From: "2026-01-01", To: "2026-01-31"}}
		response, err := engine.ResolveSemanticQuery(&SemanticCatalog{Models: []SemanticModel{model}}, request, taskRegistry())
		if err != nil {
			t.Fatal("resolve status query:", err)
		}
		rows := response.Queries[0].Rows
		if len(rows) != 1 {
			t.Fatalf("%s rows = %v, want one", measure, rows)
		}
		return rows[0]
	}
	if row := query("paid_amount"); row["status"] != "Paid" {
		t.Fatalf("live status row = %#v, want plain \"Paid\"", row)
	}
	insert := "INSERT INTO _kora_analytics_daily (site, scope_kind, legal_entity_id, doctype, metric, dimension, date, value) VALUES (?, 'tenant', '', 'Task', 'task_count_by_status', 'status=Paid', '2026-01-05', 3)"
	if _, err := database.Exec(db.Rebind(dialect, insert), site); err != nil {
		t.Fatal("insert status rollup:", err)
	}
	if row := query("count"); row["status"] != "Paid" || fmt.Sprint(row["count"]) != "3" {
		t.Fatalf("rollup status row = %#v, want plain \"Paid\" with count 3", row)
	}
}
