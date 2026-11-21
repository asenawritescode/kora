package metrics

import (
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

func TestDrillTurnsNumberIntoRecordFilters(t *testing.T) {
	cat := posCatalog(t)
	cat.Metrics["line_amount"] = Metric{Name: "line_amount", Label: "Line amount", Type: KindSimple, Model: "Sale",
		Measure: &Measure{Agg: aggSum, Field: "amount"}}
	nairobi, err := tenanttime.ParseZone("Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	day := tenanttime.Date{Year: 2026, Month: 11, Day: 20}
	plain := tenanttime.Clock{Location: nairobi}
	cutoff := tenanttime.Clock{Location: nairobi, Cutoff: 3 * 60 * 60 * 1e9}
	tests := []struct {
		name    string
		req     DrillRequest
		clock   tenanttime.Clock
		want    []Filter
		wantErr bool
	}{
		{
			name:  "sales drill to paid records in the zone's day",
			req:   DrillRequest{Metric: "sales", From: day, To: day},
			clock: plain,
			want: []Filter{
				{Field: "status", Op: "=", Value: "Paid"},
				{Field: "creation", Op: ">=", Value: "2026-11-19 21:00:00"},
				{Field: "creation", Op: "<", Value: "2026-11-20 21:00:00"},
			},
		},
		{
			name:  "a business-day cutoff moves the window",
			req:   DrillRequest{Metric: "receipts", From: day, To: day},
			clock: cutoff,
			want: []Filter{
				{Field: "status", Op: "=", Value: "Paid"},
				{Field: "creation", Op: ">=", Value: "2026-11-20 00:00:00"},
				{Field: "creation", Op: "<", Value: "2026-11-21 00:00:00"},
			},
		},
		{
			name:  "group values follow the metric filters in name order",
			req:   DrillRequest{Metric: "sales", From: day, To: day, Group: map[string]string{"till_session": "S1", "register": "R1"}},
			clock: plain,
			want: []Filter{
				{Field: "status", Op: "=", Value: "Paid"},
				{Field: "register", Op: "=", Value: "R1"},
				{Field: "till_session", Op: "=", Value: "S1"},
				{Field: "creation", Op: ">=", Value: "2026-11-19 21:00:00"},
				{Field: "creation", Op: "<", Value: "2026-11-20 21:00:00"},
			},
		},
		{name: "ratio does not drill", req: DrillRequest{Metric: "average_sale", From: day, To: day}, clock: plain, wantErr: true},
		{name: "balance does not drill", req: DrillRequest{Metric: "balance_sales", From: day, To: day}, clock: plain, wantErr: true},
		{name: "count distinct does not drill", req: DrillRequest{Metric: "distinct_registers", From: day, To: day}, clock: plain, wantErr: true},
		{name: "line sum does not drill", req: DrillRequest{Metric: "line_amount", From: day, To: day}, clock: plain, wantErr: true},
		{name: "line dimension does not drill", req: DrillRequest{Metric: "sales", From: day, To: day, Group: map[string]string{"item": "I1"}}, clock: plain, wantErr: true},
		{name: "unknown metric", req: DrillRequest{Metric: "nope", From: day, To: day}, clock: plain, wantErr: true},
		{name: "reversed range", req: DrillRequest{Metric: "sales", From: day, To: tenanttime.Date{Year: 2026, Month: 11, Day: 19}}, clock: plain, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cat.Drill(tc.req, tc.clock)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.Doctype != "Sale" || !reflect.DeepEqual(got.Filters, tc.want) {
				t.Fatalf("drill = %s %v, want Sale %v", got.Doctype, got.Filters, tc.want)
			}
		})
	}
}
