package analytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func testCatalog() *SemanticCatalog {
	return &SemanticCatalog{Models: []SemanticModel{{
		Name:          "cake_orders",
		SourceDoctype: "Cake Order",
		TimeDimension: "order_date",
		Dimensions: []SemanticDimension{
			{Name: "order_date", Field: "order_date", Type: "time"},
			{Name: "flavour", Field: "flavour", Type: "category"},
		},
		Measures: []SemanticMeasure{
			{Name: "sales", Field: "total_sales", Aggregation: "sum", Format: "currency"},
			{Name: "order_count", Aggregation: "count", Format: "number"},
		},
	}}}
}

func TestBuildSemanticCatalogDoesNotPublishModuleOwnedResources(t *testing.T) {
	catalog := BuildSemanticCatalog([]*doctype.DocType{
		{Name: "Public Record", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
		{Name: "Private Ledger", ModuleOwnerID: "finance.ledger", ModuleAccess: "private", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
		{Name: "Published Balance", ModuleOwnerID: "finance.ledger", ModuleAccess: "published", Fields: []doctype.Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
	})
	if len(catalog.Models) != 1 || catalog.Models[0].SourceDoctype != "Public Record" {
		t.Fatalf("module-owned resources leaked into automatic analytics catalog: %#v", catalog.Models)
	}
}

func TestPublishedSemanticModelHasNoDirectTableSource(t *testing.T) {
	model, err := BuildPublishedSemanticModel("ar_balance", "AR balance", "finance.ar", "finance_ar_balance", "balances", contract.ScopeEntityOwned, []SemanticDimension{{Name: "period", Field: "period", Type: "time"}}, []SemanticMeasure{{Name: "open_amount", Field: "open_amount", Aggregation: "sum"}})
	if err != nil {
		t.Fatal(err)
	}
	model.ProjectionID = "finance_ar_balance"
	if err := model.Validate(); err != nil {
		t.Fatal(err)
	}
	if model.SourceDoctype != "" || model.Relation != nil {
		t.Fatalf("published semantic model has direct table source: %#v", model)
	}
	model.AllowedFields = []string{"period"}
	if err := model.Validate(); err == nil {
		t.Fatal("projection measure outside the declared output contract was accepted")
	}
}

func TestPublishedProjectionModelUsesOnlyPublishedOutputFields(t *testing.T) {
	output := contract.SchemaRef{ID: "ar.balance.output", Version: 1}
	definition := contract.ProjectionDefinition{ID: "ar_balance", OwnerModuleID: "finance.ar", ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"id", "period", "amount"}, Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "balances", OutputSchema: output}}, OutputSchema: output, RebuildStrategy: "consistent_snapshot"}
	model, err := BuildPublishedProjectionModel("ar_balance", "AR balance", definition,
		[]SemanticDimension{{Name: "period", Field: "period", Type: "time", SupportedGranularities: []string{"day", "month"}}},
		[]SemanticMeasure{{Name: "amount_sum", Field: "amount", Aggregation: "sum"}})
	if err != nil {
		t.Fatal("build published projection model:", err)
	}
	hasFactKey := false
	for _, field := range model.AllowedFields {
		hasFactKey = hasFactKey || field == "id"
	}
	if model.SourceDoctype != "" || model.Relation != nil || !hasFactKey {
		t.Fatalf("projection model exposed direct source or dropped dedupe key: %#v", model)
	}
	definition.AllowedFields = []string{"id", "period"}
	if _, err := BuildPublishedProjectionModel("ar_balance", "AR balance", definition,
		[]SemanticDimension{{Name: "period", Field: "period", Type: "time"}},
		[]SemanticMeasure{{Name: "amount_sum", Field: "amount", Aggregation: "sum"}}); err == nil {
		t.Fatal("report model accepted a measure outside the published query field allowlist")
	}
}

func TestPublishedProjectionSnapshotRejectsUndeclaredExportFields(t *testing.T) {
	output := contract.SchemaRef{ID: "ar.balance.output", Version: 1}
	definition := contract.ProjectionDefinition{ID: "ar_balance", OwnerModuleID: "finance.ar", ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"id", "amount"}, Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "balances", OutputSchema: output}}, OutputSchema: output, RebuildStrategy: "consistent_snapshot"}
	snapshot := PublishedProjectionSnapshot{Site: "tenant-a", Definition: definition, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, SourceID: "published_sources", SourceRevision: "r1", Cursor: `{"sales.orders/balances":"c1"}`, AsOf: time.Now().UTC(), Freshness: contract.QueryFresh, Data: json.RawMessage(`[{"id":"invoice-1","amount":12}]`)}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("valid projection snapshot rejected: %v", err)
	}
	snapshot.Data = json.RawMessage(`[{"id":"invoice-1","amount":12,"secret":"private"}]`)
	if err := snapshot.Validate(); err == nil {
		t.Fatal("projection snapshot with an undeclared export field was accepted")
	}
}

func TestSemanticCatalogValidatesModelAndRollupReferences(t *testing.T) {
	catalog := testCatalog()
	catalog.Models[0].Rollups = []RollupDefinition{{
		Name:          "cake_orders_monthly",
		Measures:      []string{"sales", "order_count"},
		Dimensions:    []string{"flavour"},
		TimeDimension: "order_date",
		Granularity:   "month",
	}}

	if err := catalog.Validate(); err != nil {
		t.Fatalf("expected valid catalog: %v", err)
	}
}

func TestSemanticCatalogRejectsUnknownRollupReference(t *testing.T) {
	catalog := testCatalog()
	catalog.Models[0].Rollups = []RollupDefinition{{
		Name:        "broken_rollup",
		Measures:    []string{"missing_measure"},
		Granularity: "month",
	}}

	if err := catalog.Validate(); err == nil {
		t.Fatal("expected unknown measure to be rejected")
	}
}

func TestGenerateDefaultReportsUsesCatalogFields(t *testing.T) {
	catalog := &SemanticCatalog{Models: []SemanticModel{{
		Name: "invoice", Label: "Invoice", TimeDimension: "invoice_date",
		Dimensions: []SemanticDimension{
			{Name: "status", Field: "status", Type: "category"},
			{Name: "invoice_date", Field: "invoice_date", Type: "time"},
		},
		Measures: []SemanticMeasure{
			{Name: "count", Label: "Count", Aggregation: "count", Format: "number"},
			{Name: "amount_sum", Label: "Total Amount", Field: "amount", Aggregation: "sum", Format: "currency"},
		},
	}}}
	reports := GenerateDefaultReports(catalog)
	if len(reports) != 1 || reports[0].Name != "invoice_overview" {
		t.Fatalf("unexpected reports: %#v", reports)
	}
	query := reports[0].Queries[0]
	if len(query.Measures) != 2 || query.Measures[1] != "amount_sum" {
		t.Fatalf("unexpected measures: %#v", query.Measures)
	}
	if len(query.Dimensions) != 1 || query.Dimensions[0] != "invoice_date" {
		t.Fatalf("unexpected dimensions: %#v", query.Dimensions)
	}
	if err := reports[0].Validate(catalog); err != nil {
		t.Fatalf("generated report should validate: %v", err)
	}
}

func TestAnalyticsQueryValidatesAgainstCatalog(t *testing.T) {
	request := AnalyticsQueryRequest{
		Queries: []ModelQuery{{
			Model:      "cake_orders",
			Measures:   []string{"sales"},
			Dimensions: []string{"order_date"},
		}},
		Time: QueryTime{From: "2026-01-01", To: "2026-12-31", Granularity: "month"},
	}

	if err := request.Validate(testCatalog()); err != nil {
		t.Fatalf("expected valid query: %v", err)
	}
}

func TestAnalyticsQueryRejectsUnknownMeasureAndLimit(t *testing.T) {
	request := AnalyticsQueryRequest{
		Queries: []ModelQuery{{Model: "cake_orders", Measures: []string{"profit"}}},
		Limit:   10001,
	}

	if err := request.Validate(testCatalog()); err == nil {
		t.Fatal("expected invalid query to be rejected")
	}
}

func TestAnalyticsQueryEnforcesAdvertisedBudgetsAndStrictDates(t *testing.T) {
	base := AnalyticsQueryRequest{
		Queries: []ModelQuery{{Model: "cake_orders", Measures: []string{"sales"}}},
		Time:    QueryTime{From: "2026-01-01", To: "2026-12-31", Granularity: "month"},
	}
	cases := []struct {
		name   string
		change func(*AnalyticsQueryRequest)
	}{
		{name: "query count", change: func(q *AnalyticsQueryRequest) { q.Queries = make([]ModelQuery, MaxSemanticQueries+1) }},
		{name: "result rows", change: func(q *AnalyticsQueryRequest) { q.Limit = MaxSemanticResultRows + 1 }},
		{name: "invalid date", change: func(q *AnalyticsQueryRequest) { q.Time.From = "2026-02-30" }},
		{name: "excessive date range", change: func(q *AnalyticsQueryRequest) { q.Time = QueryTime{From: "2000-01-01", To: "2026-01-01"} }},
		{name: "multiple dimensions", change: func(q *AnalyticsQueryRequest) { q.Queries[0].Dimensions = []string{"order_date", "flavour"} }},
		{name: "filter count", change: func(q *AnalyticsQueryRequest) {
			for range MaxSemanticFilters + 1 {
				q.Queries[0].Filters = append(q.Queries[0].Filters, SemanticFilter{Field: "flavour", Operator: "=", Values: []string{"vanilla"}})
			}
		}},
		{name: "filter value count", change: func(q *AnalyticsQueryRequest) {
			q.Queries[0].Dimensions = []string{"flavour"}
			values := make([]string, MaxSemanticFilterValues+1)
			q.Queries[0].Filters = []SemanticFilter{{Field: "flavour", Operator: "in", Values: values}}
		}},
		{name: "filter value bytes", change: func(q *AnalyticsQueryRequest) {
			q.Queries[0].Dimensions = []string{"flavour"}
			q.Queries[0].Filters = []SemanticFilter{{Field: "flavour", Operator: "=", Values: []string{strings.Repeat("x", MaxSemanticFilterBytes+1)}}}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Queries = append([]ModelQuery(nil), base.Queries...)
			request.Queries[0].Filters = append([]SemanticFilter(nil), base.Queries[0].Filters...)
			test.change(&request)
			if err := request.Validate(testCatalog()); err == nil {
				t.Fatal("unsafe or over-budget request was accepted")
			}
		})
	}
}

func TestBuildSemanticModelUsesDocTypeFieldMetadata(t *testing.T) {
	model := BuildSemanticModel(&doctype.DocType{
		Name:      "Cake Order",
		SortField: "order_date",
		Fields: []doctype.Field{
			{Fieldname: "order_date", Fieldtype: "Date", Label: "Order date"},
			{Fieldname: "flavour", Fieldtype: "Select", Label: "Flavour"},
			{Fieldname: "total_sales", Fieldtype: "Currency", Label: "Total sales"},
			{Fieldname: "notes", Fieldtype: "Text", Label: "Notes"},
		},
	})

	if model.Name != "cake_order" || model.TimeDimension != "order_date" {
		t.Fatalf("unexpected model identity: %#v", model)
	}
	if len(model.Dimensions) != 2 || len(model.Measures) != 2 {
		t.Fatalf("unexpected catalog fields: dimensions=%d measures=%d", len(model.Dimensions), len(model.Measures))
	}
	if model.Measures[1].Format != "currency" {
		t.Fatalf("expected currency format, got %q", model.Measures[1].Format)
	}
}

func TestBuildSemanticModelTreatsWorkspaceUserFieldsAsCategoricalDimensions(t *testing.T) {
	model := BuildSemanticModel(&doctype.DocType{
		Name:   "Deal",
		Fields: []doctype.Field{{Fieldname: "owner_user", Label: "Deal owner", Fieldtype: "User"}},
	})
	for _, dimension := range model.Dimensions {
		if dimension.Name == "owner_user" && dimension.Type == "category" {
			return
		}
	}
	t.Fatalf("User assignment dimension missing from %#v", model.Dimensions)
}

func TestBuildSemanticModelOmitsHiddenAndPasswordFields(t *testing.T) {
	dt := &doctype.DocType{Name: "Account", Fields: []doctype.Field{
		{Fieldname: "created_on", Fieldtype: "Date"},
		{Fieldname: "private_amount", Fieldtype: "Currency", Hidden: true},
		{Fieldname: "credential", Fieldtype: "Password"},
	}}
	model := BuildSemanticModel(dt)
	for _, dimension := range model.Dimensions {
		if dimension.Field == "private_amount" || dimension.Field == "credential" {
			t.Fatalf("semantic model exposed private dimension: %#v", dimension)
		}
	}
	for _, measure := range model.Measures {
		if measure.Field == "private_amount" || measure.Field == "credential" {
			t.Fatalf("semantic model exposed private measure: %#v", measure)
		}
	}
}

func TestPreferredTimeFieldUsesConfiguredBusinessDate(t *testing.T) {
	dt := &doctype.DocType{
		Name:      "Cake Order",
		SortField: "order_date",
		Fields: []doctype.Field{
			{Fieldname: "order_date", Fieldtype: "Date"},
			{Fieldname: "total_sales", Fieldtype: "Currency"},
		},
	}

	if got := preferredTimeField(dt); got != "order_date" {
		t.Fatalf("expected order_date, got %q", got)
	}
	metric := GenerateMetrics(dt)[0]
	if metric.TimeField != "order_date" {
		t.Fatalf("expected count metric to use order_date, got %q", metric.TimeField)
	}
}

func TestEventDateNormalizesBusinessDateAndFallsBack(t *testing.T) {
	event := ChangeEvent{
		Timestamp: time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC),
		Data:      map[string]any{"order_date": "2026-08-31"},
	}
	if got := eventDate(event, "order_date"); got != "2026-08-31" {
		t.Fatalf("expected business date, got %q", got)
	}
	if got := eventDate(event, "missing_date"); got != "2026-09-05" {
		t.Fatalf("expected timestamp fallback, got %q", got)
	}
}

func TestSemanticFilterMatchesRollupDimensionValues(t *testing.T) {
	rows := []map[string]any{
		{"dimension": "status=Active", "value": 4},
		{"dimension": "status=Closed", "value": 2},
	}
	filtered := filterSemanticRows(rows, "status", []SemanticFilter{{Field: "status", Operator: "=", Values: []string{"Active"}}})
	if len(filtered) != 1 || filtered[0]["dimension"] != "status=Active" {
		t.Fatalf("unexpected filtered rows: %#v", filtered)
	}
}

func TestMetricForSemanticMeasureMapsCategoryCounts(t *testing.T) {
	model := testCatalog().Models[0]
	metric, err := metricForSemanticMeasure(&model, "order_count", []string{"flavour"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metric.Name != "cake_orders_count_by_flavour" || metric.Type != MetricCountByField {
		t.Fatalf("unexpected category metric: %#v", metric)
	}
}
