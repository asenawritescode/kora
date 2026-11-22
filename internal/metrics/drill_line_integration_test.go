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

// TestDrillLineSumListsItsSales drills the line amount of product P2 by its line dimension. Of the four sales,
// SAL-A and SAL-C hold P2, so the drill lists those two, each with its P2 lines: SAL-C 150, SAL-A 50, total 200.
func TestDrillLineSumListsItsSales(t *testing.T) {
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
			cat, names, err := NewCatalog(nil, dts).WithAdHoc([]AdHoc{{Model: "Sale", Agg: aggSum, Field: "line_total"}})
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: cat}
			req := DrillRequest{Metric: names[0], From: tenanttime.Date{Year: 2026, Month: 11, Day: 18},
				To: tenanttime.Date{Year: 2026, Month: 11, Day: 20}, Group: map[string]string{"items.product": "P2"}}
			page, err := engine.DrillRecords(context.Background(), req, Scope{Joinable: map[string]bool{"Product": true}}, tenanttime.Clock{}, "")
			if err != nil {
				t.Fatalf("drill: %v", err)
			}
			if !page.Total.Equal(decimal.RequireFromString("200")) {
				t.Errorf("total = %s, want 200", page.Total)
			}
			want := []struct{ name, value string }{{"SAL-C", "150"}, {"SAL-A", "50"}}
			if len(page.Records) != len(want) {
				t.Fatalf("records = %d, want %d: %+v", len(page.Records), len(want), page.Records)
			}
			for i, r := range page.Records {
				if r.Name != want[i].name || !r.Value.Equal(decimal.RequireFromString(want[i].value)) {
					t.Errorf("record %d = %s %s, want %s %s", i, r.Name, r.Value, want[i].name, want[i].value)
				}
			}
		})
	}
}
