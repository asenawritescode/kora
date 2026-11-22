//go:build integration

package metrics

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// drillDialects are the two dialects every drill test runs on, with the DSN variable that names each.
var drillDialects = []struct {
	driver, dialect, envVar string
}{
	{"mysql", "mysql", "KORA_METRICS_DISPOSABLE_MYSQL_DSN"},
	{"postgres", "postgres", "KORA_METRICS_DISPOSABLE_POSTGRES_DSN"},
}

// TestDrillRecordsMatchQueryAcrossDialects checks that each day's drill names the query's number, and that
// its records add up to it, newest first, on the real POS sales.
func TestDrillRecordsMatchQueryAcrossDialects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range drillDialects {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			engine := seed(t, database.DB, dialect)
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			for _, d := range []int{18, 19, 20} {
				day := tenanttime.Date{Year: 2026, Month: 11, Day: d}
				res, err := engine.Query(ctx, Query{Metrics: []string{"sales", "receipts"}, From: day, To: day, Grain: GrainDay})
				if err != nil {
					t.Fatalf("query %s: %v", day, err)
				}
				for i, metric := range []string{"sales", "receipts"} {
					page, err := engine.DrillRecords(ctx, DrillRequest{Metric: metric, From: day, To: day}, Scope{}, clock, "")
					if err != nil {
						t.Fatalf("drill %s %s: %v", metric, day, err)
					}
					if want := res.Rows[0].Values[i].Decimal; !page.Total.Equal(want) {
						t.Fatalf("%s on %s: drill total %s, query says %s", metric, day, page.Total, want)
					}
					if page.Next != "" {
						t.Fatalf("%s on %s: a page of %d records has a next cursor", metric, day, len(page.Records))
					}
					sum := decimal.Zero
					for _, r := range page.Records {
						sum = sum.Add(r.Value)
					}
					if !sum.Equal(page.Total) {
						t.Fatalf("%s on %s: records sum %s, total %s", metric, day, sum, page.Total)
					}
					checkNewestFirst(t, page.Records)
					if !page.ListOpenable {
						t.Fatalf("%s on %s: a header drill is not openable", metric, day)
					}
				}
			}
		})
	}
}

// TestDrillRecordsPagesAcrossDialects pages through the records of a drill one page at a time: the pages
// join up to the full list, with no record twice and the same total on every page.
func TestDrillRecordsPagesAcrossDialects(t *testing.T) {
	ctx := context.Background()
	old := drillPageSize
	t.Cleanup(func() { drillPageSize = old })
	for _, tc := range drillDialects {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			engine := seed(t, database.DB, db.Resolve(tc.dialect))
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			req := DrillRequest{Metric: "receipts", From: tenanttime.Date{Year: 2026, Month: 11, Day: 17}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 20}}

			drillPageSize = 50
			full, err := engine.DrillRecords(ctx, req, Scope{}, clock, "")
			if err != nil {
				t.Fatalf("full drill: %v", err)
			}
			if len(full.Records) < 3 {
				t.Fatalf("seed has %d receipts, want at least 3 to page", len(full.Records))
			}

			drillPageSize = 2
			var paged []DrillRecord
			cursor := ""
			for pages := 0; ; pages++ {
				page, err := engine.DrillRecords(ctx, req, Scope{}, clock, cursor)
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				if !page.Total.Equal(full.Total) {
					t.Fatalf("page %d total %s, want %s", pages, page.Total, full.Total)
				}
				paged = append(paged, page.Records...)
				if page.Next == "" {
					break
				}
				if len(page.Records) != 2 {
					t.Fatalf("page %d has %d records before a next cursor, want 2", pages, len(page.Records))
				}
				cursor = page.Next
			}
			if len(paged) != len(full.Records) {
				t.Fatalf("paged %d records, full list has %d", len(paged), len(full.Records))
			}
			for i := range paged {
				if paged[i].Name != full.Records[i].Name {
					t.Fatalf("record %d is %s paged, %s in full", i, paged[i].Name, full.Records[i].Name)
				}
			}
			if _, err := engine.DrillRecords(ctx, req, Scope{}, clock, "bogus"); !errors.Is(err, ErrBadCursor) {
				t.Fatalf("bad cursor error = %v, want ErrBadCursor", err)
			}
		})
	}
}

// TestDrillCountsRecordsOnceAcrossDialects checks the count of sales by a line dimension: a sale with two
// lines of one product counts once, in the query and in its drill, and the drill's records are that sale.
func TestDrillCountsRecordsOnceAcrossDialects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range drillDialects {
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
			cat, names, err := NewCatalog(nil, dts).WithAdHoc([]AdHoc{{Model: "Sale", Agg: aggCount}})
			if err != nil {
				t.Fatal(err)
			}
			engine := Engine{DB: database.DB, Dialect: dialect, Catalog: cat}
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			day20 := tenanttime.Date{Year: 2026, Month: 11, Day: 20}

			res, err := engine.Query(ctx, Query{Metrics: names, Dimensions: []string{"items.product"}, From: day20, To: day20, Grain: GrainRange})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(res.Rows) != 1 || res.Rows[0].Group[0] != "P1" || !res.Rows[0].Values[0].Decimal.Equal(decimal.NewFromInt(1)) {
				t.Fatalf("sales by product on 20 Nov = %+v, want one sale of P1", res.Rows)
			}
			page, err := engine.DrillRecords(ctx, DrillRequest{Metric: names[0], From: day20, To: day20, Group: map[string]string{"items.product": "P1"}}, Scope{}, clock, "")
			if err != nil {
				t.Fatalf("drill: %v", err)
			}
			if !page.Total.Equal(decimal.NewFromInt(1)) || len(page.Records) != 1 || page.Records[0].Name != "SAL-D" {
				t.Fatalf("drill total %s records %+v, want SAL-D once", page.Total, page.Records)
			}
			if page.ListOpenable {
				t.Fatal("a line group drill is not openable")
			}
			// The sum of a line field is still a line sum: the POS count on 18 Nov is two sales for P1.
			res, err = engine.Query(ctx, Query{Metrics: names, Dimensions: []string{"items.product"},
				From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, Grain: GrainRange})
			if err != nil {
				t.Fatalf("query 18 Nov: %v", err)
			}
			for _, row := range res.Rows {
				if row.Group[0] == "P1" && !row.Values[0].Decimal.Equal(decimal.NewFromInt(2)) {
					t.Fatalf("P1 sales on 18 Nov = %s, want 2 (SAL-A and SAL-B)", row.Values[0].Decimal)
				}
			}
		})
	}
}

// TestDrillThroughLinkPathAcrossDialects drills an ad-hoc count of expenses through a link path: the
// total is the query's number for the supplier town, and the records are the expenses behind it.
func TestDrillThroughLinkPathAcrossDialects(t *testing.T) {
	ctx := context.Background()
	for _, tc := range drillDialects {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			seedExpenses(t, database.DB, dialect)
			seedSuppliers(t, database.DB, dialect)
			engine, names := pathEngine(t, database.DB, dialect)
			clock := tenanttime.Clock{Location: mustNairobi(t)}
			day := tenanttime.Date{Year: 2026, Month: 11, Day: 18}
			group := map[string]string{"supplier.town": "Nairobi"}
			joinable := Scope{Joinable: map[string]bool{"Supplier": true}}

			res, err := engine.Query(ctx, Query{Metrics: []string{names[0]}, Dimensions: []string{"supplier.town"}, From: day, To: day, Grain: GrainRange, Scope: joinable})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			var want decimal.Decimal
			for _, row := range res.Rows {
				if row.Group[0] == "Nairobi" {
					want = row.Values[0].Decimal
				}
			}
			page, err := engine.DrillRecords(ctx, DrillRequest{Metric: names[0], From: day, To: day, Group: group}, joinable, clock, "")
			if err != nil {
				t.Fatalf("drill: %v", err)
			}
			if !page.Total.Equal(want) || int64(len(page.Records)) != want.IntPart() {
				t.Fatalf("drill total %s with %d records, query says %s", page.Total, len(page.Records), want)
			}
			if page.ListOpenable {
				t.Fatal("a path group drill is not openable")
			}
			if _, err := engine.DrillRecords(ctx, DrillRequest{Metric: names[0], From: day, To: day, Group: group}, Scope{}, clock, ""); !errors.Is(err, ErrForbidden) {
				t.Fatalf("drill without the supplier record type = %v, want ErrForbidden", err)
			}
		})
	}
}

// checkNewestFirst fails unless records run newest first, ties broken by name, descending.
func checkNewestFirst(t *testing.T, records []DrillRecord) {
	t.Helper()
	for i := 1; i < len(records); i++ {
		prev, cur := records[i-1], records[i]
		if prev.OccurredAt.Before(cur.OccurredAt) || (prev.OccurredAt.Equal(cur.OccurredAt) && prev.Name < cur.Name) {
			t.Fatalf("record %s is listed before %s, which is older or ties with a smaller name", prev.Name, cur.Name)
		}
	}
}
