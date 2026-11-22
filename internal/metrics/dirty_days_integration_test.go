//go:build integration

package metrics

import (
	"context"
	"os"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestDirtyDaysReadTheFactRows writes sales through the fact builder and queries before any summary is
// recomputed: every day is dirty, so the header total and count must come from the fact rows, not from the
// empty summary. Expected per day: 18 Nov 200 (two sales), 19 Nov 150 (one), 20 Nov 100 (one); 4 sales in all.
func TestDirtyDaysReadTheFactRows(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			dts := erpsellDoctypes(t)
			seedSaleLines(t, database.DB, dialect)
			writeSaleLines(t, database.DB, dialect, dts)
			cat, names, err := NewCatalog(nil, dts).WithAdHoc([]AdHoc{{Model: "Sale", Agg: aggSum, Field: "total"}, {Model: "Sale", Agg: aggCount}})
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: cat}
			res, err := engine.Query(context.Background(), Query{Metrics: names, Grain: GrainDay,
				From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 20}})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			want := []struct{ day, sum, count string }{{"18", "200", "2"}, {"19", "150", "1"}, {"20", "100", "1"}}
			if len(res.Rows) != len(want) {
				t.Fatalf("rows = %d, want %d", len(res.Rows), len(want))
			}
			for i, row := range res.Rows {
				w := want[i]
				if got := row.Period.String()[8:]; got != w.day {
					t.Errorf("row %d day = %s, want %s", i, got, w.day)
				}
				if sum := row.Values[0]; !sum.Valid || !sum.Decimal.Equal(decimal.RequireFromString(w.sum)) {
					t.Errorf("row %d sum = %v, want %s", i, sum, w.sum)
				}
				if count := row.Values[1]; !count.Valid || !count.Decimal.Equal(decimal.RequireFromString(w.count)) {
					t.Errorf("row %d count = %v, want %s", i, count, w.count)
				}
			}
		})
	}
}
