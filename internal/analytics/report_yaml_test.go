package analytics

import "testing"

func TestParseReportYAML(t *testing.T) {
	report, err := ParseReportYAML([]byte(`
name: cake_profit
label: Cake profit and loss
queries:
  - id: sales
    model: cake_order
    measures: [total_sales_sum]
visuals:
  - id: sales_trend
    type: area
    source: sales
`), "cake_profit.yaml")
	if err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if report.Name != "cake_profit" || report.Visuals[0].Source != "sales" {
		t.Fatalf("unexpected report: %#v", report)
	}
}

func TestReportValidationRejectsUnknownVisualQuery(t *testing.T) {
	err := (&ReportDefinition{
		Name: "example", Label: "Example",
		Queries: []ReportQuery{{ID: "sales", Model: "cake_order", Measures: []string{"count"}}},
		Visuals: []ReportVisual{{ID: "chart", Type: "line", Source: "missing"}},
	}).Validate(nil)
	if err == nil {
		t.Fatal("expected unknown visual source to fail")
	}
}

func TestAnalyticsQueryValidationRequiresValidTimeRange(t *testing.T) {
	err := (&AnalyticsQueryRequest{Queries: []ModelQuery{{Model: "example", Measures: []string{"count"}}}, Time: QueryTime{From: "2026-02-01", To: "2026-01-01", Granularity: "month"}}).Validate(&SemanticCatalog{Models: []SemanticModel{{Name: "example", SourceDoctype: "Example", TimeDimension: "creation", Dimensions: []SemanticDimension{{Name: "creation", Field: "creation", Type: "time"}}, Measures: []SemanticMeasure{{Name: "count", Aggregation: "count"}}}}})
	if err == nil {
		t.Fatal("expected invalid date range to fail")
	}
}
