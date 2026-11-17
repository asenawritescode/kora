package analytics

import (
	"reflect"
	"testing"
)

func floatPtr(value float64) *float64 { return &value }

func TestParseFormulaEvaluatesOrNulls(t *testing.T) {
	env := map[string]*float64{
		"sales_total": floatPtr(300), "sales_count": floatPtr(4),
		"zero": floatPtr(0), "missing_value": nil,
		"sales.total": floatPtr(10), "expenses.total": floatPtr(4),
	}
	cases := []struct {
		name    string
		formula string
		want    *float64
		wantErr bool
	}{
		{name: "average", formula: "sales_total / sales_count", want: floatPtr(75)},
		{name: "precedence", formula: "1 + 2 * 3", want: floatPtr(7)},
		{name: "parentheses", formula: "(1 + 2) * 3", want: floatPtr(9)},
		{name: "unary minus is unsupported", formula: "-sales_count + 10", wantErr: true},
		{name: "qualified refs", formula: "sales.total - expenses.total", want: floatPtr(6)},
		{name: "divide by zero is null", formula: "sales_total / zero", want: nil},
		{name: "null operand is null", formula: "sales_total + missing_value", want: nil},
		{name: "unknown ref evaluates null", formula: "not_in_env * 2", want: nil},
		{name: "unsupported function", formula: "if(sales_total > 0, 1, 0)", wantErr: true},
		{name: "dangling operator", formula: "sales_total /", wantErr: true},
		{name: "unclosed paren", formula: "(sales_total + 1", wantErr: true},
		{name: "trailing token", formula: "sales_total sales_count", wantErr: true},
		{name: "empty", formula: "  ", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parseFormula(tc.formula)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFormula(%q) succeeded, want error", tc.formula)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFormula(%q): %v", tc.formula, err)
			}
			got := root.eval(env)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("eval(%q) = %v, want %v", tc.formula, got, tc.want)
			}
		})
	}
}

func TestApplyDerivedMetricsRowsTotalsAndPreviousWindow(t *testing.T) {
	request := SemanticRequest{
		Queries: []ModelQuery{{ID: "sales", Model: "order", Measures: []string{"total", "count"}, Dimensions: []string{"order_date"}}},
		Derived: []DerivedMetric{
			{Name: "avg_sale", Label: "Average sale", Formula: "sales.total / sales.count"},
			{Name: "cross", Label: "Cross", Formula: "sales.total - expenses.total"},
			{Name: "unsupported", Label: "Unsupported", Formula: "if(total > 0, 1, 0)"},
		},
	}
	comparison := &SemanticComparison{
		Totals: SemanticTotals{
			Current:  map[string]*float64{"total": floatPtr(300), "count": floatPtr(4)},
			Previous: map[string]*float64{"total": floatPtr(100), "count": floatPtr(0)},
			Delta:    map[string]SemanticDelta{},
		},
		PreviousRows: []map[string]any{{"order_date": "2026-01-01", "total": 100.0, "count": 0.0}},
	}
	response := &SemanticQueryResponse{Queries: []SemanticQueryResult{{
		Model:      "order",
		Columns:    []string{"order_date", "total", "count"},
		Rows:       []map[string]any{{"order_date": "2026-01-01", "total": 100.0, "count": 2.0}, {"order_date": "2026-01-02", "total": 200.0, "count": 0.0}},
		Comparison: comparison,
	}}}

	applyDerivedMetrics(response, request)

	result := response.Queries[0]
	if want := []string{"order_date", "total", "count", "avg_sale"}; !reflect.DeepEqual(result.Columns, want) {
		t.Fatalf("columns = %v, want %v", result.Columns, want)
	}
	if got := result.Rows[0]["avg_sale"]; got != 50.0 {
		t.Fatalf("row 1 avg_sale = %v, want 50", got)
	}
	if got := result.Rows[1]["avg_sale"]; got != nil {
		t.Fatalf("row 2 avg_sale = %v, want null (divide by zero)", got)
	}
	if got := comparison.Totals.Current["avg_sale"]; got == nil || *got != 75 {
		t.Fatalf("total avg_sale = %v, want 75", got)
	}
	if got := comparison.Totals.Previous["avg_sale"]; got != nil {
		t.Fatalf("previous total avg_sale = %v, want null", got)
	}
	if delta := comparison.Totals.Delta["avg_sale"]; delta.Absolute != nil || delta.Percent != nil {
		t.Fatalf("delta = %+v, want null when previous is null", delta)
	}
	if got := comparison.PreviousRows[0]["avg_sale"]; got != nil {
		t.Fatalf("previous row avg_sale = %v, want null", got)
	}
	if _, ok := result.Rows[0]["cross"]; ok {
		t.Fatal("cross-query formula evaluated for a single query")
	}
	if _, ok := result.Rows[0]["unsupported"]; ok {
		t.Fatal("unsupported formula produced a column")
	}
}

func TestReportApplyToCarriesQueryIDsAndDerived(t *testing.T) {
	report := ReportDefinition{
		Queries: []ReportQuery{{ID: "sales", Model: "order", Measures: []string{"total"}}},
		Derived: []DerivedMetric{{Name: "double", Label: "Double", Formula: "sales.total * 2"}},
	}
	request := SemanticRequest{Derived: []DerivedMetric{{Name: "client", Label: "Client", Formula: "total"}}}
	report.ApplyTo(&request)
	if want := []ModelQuery{{ID: "sales", Model: "order", Measures: []string{"total"}}}; !reflect.DeepEqual(request.Queries, want) {
		t.Fatalf("queries = %+v, want %+v", request.Queries, want)
	}
	if want := report.Derived; !reflect.DeepEqual(request.Derived, want) {
		t.Fatalf("derived = %+v, want the report's own %+v", request.Derived, want)
	}
}
