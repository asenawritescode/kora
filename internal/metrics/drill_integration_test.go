//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// TestDrillMatchesQueryAcrossDialects checks that each day's number and its drill agree on the real
// POS sales: the drill's list filters, run against the records table, count and sum what the query
// reported for that day.
func TestDrillMatchesQueryAcrossDialects(t *testing.T) {
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
			engine := seed(t, database.DB, dialect)
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			// The seed consumes sequences 1 to 7; the newest applied one is the last sale's.
			seq, err := engine.SourceSequence(context.Background(), []*doctype.DocType{engine.Catalog.Models["Sale"]})
			if err != nil || seq != int64(len(seedSales)) {
				t.Fatalf("source sequence = %d, %v; want %d", seq, err, len(seedSales))
			}
			for _, d := range []int{18, 19, 20} {
				day := tenanttime.Date{Year: 2026, Month: 11, Day: d}
				res, err := engine.Query(context.Background(), Query{Metrics: []string{"sales", "receipts"}, From: day, To: day, Grain: GrainDay})
				if err != nil {
					t.Fatalf("query %s: %v", day, err)
				}
				want := res.Rows[0].Values
				for i, metric := range []string{"sales", "receipts"} {
					drill, err := engine.Catalog.Drill(DrillRequest{Metric: metric, From: day, To: day}, clock)
					if err != nil {
						t.Fatalf("drill %s %s: %v", metric, day, err)
					}
					got := runDrill(t, database.DB, dialect, drill, metric == "sales")
					if !got.Equal(want[i].Decimal) {
						t.Fatalf("%s on %s: drill sums %s, query says %s", metric, day, got, want[i].Decimal)
					}
				}
			}
		})
	}
}

// runDrill counts the records a drill lists, or sums their total when sum is set.
func runDrill(t *testing.T, conn *sql.DB, dialect db.Dialect, drill Drill, sum bool) decimal.Decimal {
	t.Helper()
	var where []string
	var args []any
	for _, f := range drill.Filters {
		where = append(where, dialect.QuoteIdent(f.Field)+" "+f.Op+" ?")
		args = append(args, f.Value)
	}
	value := "COUNT(*)"
	if sum {
		value = "COALESCE(SUM(" + dialect.QuoteIdent("total") + "), 0)"
	}
	stmt := db.Rebind(dialect, fmt.Sprintf("SELECT %s FROM %s WHERE %s", value, dialect.QuoteIdent("tab"+drill.Doctype), strings.Join(where, " AND ")))
	var got decimal.Decimal
	if err := conn.QueryRowContext(context.Background(), stmt, args...).Scan(&got); err != nil {
		t.Fatalf("run drill %v: %v", drill.Filters, err)
	}
	return got
}

// mustNairobi is the zone the seeded sales are placed in.
func mustNairobi(t *testing.T) *time.Location {
	t.Helper()
	loc, err := tenanttime.ParseZone("Africa/Nairobi")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
