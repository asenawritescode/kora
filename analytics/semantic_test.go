package analytics

import (
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

func TestAnalyticsQueryValidatesAgainstCatalog(t *testing.T) {
	request := AnalyticsQueryRequest{
		Queries: []ModelQuery{{
			Model:      "cake_orders",
			Measures:   []string{"sales"},
			Dimensions: []string{"order_date", "flavour"},
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
