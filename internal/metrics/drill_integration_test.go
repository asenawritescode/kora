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

// TestDrillDateFieldMatchesRecordsAcrossDialects checks a Date time field: the drill's inclusive day
// bounds select the same records a hand computation gives on a real DATE column, both ends included.
func TestDrillDateFieldMatchesRecordsAcrossDialects(t *testing.T) {
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
			q := dialect.QuoteIdent
			setup := fmt.Sprintf("CREATE TABLE %s (name VARCHAR(40) NOT NULL PRIMARY KEY, status VARCHAR(40) NOT NULL, total DECIMAL(18,2) NOT NULL, sale_date DATE NOT NULL)", q("tabDay"))
			if _, err := database.DB.Exec(setup); err != nil {
				t.Fatalf("setup: %v", err)
			}
			rows := []struct{ name, status, total, day string }{
				{"D-1", "Paid", "10.00", "2026-11-19"},
				{"D-2", "Paid", "20.00", "2026-11-20"},
				{"D-3", "Return", "5.00", "2026-11-20"},
				{"D-4", "Paid", "30.00", "2026-11-21"},
			}
			for _, r := range rows {
				insert := db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, status, total, sale_date) VALUES (?, ?, ?, ?)", q("tabDay")))
				if _, err := database.DB.Exec(insert, r.name, r.status, r.total, r.day); err != nil {
					t.Fatalf("insert %s: %v", r.name, err)
				}
			}
			cat := dayCatalog()
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			tests := []struct {
				name     string
				from, to tenanttime.Date
				want     string
			}{
				{"two days include both ends", tenanttime.Date{Year: 2026, Month: 11, Day: 19}, tenanttime.Date{Year: 2026, Month: 11, Day: 20}, "30.00"},
				{"one day is that day only", tenanttime.Date{Year: 2026, Month: 11, Day: 20}, tenanttime.Date{Year: 2026, Month: 11, Day: 20}, "20.00"},
				{"the last day is included", tenanttime.Date{Year: 2026, Month: 11, Day: 21}, tenanttime.Date{Year: 2026, Month: 11, Day: 21}, "30.00"},
			}
			for _, c := range tests {
				t.Run(c.name, func(t *testing.T) {
					drill, err := cat.Drill(DrillRequest{Metric: "day_sales", From: c.from, To: c.to}, clock)
					if err != nil {
						t.Fatalf("drill: %v", err)
					}
					got := runDrill(t, database.DB, dialect, drill, true)
					if !got.Equal(decimal.RequireFromString(c.want)) {
						t.Fatalf("records sum %s, want %s (filters %v)", got, c.want, drill.Filters)
					}
				})
			}
		})
	}
}

// dayCatalog holds one metric over a record type whose time field is a Date.
func dayCatalog() Catalog {
	day := &doctype.DocType{Name: "Day", Fields: []doctype.Field{{Fieldname: "sale_date", Fieldtype: "Date"}, {Fieldname: "status", Fieldtype: "Data"}, {Fieldname: "total", Fieldtype: "Currency"}},
		Facts: &doctype.FactModel{Time: "sale_date", StatusField: "status", Measures: []doctype.FactMeasure{{Field: "total"}}}}
	defs := []Metric{{Name: "day_sales", Label: "Day sales", Type: KindSimple, Model: "Day",
		Measure: &Measure{Agg: aggSum, Field: "total"}, Filters: []Filter{{Field: "status", Op: "=", Value: "Paid"}}}}
	return NewCatalog(defs, []*doctype.DocType{day})
}
