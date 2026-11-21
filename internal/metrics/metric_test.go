package metrics

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func posDoctypes(t *testing.T) []*doctype.DocType {
	t.Helper()
	dts, err := doctype.ParseConfigTree("../../config/pos")
	if err != nil {
		t.Fatal(err)
	}
	return dts
}

func paidSales(name string, measure Measure) Metric {
	return Metric{Name: name, Label: name, Type: KindSimple, Model: "Sale", Measure: &measure,
		Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}}
}

func TestValidateRules(t *testing.T) {
	sales := paidSales("sales", Measure{Agg: "sum", Field: "total"})
	receipts := Metric{Name: "receipts", Label: "Receipts", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}}
	ratio := Metric{Name: "avg", Label: "Avg", Type: KindRatio, Numerator: "sales", Denominator: "receipts"}
	tests := []struct {
		name    string
		defs    []Metric
		wantErr string
	}{
		{"valid simple and ratio", []Metric{sales, receipts, ratio}, ""},
		{"valid derived", []Metric{sales, receipts, {Name: "d", Label: "D", Type: KindDerived, Formula: "sales * 2 - receipts"}}, ""},
		{"unknown model", []Metric{{Name: "x", Label: "X", Type: KindSimple, Model: "Nope", Measure: &Measure{Agg: "count"}}}, "not a record type with facts"},
		{"sum of a non-measure", []Metric{paidSales("x", Measure{Agg: "sum", Field: "status"})}, "not a facts measure"},
		{"count takes no field", []Metric{paidSales("x", Measure{Agg: "count", Field: "total"})}, "count takes no field"},
		{"count_distinct of a non-dimension", []Metric{paidSales("x", Measure{Agg: "count_distinct", Field: "total"})}, "not a facts dimension"},
		{"unknown agg", []Metric{paidSales("x", Measure{Agg: "avg", Field: "total"})}, "agg \"avg\""},
		{"filter on unknown field", []Metric{{Name: "x", Label: "X", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}, Filters: []Filter{{Field: "customer", Op: "=", Value: "a"}}}}, "neither the status field"},
		{"in needs a list", []Metric{{Name: "x", Label: "X", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}, Filters: []Filter{{Field: "status", Op: "in", Value: "Paid"}}}}, "non-empty list"},
		{"ratio of a missing metric", []Metric{sales, {Name: "r", Label: "R", Type: KindRatio, Numerator: "sales", Denominator: "missing"}}, "unknown metric \"missing\""},
		{"derived formula of a missing metric", []Metric{sales, {Name: "d", Label: "D", Type: KindDerived, Formula: "sales + ghost"}}, "unknown metric \"ghost\""},
		{"derived formula with a syntax error", []Metric{sales, {Name: "d", Label: "D", Type: KindDerived, Formula: "sales +"}}, "ends early"},
		{"derived formula with unsupported text", []Metric{sales, {Name: "d", Label: "D", Type: KindDerived, Formula: "sales ^ 2"}}, "unsupported text"},
		{"derived cycle", []Metric{{Name: "a", Label: "A", Type: KindDerived, Formula: "b"}, {Name: "b", Label: "B", Type: KindDerived, Formula: "a"}}, "part of a cycle"},
		{"ratio of itself", []Metric{{Name: "a", Label: "A", Type: KindRatio, Numerator: "a", Denominator: "a"}}, "part of a cycle"},
		{"duplicate name", []Metric{sales, sales}, "declared more than once"},
		{"ratio takes no model", []Metric{sales, receipts, {Name: "avg", Label: "A", Type: KindRatio, Model: "Sale", Numerator: "sales", Denominator: "receipts"}}, "only numerator and denominator"},
		{"bad good", []Metric{{Name: "x", Label: "X", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}, Good: "sideways"}}, "good \"sideways\""},
		{"bad name", []Metric{{Name: "Sales", Label: "S", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}}}, "snake_case"},
		{"missing label", []Metric{{Name: "sales", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: "count"}}}, "label is required"},
		{"unknown type", []Metric{{Name: "x", Label: "X", Type: "stock"}}, "must be simple"},
	}
	dts := posDoctypes(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.defs, dts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v; want containing %q", err, tc.wantErr)
			}
		})
	}
}
