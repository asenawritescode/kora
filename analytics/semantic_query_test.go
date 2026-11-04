package analytics

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

func TestSemanticRollupQueryAppliesFiltersBeforeBoundedLimit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT dimension, SUM(value) AS value FROM _kora_analytics_daily WHERE site = ? AND scope_kind = ? AND legal_entity_id = ? AND doctype = ? AND metric = ? AND date >= ? AND date <= ? AND `dimension` = ? GROUP BY dimension ORDER BY value DESC LIMIT ?")).
		WithArgs("tenant-a", "tenant", "", "Cake Order", "cake_orders_count_by_flavour", "2026-01-01", "2026-01-31", "flavour=vanilla", 1).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).AddRow("flavour=vanilla", 3.0))
	engine := &QueryEngine{DB: database, SiteName: "tenant-a", Dialect: kdb.Resolve("mysql")}
	result, err := engine.Resolve(&Metric{
		Name: "cake_orders_count_by_flavour", DocType: "Cake Order", Type: MetricCountByField,
		Field: "flavour", TimeField: "order_date",
	}, QueryRequest{
		From: "2026-01-01", To: "2026-01-31", Dimension: "flavour", Limit: 1,
		Filters: []SemanticFilter{{Field: "flavour", Operator: "=", Values: []string{"vanilla"}}},
	})
	if err != nil {
		t.Fatalf("resolve bounded semantic query: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["dimension"] != "flavour=vanilla" {
		t.Fatalf("unexpected bounded query result: %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestPublishedProjectionAnalyticsUsesScopedSnapshotAndReportsFreshness(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	asOf := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	data := json.RawMessage(`[{"id":"invoice-1","period":"2026-10-01","amount":12,"assigned_to":"alice"},{"id":"invoice-2","period":"2026-10-01","amount":8,"assigned_to":"bob"},{"id":"invoice-3","period":"2026-11-01","amount":5,"assigned_to":"alice"}]`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT source_revision, as_of, freshness, data FROM _kora_analytics_module_projection WHERE site = ? AND projection_id = ? AND owner_module_id = ? AND scope_kind = ? AND legal_entity_id = ? AND source_id = 'published_sources' ORDER BY generation DESC LIMIT 1")).
		WithArgs("tenant-a", "ar_balance", "finance.ar", "legal_entity", "entity-a").
		WillReturnRows(sqlmock.NewRows([]string{"source_revision", "as_of", "freshness", "data"}).AddRow("revision-1", asOf, "stale", string(data)))
	model, err := BuildPublishedSemanticModel("ar_balance", "AR balance", "finance.ar", "ar_balance", "sales.orders/balances", contract.ScopeEntityOwned,
		[]SemanticDimension{{Name: "period", Field: "period", Type: "time", SupportedGranularities: []string{"day", "month"}}},
		[]SemanticMeasure{{Name: "amount_sum", Field: "amount", Aggregation: "sum"}, {Name: "count", Aggregation: "count"}})
	if err != nil {
		t.Fatal("build published model:", err)
	}
	model.AllowedFields = []string{"amount", "id", "period", "assigned_to"}
	catalog := &SemanticCatalog{Models: []SemanticModel{model}}
	engine := &QueryEngine{DB: database, SiteName: "tenant-a", Dialect: kdb.Resolve("mysql"), BusinessScope: scope, ProjectionActorScopes: map[string]ProjectionActorScope{model.Name: {SubjectID: "alice", AssignedFields: []string{"assigned_to"}}}}
	response, err := engine.ResolveSemanticQuery(catalog, AnalyticsQueryRequest{
		Queries: []ModelQuery{{Model: model.Name, Dimensions: []string{"period"}, Measures: []string{"amount_sum", "count"}}},
		Time:    QueryTime{From: "2026-10-01", To: "2026-10-31", Granularity: "month"}, Limit: 10,
	})
	if err != nil {
		t.Fatal("query published projection:", err)
	}
	if response.DataSource != "published_projection" || len(response.Queries) != 1 || response.Queries[0].Freshness != contract.QueryStale || response.Queries[0].AsOf == nil || !response.Queries[0].AsOf.Equal(asOf) {
		t.Fatalf("projection response metadata = %#v", response)
	}
	if len(response.Queries[0].Rows) != 1 || response.Queries[0].Rows[0]["period"] != "2026-10-01" || response.Queries[0].Rows[0]["amount_sum"] != float64(12) || response.Queries[0].Rows[0]["count"] != 1 {
		t.Fatalf("projection aggregate rows = %#v", response.Queries[0].Rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unexpected projection query:", err)
	}
}

func TestQueryEngineRejectsOverLimitBeforeSQL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	engine := &QueryEngine{DB: database, SiteName: "tenant-a", Dialect: kdb.Resolve("mysql")}
	_, err = engine.Resolve(&Metric{Name: "count", DocType: "Cake Order", Type: MetricCount}, QueryRequest{Limit: MaxSemanticResultRows + 1})
	if err == nil {
		t.Fatal("over-limit query was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unexpected SQL query was executed:", err)
	}
}

func TestSemanticLikeFilterTreatsWildcardsAsLiteralText(t *testing.T) {
	clause, args := semanticFilterSQL(kdb.Resolve("mysql"), "dimension", "invoice_date=", SemanticFilter{
		Field: "invoice_date", Operator: "like", Values: []string{"_rate%"},
	})
	if clause != "LOWER(`dimension`) LIKE LOWER(?) ESCAPE '!'" {
		t.Fatalf("unexpected LIKE clause: %q", clause)
	}
	if len(args) != 1 || args[0] != "invoice!_date=%!_rate!%%" {
		t.Fatalf("LIKE values were not escaped literally: %#v", args)
	}
}

func TestSemanticInFilterPrefixesEveryRollupDimensionValue(t *testing.T) {
	clause, args := semanticFilterSQL(kdb.Resolve("mysql"), "dimension", "pipeline_stage=", SemanticFilter{
		Field: "pipeline_stage", Operator: "in", Values: []string{"Lead", "Qualified"},
	})
	if clause != "`dimension` IN (?,?)" {
		t.Fatalf("unexpected IN clause: %q", clause)
	}
	if len(args) != 2 || args[0] != "pipeline_stage=Lead" || args[1] != "pipeline_stage=Qualified" {
		t.Fatalf("filter values were not encoded for rollup dimensions: %#v", args)
	}
}

func TestSemanticRollupInFilterRunsBeforeLimit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT dimension, SUM(value) AS value FROM _kora_analytics_daily WHERE site = ? AND scope_kind = ? AND legal_entity_id = ? AND doctype = ? AND metric = ? AND date >= ? AND date <= ? AND `dimension` IN (?,?) GROUP BY dimension ORDER BY value DESC LIMIT ?")).
		WithArgs("tenant-a", "tenant", "", "Deal", "deal_total_amount_sum_by_pipeline_stage", "2026-01-01", "2026-01-31", "pipeline_stage=Lead", "pipeline_stage=Qualified", 10).
		WillReturnRows(sqlmock.NewRows([]string{"dimension", "value"}).
			AddRow("pipeline_stage=Lead", 1200.0).
			AddRow("pipeline_stage=Qualified", 800.0))
	engine := &QueryEngine{DB: database, SiteName: "tenant-a", Dialect: kdb.Resolve("mysql")}
	result, err := engine.Resolve(&Metric{
		Name: "deal_total_amount_sum_by_pipeline_stage", DocType: "Deal", Type: MetricSum,
		Field: "total_amount", TimeField: "creation", GroupByField: "pipeline_stage",
	}, QueryRequest{
		From: "2026-01-01", To: "2026-01-31", Dimension: "pipeline_stage", Limit: 10,
		Filters: []SemanticFilter{{Field: "pipeline_stage", Operator: "in", Values: []string{"Lead", "Qualified"}}},
	})
	if err != nil {
		t.Fatalf("resolve stage-filtered rollup: %v", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("expected both open stages, got %#v", result.Rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestQueryCacheKeyIncludesAllQueryInputs(t *testing.T) {
	engine := &QueryEngine{SiteName: "tenant-a"}
	base := QueryRequest{Metric: "orders", From: "2026-01-01", To: "2026-01-31", Limit: 20}
	keys := map[string]bool{engine.cacheKey("orders", base): true}
	variants := []QueryRequest{
		{Metric: "orders", From: base.From, To: base.To, Limit: 10},
		{Metric: "orders", From: base.From, To: base.To, Limit: 20, Dimension: "status", Filters: []SemanticFilter{{Field: "status", Operator: "=", Values: []string{"paid"}}}},
		{Metric: "orders", From: base.From, To: base.To, Limit: 20, GroupBy: "month"},
	}
	for _, variant := range variants {
		key := engine.cacheKey("orders", variant)
		if keys[key] {
			t.Fatalf("distinct query shared cache key: %#v", variant)
		}
		keys[key] = true
	}
}

func TestAnalyticsPeriodExpressionIsDialectSpecific(t *testing.T) {
	tests := []struct {
		name, driver, period, want string
	}{
		{"mysql quarter", "mysql", "quarter", "CONCAT(YEAR(`date`), '-Q', QUARTER(`date`))"},
		{"postgres week", "postgres", "week", "TO_CHAR((\"date\"::date - (EXTRACT(ISODOW FROM \"date\")::int - 1)), 'YYYY-MM-DD')"},
		{"libsql month", "libsql", "month", `STRFTIME('%Y-%m', "date") || '-01'`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := analyticsPeriodExpression(kdb.Resolve(test.driver), "date", test.period)
			if err != nil {
				t.Fatal("period expression:", err)
			}
			if got != test.want {
				t.Fatalf("period expression = %q, want %q", got, test.want)
			}
		})
	}
}
