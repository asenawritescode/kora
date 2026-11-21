package metrics

import (
	"encoding/base64"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
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

// TestDrillDateFieldFiltersByDay checks the two kinds of time field: a Date field is bounded by the
// days themselves, inclusive, and a Datetime field such as creation by the zone's instants.
func TestDrillDateFieldFiltersByDay(t *testing.T) {
	cat := posCatalog(t)
	day := &doctype.DocType{Name: "Day", Fields: []doctype.Field{{Fieldname: "sale_date", Fieldtype: "Date"}, {Fieldname: "status", Fieldtype: "Data"}, {Fieldname: "total", Fieldtype: "Currency"}},
		Facts: &doctype.FactModel{Time: "sale_date", StatusField: "status", Measures: []doctype.FactMeasure{{Field: "total"}}}}
	cat.Models["Day"] = day
	cat.Metrics["day_sales"] = Metric{Name: "day_sales", Label: "Day sales", Type: KindSimple, Model: "Day",
		Measure: &Measure{Agg: aggSum, Field: "total"}, Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}}
	nairobi := tenanttime.Clock{Location: mustLoadZone(t, "Africa/Nairobi")}
	from, to := tenanttime.Date{Year: 2026, Month: 11, Day: 19}, tenanttime.Date{Year: 2026, Month: 11, Day: 20}
	got, err := cat.Drill(DrillRequest{Metric: "day_sales", From: from, To: to}, nairobi)
	if err != nil {
		t.Fatal(err)
	}
	want := []Filter{
		{Field: "status", Op: "=", Value: "Paid"},
		{Field: "sale_date", Op: ">=", Value: "2026-11-19"},
		{Field: "sale_date", Op: "<=", Value: "2026-11-20"},
	}
	if got.Doctype != "Day" || !reflect.DeepEqual(got.Filters, want) {
		t.Fatalf("drill = %s %v, want Day %v", got.Doctype, got.Filters, want)
	}
}

// mustLoadZone loads an IANA zone for a test.
func mustLoadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := tenanttime.ParseZone(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestDrillListOpenableOnlyForHeaderGroups checks that a group through a line or a path still drills to
// records, but leaves the record list unopenable, because its list filters cannot name the group.
func TestDrillListOpenableOnlyForHeaderGroups(t *testing.T) {
	order := &doctype.DocType{Name: "Order", Facts: &doctype.FactModel{Time: "creation", StatusField: "status", Lines: "items",
		Dimensions: []doctype.FactDimension{{Field: "register"}, {Field: "items.product"}},
		Measures:   []doctype.FactMeasure{{Field: "total"}}}}
	cat := NewCatalog([]Metric{{Name: "orders", Label: "Orders", Type: KindSimple, Model: "Order", Measure: &Measure{Agg: aggCount}}},
		[]*doctype.DocType{order})
	nairobi := tenanttime.Clock{Location: mustLoadZone(t, "Africa/Nairobi")}
	day := tenanttime.Date{Year: 2026, Month: 11, Day: 20}
	timeWant := []Filter{{Field: "creation", Op: ">=", Value: "2026-11-19 21:00:00"}, {Field: "creation", Op: "<", Value: "2026-11-20 21:00:00"}}
	tests := []struct {
		name        string
		group       map[string]string
		wantOpen    bool
		wantFilters []Filter
		wantErr     bool
	}{
		{name: "no group is openable", wantOpen: true, wantFilters: timeWant},
		{name: "header group is openable", group: map[string]string{"register": "R1"}, wantOpen: true,
			wantFilters: append([]Filter{{Field: "register", Op: "=", Value: "R1"}}, timeWant...)},
		{name: "line group is not openable", group: map[string]string{"items.product": "P1"}, wantOpen: false, wantFilters: timeWant},
		{name: "line group keeps the header filters", group: map[string]string{"items.product": "P1", "register": "R1"},
			wantOpen: false, wantFilters: append([]Filter{{Field: "register", Op: "=", Value: "R1"}}, timeWant...)},
		{name: "a line dimension the model lacks is refused", group: map[string]string{"items.nope": "x"}, wantErr: true},
		{name: "a path the model has no link for is refused", group: map[string]string{"supplier.town": "x"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cat.Drill(DrillRequest{Metric: "orders", From: day, To: day, Group: tc.group}, nairobi)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.ListOpenable != tc.wantOpen || !reflect.DeepEqual(got.Filters, tc.wantFilters) {
				t.Fatalf("drill open %v filters %v, want open %v filters %v", got.ListOpenable, got.Filters, tc.wantOpen, tc.wantFilters)
			}
		})
	}
}

// TestDrillCursorRoundTrips checks that a cursor reads back the position it was written from, and that
// anything else is refused as a bad cursor.
func TestDrillCursorRoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 20, 9, 30, 15, 123000000, time.UTC)
	cursor := encodeCursor(at, "SAL-7")
	got, err := decodeCursor(cursor)
	if err != nil || got == nil || !got.at.Equal(at) || got.name != "SAL-7" {
		t.Fatalf("decode(%q) = %+v, %v; want %s SAL-7", cursor, got, err, at)
	}
	if first, err := decodeCursor(""); first != nil || err != nil {
		t.Fatalf("empty cursor = %+v, %v; want the first page", first, err)
	}
	raw := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	tests := []struct{ name, cursor string }{
		{"not base64", "!!!"},
		{"no separator", raw("2026-11-20T09:30:15Z")},
		{"bad time", raw("yesterday|SAL-7")},
		{"no record", raw("2026-11-20T09:30:15Z|")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeCursor(tc.cursor); !errors.Is(err, ErrBadCursor) {
				t.Fatalf("err = %v, want ErrBadCursor", err)
			}
		})
	}
}
