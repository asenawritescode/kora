//go:build integration

package analytics

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

func TestDrillIntegrationMySQL(t *testing.T) {
	runDrillIntegration(t, "KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN", "mysql", "mysql")
}

func TestDrillIntegrationPostgres(t *testing.T) {
	runDrillIntegration(t, "KORA_ANALYTICS_DISPOSABLE_POSTGRES_DSN", "postgres", "postgres")
}

// runDrillIntegration checks that the drill filter, fed to the list query, counts
// exactly the records behind each number the query returned.
func runDrillIntegration(t *testing.T, dsnVar, driver, dialectName string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(dsnVar))
	if dsn == "" {
		t.Skipf("set %s to a disposable %s database", dsnVar, dialectName)
	}
	database, cleanup := openIsolatedCompareDatabase(t, dsn, driver, dialectName)
	defer cleanup()
	dialect := db.Resolve(dialectName)
	seedLiveTables(t, database, dialect)
	model := taskModel(SemanticFilter{Field: "status", Operator: "=", Values: []string{"Paid"}})
	model.Dimensions = append(model.Dimensions, SemanticDimension{Name: "status", Field: "status", Type: "category"})
	model.Measures = append(model.Measures, SemanticMeasure{Name: "paid_count", Aggregation: "count", Filters: []SemanticFilter{{Field: "status", Operator: "=", Values: []string{"Paid"}}}})
	catalog := &SemanticCatalog{Models: []SemanticModel{model}}
	site := "drill-" + dialectName
	engine := func(scope contract.BusinessScope) *QueryEngine {
		return &QueryEngine{DB: database, SiteName: site, Dialect: dialect, BusinessScope: scope}
	}
	ask := func(e *QueryEngine, registry *doctype.Registry, request SemanticRequest) *SemanticQueryResult {
		t.Helper()
		response, err := e.ResolveSemanticQuery(catalog, request, registry)
		if err != nil {
			t.Fatal("resolve query:", err)
		}
		return &response.Queries[0]
	}
	// listCount runs the drill filter through the list API's own filter parser.
	listCount := func(drill *DrillResponse) string {
		t.Helper()
		raw, err := json.Marshal(drill.Filters)
		if err != nil {
			t.Fatal("encode drill filters:", err)
		}
		set, err := orm.ParseFilters(string(raw))
		if err != nil {
			t.Fatal("parse drill filters:", err)
		}
		clause, args, err := set.ToSQL()
		if err != nil {
			t.Fatal("drill filters to SQL:", err)
		}
		var n int
		statement := "SELECT COUNT(*) FROM " + dialect.QuoteIdent("tabTask") + " WHERE " + clause
		if err := database.QueryRow(db.Rebind(dialect, statement), args...).Scan(&n); err != nil {
			t.Fatal("count drill records:", err)
		}
		return fmt.Sprint(n)
	}
	jan := QueryTime{From: "2026-01-01", To: "2026-01-31"}
	drill := func(request DrillRequest) *DrillResponse {
		t.Helper()
		response, err := BuildDrill(catalog, taskRegistry(), request, contract.BusinessScope{Kind: contract.ScopeKindTenant})
		if err != nil {
			t.Fatal("build drill:", err)
		}
		return response
	}

	t.Run("link category drills by its raw key", func(t *testing.T) {
		result := ask(engine(contract.BusinessScope{}), taskRegistry(), SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_count"}, Dimensions: []string{"customer"}}}, Time: jan})
		if len(result.Rows) != 3 {
			t.Fatalf("rows = %v, want 3 customers", result.Rows)
		}
		for _, row := range result.Rows {
			request := DrillRequest{Model: "task", Measure: "paid_count", Dimension: "customer", Value: fmt.Sprint(row["customer.key"]), Time: jan}
			if got, want := listCount(drill(request)), fmt.Sprint(row["paid_count"]); got != want {
				t.Fatalf("customer %v: list counts %s, query counted %s", row["customer"], got, want)
			}
		}
	})

	t.Run("plain category drills by its bare value", func(t *testing.T) {
		result := ask(engine(contract.BusinessScope{}), taskRegistry(), SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_count"}, Dimensions: []string{"status"}}}, Time: jan})
		if len(result.Rows) != 1 {
			t.Fatalf("rows = %v, want one status", result.Rows)
		}
		request := DrillRequest{Model: "task", Measure: "paid_count", Dimension: "status", Value: fmt.Sprint(result.Rows[0]["status"]), Time: jan}
		if got, want := listCount(drill(request)), fmt.Sprint(result.Rows[0]["paid_count"]); got != want {
			t.Fatalf("status drill lists %s, query counted %s", got, want)
		}
	})

	t.Run("time bucket drills to its bucket and range", func(t *testing.T) {
		monthly := QueryTime{From: "2026-01-01", To: "2026-02-28", Granularity: "month"}
		result := ask(engine(contract.BusinessScope{}), taskRegistry(), SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_count"}}}, Time: monthly})
		if len(result.Rows) != 2 {
			t.Fatalf("rows = %v, want two months", result.Rows)
		}
		for _, row := range result.Rows {
			request := DrillRequest{Model: "task", Measure: "paid_count", Dimension: "transaction_date", Value: fmt.Sprint(row["transaction_date"]), Time: monthly}
			if got, want := listCount(drill(request)), fmt.Sprint(row["paid_count"]); got != want {
				t.Fatalf("bucket %v: list counts %s, query counted %s", row["transaction_date"], got, want)
			}
		}
	})

	t.Run("legal entity scope narrows the drill as the query", func(t *testing.T) {
		scope := contract.BusinessScope{TenantID: contract.TenantID(site), Kind: contract.ScopeKindLegalEntity, LegalEntityID: "LE-1"}
		entityCatalog := &SemanticCatalog{Models: []SemanticModel{model}}
		response, err := engine(scope).ResolveSemanticQuery(entityCatalog, SemanticRequest{Queries: []ModelQuery{{Model: "task", Measures: []string{"paid_count"}, Dimensions: []string{"customer"}}}, Time: jan}, entityTaskRegistry())
		if err != nil {
			t.Fatal("resolve scoped query:", err)
		}
		result := response.Queries[0]
		if len(result.Rows) != 2 {
			t.Fatalf("LE-1 rows = %v, want its two customers", result.Rows)
		}
		for _, row := range result.Rows {
			scoped, err := BuildDrill(entityCatalog, entityTaskRegistry(), DrillRequest{Model: "task", Measure: "paid_count", Dimension: "customer", Value: fmt.Sprint(row["customer.key"]), Time: jan}, scope)
			if err != nil {
				t.Fatal("build scoped drill:", err)
			}
			if got, want := listCount(scoped), fmt.Sprint(row["paid_count"]); got != want {
				t.Fatalf("LE-1 customer %v: list counts %s, query counted %s", row["customer"], got, want)
			}
		}
	})
}
