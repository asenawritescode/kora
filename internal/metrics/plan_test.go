package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// posCatalog is the POS metric pack over the real Sale doctype, plus two synthetic metrics the
// planner must refuse or route, which the pack does not declare.
func posCatalog(t *testing.T) Catalog {
	t.Helper()
	defs, err := ParseDirectory("../../config/pos/metrics")
	if err != nil {
		t.Fatalf("parse pos metrics: %v", err)
	}
	sale, err := doctype.ParseFile("../../config/pos/doctypes/sale.yaml")
	if err != nil {
		t.Fatalf("parse sale: %v", err)
	}
	other := &doctype.DocType{Name: "Stock", Facts: &doctype.FactModel{Time: "creation", Measures: []doctype.FactMeasure{{Field: "qty"}}}}
	defs = append(defs,
		Metric{Name: "distinct_registers", Label: "Registers", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggCountDistinct, Field: "register"}},
		Metric{Name: "stock_qty", Label: "Stock", Type: KindSimple, Model: "Stock", Measure: &Measure{Agg: aggSum, Field: "qty"}},
		Metric{Name: "balance_sales", Label: "Balance", Type: KindBalance, Model: "Sale", Measure: &Measure{Agg: aggSum, Field: "total"}},
		Metric{Name: "ratio_of_ratio", Label: "Nested", Type: KindRatio, Numerator: "average_sale", Denominator: "receipts"},
	)
	return NewCatalog(defs, []*doctype.DocType{sale, other})
}

func TestPlanQueryRoutesAndRefuses(t *testing.T) {
	cat := posCatalog(t)
	day := func(d int) tenanttime.Date { return tenanttime.Date{Year: 2026, Month: 11, Day: d} }
	base := Query{Metrics: []string{"sales"}, From: day(16), To: day(22), Grain: GrainDay}
	with := func(f func(*Query)) Query {
		q := base
		q.Metrics = append([]string(nil), base.Metrics...)
		f(&q)
		return q
	}
	tests := []struct {
		name        string
		query       Query
		summary     bool
		series      int
		table       string
		unsupported bool
		invalid     bool
	}{
		{name: "simple total reads the summary", query: base, summary: true, series: 1, table: "_kora_fact_summary_sale"},
		{name: "ratio shares its components", query: with(func(q *Query) { q.Metrics = []string{"average_sale"} }), summary: true, series: 2, table: "_kora_fact_summary_sale"},
		{name: "two metrics share a series", query: with(func(q *Query) { q.Metrics = []string{"sales", "average_sale"} }), summary: true, series: 2},
		{name: "one declared dimension reads the summary", query: with(func(q *Query) { q.Dimensions = []string{"register"} }), summary: true, series: 1},
		{name: "entity scope reads the summary", query: with(func(q *Query) { q.Scope.LegalEntity = "E1" }), summary: true, series: 1},
		{name: "owner scope reads the facts", query: with(func(q *Query) { q.Scope.Owner = "u1" }), summary: false, series: 1, table: "_kora_fact_sale"},
		{name: "two dimensions read the facts", query: with(func(q *Query) { q.Dimensions = []string{"register", "item"} }), summary: false, series: 1},
		{name: "filter on an ungrouped dimension reads the facts", query: with(func(q *Query) {
			q.Filters = []Filter{{Field: "register", Op: "=", Value: "R1"}}
		}), summary: false, series: 1},
		{name: "filter on the grouped dimension reads the summary", query: with(func(q *Query) {
			q.Dimensions = []string{"register"}
			q.Filters = []Filter{{Field: "register", Op: "in", Value: []any{"R1", "R2"}}}
		}), summary: true, series: 1},
		{name: "count_distinct reads the facts", query: with(func(q *Query) { q.Metrics = []string{"distinct_registers"} }), summary: false, series: 1},
		{name: "balance metric is not supported yet", query: with(func(q *Query) { q.Metrics = []string{"balance_sales"} }), unsupported: true},
		{name: "ratio of a ratio is not supported yet", query: with(func(q *Query) { q.Metrics = []string{"ratio_of_ratio"} }), unsupported: true},
		{name: "metrics of two models are not supported yet", query: with(func(q *Query) { q.Metrics = []string{"sales", "stock_qty"} }), unsupported: true},
		{name: "unknown metric", query: with(func(q *Query) { q.Metrics = []string{"nope"} }), invalid: true},
		{name: "undeclared dimension", query: with(func(q *Query) { q.Dimensions = []string{"customer"} }), invalid: true},
		{name: "filter on an unknown field", query: with(func(q *Query) {
			q.Filters = []Filter{{Field: "customer", Op: "=", Value: "x"}}
		}), invalid: true},
		{name: "grain is required", query: with(func(q *Query) { q.Grain = "" }), invalid: true},
		{name: "range must not end before it starts", query: with(func(q *Query) { q.From, q.To = day(22), day(16) }), invalid: true},
		{name: "no metric", query: with(func(q *Query) { q.Metrics = nil }), invalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planQuery(cat, tc.query)
			switch {
			case tc.unsupported:
				if !errors.Is(err, ErrNotSupported) {
					t.Fatalf("err = %v, want ErrNotSupported", err)
				}
				return
			case tc.invalid:
				if err == nil || errors.Is(err, ErrNotSupported) {
					t.Fatalf("err = %v, want a validation error", err)
				}
				return
			case err != nil:
				t.Fatalf("planQuery: %v", err)
			}
			if p.summary != tc.summary {
				t.Errorf("summary = %v, want %v", p.summary, tc.summary)
			}
			if len(p.series) != tc.series {
				t.Errorf("series = %d, want %d", len(p.series), tc.series)
			}
			if tc.table != "" {
				stmt, _ := p.build(db.MySQL())
				if !strings.Contains(stmt, "`"+tc.table+"`") {
					t.Errorf("statement does not read %s: %s", tc.table, stmt)
				}
			}
		})
	}
}
