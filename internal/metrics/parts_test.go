package metrics

import (
	"reflect"
	"testing"
)

// parkedMetric reads one metric from the parked erpsell pack, the real definitions of net_sales and average_sale.
func parkedMetric(t *testing.T, name string) Metric {
	t.Helper()
	defs, err := ParseDirectory("../../config/_parked/erpsell/metrics")
	if err != nil {
		t.Fatalf("parse parked metrics: %v", err)
	}
	for _, m := range defs {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("parked pack has no metric %q", name)
	return Metric{}
}

func TestMetricPartsNameWhatTheMetricIsMadeOf(t *testing.T) {
	tests := []struct {
		name   string
		metric Metric
		want   []Part
	}{
		{
			name:   "net sales subtracts refunds",
			metric: parkedMetric(t, "net_sales"),
			want:   []Part{{Metric: "sales", Sign: "+"}, {Metric: "refunds", Sign: "-"}},
		},
		{
			name:   "average sale divides sales by receipts",
			metric: parkedMetric(t, "average_sale"),
			want:   []Part{{Metric: "sales", Role: "numerator"}, {Metric: "receipts", Role: "denominator"}},
		},
		{
			name:   "a simple metric has no parts",
			metric: Metric{Name: "sales", Type: KindSimple, Model: "Sale", Measure: &Measure{Agg: aggSum, Field: "total"}},
			want:   nil,
		},
		{
			name:   "a minus flips the sign of everything on its right",
			metric: Metric{Name: "m", Type: KindDerived, Formula: "a - (b - c)"},
			want:   []Part{{Metric: "a", Sign: "+"}, {Metric: "b", Sign: "-"}, {Metric: "c", Sign: "+"}},
		},
		{
			name:   "a derived division names numerator and denominator",
			metric: Metric{Name: "m", Type: KindDerived, Formula: "sales / receipts"},
			want:   []Part{{Metric: "sales", Role: "numerator"}, {Metric: "receipts", Role: "denominator"}},
		},
		{
			name:   "a number in the formula leaves no parts",
			metric: Metric{Name: "m", Type: KindDerived, Formula: "sales + 1"},
			want:   nil,
		},
		{
			name:   "a product leaves no parts",
			metric: Metric{Name: "m", Type: KindDerived, Formula: "sales * receipts"},
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.metric.Parts(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parts() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
