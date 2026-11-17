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

// orderRollups are stored task_count_by_customer rows for January. Totals:
// CUST-3 5, CUST-1 3, CUST-2 1, CUST-4 1 (CUST-4 has no Customer label).
var orderRollups = []struct {
	dimension, date string
	value           float64
}{
	{"customer=CUST-1", "2026-01-05", 2},
	{"customer=CUST-1", "2026-01-07", 1},
	{"customer=CUST-2", "2026-01-06", 1},
	{"customer=CUST-3", "2026-01-08", 4},
	{"customer=CUST-3", "2026-01-09", 1},
	{"customer=CUST-4", "2026-01-10", 1},
}

func TestOrderIntegrationMySQL(t *testing.T) {
	runOrderIntegration(t, "KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN", "mysql", "mysql")
}

func TestOrderIntegrationPostgres(t *testing.T) {
	runOrderIntegration(t, "KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN", "postgres", "postgres")
}

func runOrderIntegration(t *testing.T, dsnVar, driver, dialectName string) {
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
	seedOrderCustomers(t, database, dialect)
	site := "order-" + dialectName
	for _, seed := range orderRollups {
		insertOrderRollup(t, database, dialect, site, seed.dimension, seed.date, seed.value)
	}
	engine := &QueryEngine{DB: database, SiteName: site, Dialect: dialect}
	catalog := &SemanticCatalog{Models: []SemanticModel{taskModel()}}
	run := func(request SemanticRequest) *SemanticQueryResult {
		t.Helper()
		response, err := engine.ResolveSemanticQuery(catalog, request, taskRegistry())
		if err != nil {
			t.Fatal("resolve rollup query:", err)
		}
		return &response.Queries[0]
	}
	jan := QueryTime{From: "2026-01-01", To: "2026-01-31"}

	t.Run("unordered labels sort alphabetically", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}, Dimensions: []string{"customer"}}}, Time: jan})
		if names := rowLabelsOf(result.Rows, "customer"); fmt.Sprint(names) != "[Acme Ltd Beta Co CUST-4 Gamma Ltd]" {
			t.Fatalf("labels = %v, want alphabetical with unlabelled id kept", names)
		}
		if got := labelledNumbers(result.Rows, "customer", "count"); fmt.Sprint(got) != "map[Acme Ltd:3 Beta Co:1 CUST-4:1 Gamma Ltd:5]" {
			t.Fatalf("counts = %v", got)
		}
	})

	t.Run("measure_desc ranks before limit", func(t *testing.T) {
		result := run(SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"count"}, Dimensions: []string{"customer"}, Order: OrderMeasureDesc}}, Time: jan, Limit: 2})
		if names := rowLabelsOf(result.Rows, "customer"); fmt.Sprint(names) != "[Gamma Ltd Acme Ltd]" {
			t.Fatalf("ranked labels = %v, want [Gamma Ltd Acme Ltd]", names)
		}
	})
}

func seedOrderCustomers(t *testing.T, database *sql.DB, dialect db.Dialect) {
	t.Helper()
	q := dialect.QuoteIdent
	create := "CREATE TABLE " + q("tabCustomer") + " (" + q("name") + " VARCHAR(64) PRIMARY KEY, " + q("customer_name") + " VARCHAR(128))"
	if _, err := database.Exec(create); err != nil {
		t.Fatal("create customer table:", err)
	}
	insert := "INSERT INTO " + q("tabCustomer") + " (" + q("name") + ", " + q("customer_name") + ") VALUES (?, ?)"
	for id, label := range map[string]string{"CUST-1": "Acme Ltd", "CUST-2": "Beta Co", "CUST-3": "Gamma Ltd"} {
		if _, err := database.Exec(db.Rebind(dialect, insert), id, label); err != nil {
			t.Fatal("insert customer:", err)
		}
	}
}

func insertOrderRollup(t *testing.T, database *sql.DB, dialect db.Dialect, site, dimension, date string, value float64) {
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
