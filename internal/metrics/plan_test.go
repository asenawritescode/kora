package metrics

import (
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

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
		Metric{Name: "balance_registers", Label: "Balance", Type: KindBalance, Model: "Sale", Measure: &Measure{Agg: aggCountDistinct, Field: "register"}},
		Metric{Name: "receipts_to_date", Label: "Receipts", Type: KindCumulative, Model: "Sale", Measure: &Measure{Agg: aggCount}},
		Metric{Name: "ratio_of_balance", Label: "Nested", Type: KindRatio, Numerator: "balance_sales", Denominator: "receipts"},
		Metric{Name: "ratio_of_ratio", Label: "Nested", Type: KindRatio, Numerator: "average_sale", Denominator: "receipts"},
		Metric{Name: "net_sales", Label: "Net", Type: KindDerived, Formula: "sales - refunds"},
		Metric{Name: "average_plus_one", Label: "Plus", Type: KindDerived, Formula: "average_sale + 1"},
	)
	return NewCatalog(defs, []*doctype.DocType{sale, other})
}

func TestRatioValuesNullOnZeroDenominator(t *testing.T) {
	p := plan{series: make([]series, 2), metrics: []output{
		{metric: "sales", kind: KindSimple, num: 0, den: -1},
		{metric: "average_sale", kind: KindRatio, num: 0, den: 1},
	}, requested: []int{0, 1}}
	tests := []struct {
		name   string
		bucket []decimal.Decimal
		want   []decimal.NullDecimal
	}{
		{"ratio divides its components", []decimal.Decimal{dec("100"), dec("4")}, []decimal.NullDecimal{{Decimal: dec("100"), Valid: true}, {Decimal: dec("25"), Valid: true}}},
		{"zero denominator is null, sum stays", []decimal.Decimal{dec("100"), dec("0")}, []decimal.NullDecimal{{Decimal: dec("100"), Valid: true}, {}}},
		{"empty bucket: sum is zero, ratio is null", []decimal.Decimal{dec("0"), dec("0")}, []decimal.NullDecimal{{Decimal: dec("0"), Valid: true}, {}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.values(tc.bucket)
			if len(got) != len(tc.want) {
				t.Fatalf("values = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i].Valid != tc.want[i].Valid || (got[i].Valid && !got[i].Decimal.Equal(tc.want[i].Decimal)) {
					t.Errorf("value %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

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
		{name: "derived over simple metrics reads the summary", query: with(func(q *Query) { q.Metrics = []string{"net_sales"} }), summary: true, series: 2, table: "_kora_fact_summary_sale"},
		{name: "derived over a ratio shares its components", query: with(func(q *Query) { q.Metrics = []string{"average_plus_one"} }), summary: true, series: 2},
		{name: "derived with owner scope reads the facts", query: with(func(q *Query) { q.Metrics = []string{"net_sales"}; q.Scope.Owner = "u1" }), summary: false, series: 2},
		{name: "top with one dimension reads the summary", query: with(func(q *Query) { q.Dimensions = []string{"register"}; q.Top = 2 }), summary: true, series: 1},
		{name: "top without a dimension", query: with(func(q *Query) { q.Top = 2 }), invalid: true},
		{name: "top with two dimensions", query: with(func(q *Query) { q.Dimensions = []string{"register", "item"}; q.Top = 2 }), invalid: true},
		{name: "negative top", query: with(func(q *Query) { q.Top = -1 }), invalid: true},
		{name: "unknown compare window", query: with(func(q *Query) { q.Compare = "last_year" }), invalid: true},
		{name: "compare previous period is accepted", query: with(func(q *Query) { q.Compare = ComparePreviousPeriod }), summary: true, series: 1},
		{name: "balance reads the summary and its opening", query: with(func(q *Query) { q.Metrics = []string{"balance_sales"} }), summary: true, series: 1, table: "_kora_fact_summary_sale"},
		{name: "balance with owner scope reads the facts", query: with(func(q *Query) { q.Metrics = []string{"balance_sales"}; q.Scope.Owner = "u1" }), summary: false, series: 1, table: "_kora_fact_sale"},
		{name: "cumulative count reads the summary", query: with(func(q *Query) { q.Metrics = []string{"receipts_to_date"} }), summary: true, series: 1},
		{name: "balance with count_distinct is invalid", query: with(func(q *Query) { q.Metrics = []string{"balance_registers"} }), invalid: true},
		{name: "balance in a ratio is invalid", query: with(func(q *Query) { q.Metrics = []string{"ratio_of_balance"} }), invalid: true},
		{name: "ratio of a ratio is not supported yet", query: with(func(q *Query) { q.Metrics = []string{"ratio_of_ratio"} }), unsupported: true},
		{name: "metrics of two models are not supported yet", query: with(func(q *Query) { q.Metrics = []string{"sales", "stock_qty"} }), unsupported: true},
		{name: "unknown metric", query: with(func(q *Query) { q.Metrics = []string{"nope"} }), invalid: true},
		{name: "undeclared dimension", query: with(func(q *Query) { q.Dimensions = []string{"customer"} }), invalid: true},
		{name: "filter on an unknown field", query: with(func(q *Query) {
			q.Filters = []Filter{{Field: "customer", Op: "=", Value: "x"}}
		}), invalid: true},
		{name: "grain is required", query: with(func(q *Query) { q.Grain = "" }), invalid: true},
		{name: "range must not end before it starts", query: with(func(q *Query) { q.From, q.To = day(22), day(16) }), invalid: true},
		{name: "range grain reads the summary", query: with(func(q *Query) { q.Grain = GrainRange }), summary: true, series: 1, table: "_kora_fact_summary_sale"},
		{name: "range grain with a balance", query: with(func(q *Query) { q.Grain = GrainRange; q.Metrics = []string{"balance_sales"} }), summary: true, series: 1},
		{name: "range grain with owner scope reads the facts", query: with(func(q *Query) { q.Grain = GrainRange; q.Scope.Owner = "u1" }), summary: false, series: 1, table: "_kora_fact_sale"},
		{name: "unknown grain names range too", query: with(func(q *Query) { q.Grain = "year" }), invalid: true},
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

func TestRangeGrainIsOneBucketWithoutCap(t *testing.T) {
	tests := []struct {
		name     string
		from, to tenanttime.Date
		grain    Grain
		buckets  int
		tooLarge bool
	}{
		{"range over two years is one bucket", day(2025, 1, 1), day(2026, 12, 31), GrainRange, 1, false},
		{"range of one day is one bucket", day(2026, 11, 18), day(2026, 11, 18), GrainRange, 1, false},
		{"day grain over the same two years is capped", day(2025, 1, 1), day(2026, 12, 31), GrainDay, 731, true},
		{"month grain over the same two years fits", day(2025, 1, 1), day(2026, 12, 31), GrainMonth, 24, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := Query{Metrics: []string{"sales"}, From: tc.from, To: tc.to, Grain: tc.grain}
			if got := rangeTooLarge(q); got != tc.tooLarge {
				t.Fatalf("rangeTooLarge = %v, want %v", got, tc.tooLarge)
			}
			err := checkShape(q)
			if errors.Is(err, ErrRangeTooLarge) != tc.tooLarge {
				t.Fatalf("checkShape = %v, want too large %v", err, tc.tooLarge)
			}
			if tc.tooLarge {
				return
			}
			if err != nil {
				t.Fatalf("checkShape: %v", err)
			}
			starts := bucketStarts(tc.from, tc.to, tc.grain)
			if len(starts) != tc.buckets {
				t.Fatalf("buckets = %d, want %d", len(starts), tc.buckets)
			}
			if tc.grain == GrainRange && !starts[0].Equal(toTime(tc.from)) {
				t.Fatalf("range bucket starts %s, want its first day %s", starts[0], toTime(tc.from))
			}
		})
	}
}

func TestRangeGrainKeysEveryDayToItsFirstDay(t *testing.T) {
	p := plan{grain: GrainRange, from: day(2026, 11, 18), to: day(2026, 11, 20)}
	for _, d := range []tenanttime.Date{day(2026, 11, 18), day(2026, 11, 19), day(2026, 11, 20)} {
		if got := p.bucketOf(toTime(d)); got != p.from {
			t.Fatalf("bucketOf(%s) = %s, want %s", d, got, p.from)
		}
	}
}
