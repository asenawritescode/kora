package metrics

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/tenanttime"
)

func TestClosureListsComponentsOnce(t *testing.T) {
	cat := posCatalog(t)
	tests := []struct {
		name    string
		names   []string
		want    []string
		wantErr bool
	}{
		{"simple metric stands alone", []string{"sales"}, []string{"sales"}, false},
		{"ratio reads its components", []string{"average_sale"}, []string{"average_sale", "receipts", "sales"}, false},
		{"overlapping roots list each metric once", []string{"sales", "average_sale"}, []string{"average_sale", "receipts", "sales"}, false},
		{"undefined name is an error", []string{"nope"}, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cat.Closure(tc.names)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			var names []string
			for _, m := range got {
				names = append(names, m.Name)
			}
			if !slices.Equal(names, tc.want) {
				t.Fatalf("closure = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestModelsOnlyFactMetrics(t *testing.T) {
	cat := posCatalog(t)
	ms, err := cat.Closure([]string{"stock_qty", "average_sale", "sales"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Models(ms); !slices.Equal(got, []string{"Sale", "Stock"}) {
		t.Fatalf("models = %v, want [Sale Stock]", got)
	}
}

func TestCheckRefusesRangesOverBucketCap(t *testing.T) {
	cat := posCatalog(t)
	day := func(y int, m, d int) tenanttime.Date { return tenanttime.Date{Year: y, Month: time.Month(m), Day: d} }
	tests := []struct {
		name     string
		from, to tenanttime.Date
		grain    Grain
		tooLarge bool
	}{
		{"366 days fit", day(2026, 1, 1), day(2026, 12, 31), GrainDay, false},
		{"367 days do not", day(2026, 1, 1), day(2027, 1, 2), GrainDay, true},
		{"157 weeks fit", day(2023, 1, 2), day(2026, 1, 1), GrainWeek, false},
		{"158 weeks do not", day(2023, 1, 2), day(2026, 1, 8), GrainWeek, true},
		{"120 months fit", day(2016, 1, 1), day(2025, 12, 31), GrainMonth, false},
		{"121 months do not", day(2016, 1, 1), day(2026, 1, 1), GrainMonth, true},
		{"huge range stops early", day(1, 1, 1), day(9999, 12, 31), GrainDay, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cat.Check(Query{Metrics: []string{"sales"}, From: tc.from, To: tc.to, Grain: tc.grain})
			if got := errors.Is(err, ErrRangeTooLarge); got != tc.tooLarge {
				t.Fatalf("err = %v, too large = %v, want %v", err, got, tc.tooLarge)
			}
		})
	}
}
