//go:build integration

package metrics

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestLineRowWithEmptyIDIsNotCountedAsHeader stores a sale's line under an empty line id, the state an older
// build left behind. A count of sales reads header rows only, so the sale counts once per day, and a count of
// line rows is not a count of sales.
func TestLineRowWithEmptyIDIsNotCountedAsHeader(t *testing.T) {
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
			var sale *doctype.DocType
			for _, dt := range dts {
				if dt.Name == "Sale" {
					sale = dt
				}
			}
			// SAL-B has one line. Give that line an empty id: the header row is then the only header.
			update := fmt.Sprintf("UPDATE %s SET line_id = '' WHERE record_id = 'SAL-B'", dialect.QuoteIdent(sale.Facts.LineTableName(sale)))
			if _, err := database.DB.ExecContext(context.Background(), update); err != nil {
				t.Fatalf("stray line row: %v", err)
			}
			cat, names, err := NewCatalog(nil, dts).WithAdHoc([]AdHoc{{Model: "Sale", Agg: aggCount, Filters: []Filter{{Field: "register", Op: "=", Value: "R1"}}}})
			if err != nil {
				t.Fatal(err)
			}
			// A filter on a header field that is not the one summary dimension reads the fact tables, not the summary.
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: cat}
			res, err := engine.Query(context.Background(), Query{Metrics: []string{names[0]},
				From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 19},
				Grain: GrainDay})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			// Four paid sales, two on 18 November and one on 19 November; the stray line row adds nothing.
			want := []string{"2", "1"}
			if len(res.Rows) != len(want) {
				t.Fatalf("rows = %d, want %d", len(res.Rows), len(want))
			}
			for i, row := range res.Rows {
				if v := row.Values[0]; !v.Valid || v.Decimal.String() != want[i] {
					t.Errorf("day %d count = %v, want %s", i, v, want[i])
				}
			}
		})
	}
}
