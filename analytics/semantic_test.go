package analytics

import (
	"strings"
	"testing"
	"time"

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
