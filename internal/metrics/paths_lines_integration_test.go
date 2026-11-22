//go:build integration

package metrics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// lineSale is one selling-pack sale with its lines: the header total is the sum of the line totals.
type lineSale struct {
	record, status, date string
	lines                []saleLine
}

// saleLine is one line of a sale: the product it sells and its line total.
type saleLine struct {
	name, product, quantity, total, cost string
}

// lineSales are the Sale records, all paid. P1 is Tea (Beverages), P2 is Bread (Bakery). SAL-D, on 20
// November, holds two lines of P1: it counts once in a count of sales by product.
var lineSales = []lineSale{
	{"SAL-A", "Paid", "2026-11-18", []saleLine{{"L-A1", "P1", "2", "100.00", "60.00"}, {"L-A2", "P2", "1", "50.00", "30.00"}}},
	{"SAL-B", "Paid", "2026-11-18", []saleLine{{"L-B1", "P1", "1", "50.00", "30.00"}}},
	{"SAL-C", "Paid", "2026-11-19", []saleLine{{"L-C1", "P2", "3", "150.00", "90.00"}}},
	{"SAL-D", "Paid", "2026-11-20", []saleLine{{"L-D1", "P1", "1", "40.00", "20.00"}, {"L-D2", "P1", "1", "60.00", "30.00"}}},
}

// TestSaleLinesGroupByProductAcrossDialects groups selling-pack sale lines by product and by a
// field of the product. Expected per day: 18 Nov Tea 150 (100 + 50), Bread 50; 19 Nov Bread 150.
func TestSaleLinesGroupByProductAcrossDialects(t *testing.T) {
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
			runLineCases(t, engine, names[0])
		})
	}
}

// runLineCases checks each grouping's buckets, and the refusal of a product path the caller may not join.
func runLineCases(t *testing.T, engine Engine, sum string) {
	t.Helper()
	ctx := context.Background()
	product := map[string]bool{"Product": true}
	tests := []struct {
		name string
		dim  string
		want []lineGroupRow
	}{
		{"best sellers by product carry product names", "items.product", []lineGroupRow{
			{"P1", "18", "150", "Tea"}, {"P1", "19", "0", "Tea"},
			{"P2", "18", "50", "Bread"}, {"P2", "19", "150", "Bread"},
		}},
		{"sales by the product's category", "items.product.category", []lineGroupRow{
			{"Bakery", "18", "50", ""}, {"Bakery", "19", "150", ""},
			{"Beverages", "18", "150", ""}, {"Beverages", "19", "0", ""},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := engine.Query(ctx, Query{Metrics: []string{sum}, Dimensions: []string{tc.dim},
				From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 19},
				Grain: GrainDay, Scope: Scope{Joinable: product}})
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			checkLineRows(t, res, tc.want)
		})
	}
	t.Run("a product path is refused without the product record type", func(t *testing.T) {
		_, err := engine.Query(ctx, Query{Metrics: []string{sum}, Dimensions: []string{"items.product.category"},
			From: tenanttime.Date{Year: 2026, Month: 11, Day: 18}, To: tenanttime.Date{Year: 2026, Month: 11, Day: 19}, Grain: GrainDay})
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})
}

// lineGroupRow is one expected bucket: its group, its day, its sum, and the title of the group's product.
type lineGroupRow struct {
	group, day, sum, label string
}

// checkLineRows compares every bucket, in order, with its expectation.
func checkLineRows(t *testing.T, res Result, want []lineGroupRow) {
	t.Helper()
	if len(res.Rows) != len(want) {
		t.Fatalf("rows = %d, want %d", len(res.Rows), len(want))
	}
	for i, row := range res.Rows {
		w := want[i]
		if row.Group[0] != w.group || row.Period.String()[8:] != w.day || labelText(row) != w.label {
			t.Errorf("row %d = %v %s label %q, want %s %s label %q", i, row.Group, row.Period, labelText(row), w.group, w.day, w.label)
		}
		v := row.Values[0]
		if !v.Valid || !v.Decimal.Equal(decimal.RequireFromString(w.sum)) {
			t.Errorf("row %d sum = %v, want %s", i, v, w.sum)
		}
	}
}

// seedSaleLines creates the tables the selling pack's facts and products read: sales, the site settings the
// business clock reads, and the Product table with its titles and categories.
func seedSaleLines(t *testing.T, conn *sql.DB, dialect db.Dialect) {
	t.Helper()
	ctx := context.Background()
	q := dialect.QuoteIdent
	steps := []string{
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL, reference VARCHAR(140) NOT NULL DEFAULT '')", q("tabSale")),
		"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
		fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, product_name VARCHAR(140) NOT NULL, category VARCHAR(140) NOT NULL)", q("tabProduct")),
	}
	for _, stmt := range steps {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	inserts := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)", []any{"test", tenanttime.KeyTimeZone, "Africa/Nairobi"}},
		{fmt.Sprintf("INSERT INTO %s (name, product_name, category) VALUES (?, ?, ?)", q("tabProduct")), []any{"P1", "Tea", "Beverages"}},
		{fmt.Sprintf("INSERT INTO %s (name, product_name, category) VALUES (?, ?, ?)", q("tabProduct")), []any{"P2", "Bread", "Bakery"}},
	}
	for _, in := range inserts {
		if _, err := conn.ExecContext(ctx, db.Rebind(dialect, in.query), in.args...); err != nil {
			t.Fatalf("insert %q: %v", in.query, err)
		}
	}
}

// writeSaleLines consumes each sale through the fact builder, as a Sale insert event with its lines.
func writeSaleLines(t *testing.T, conn *sql.DB, dialect db.Dialect, dts []*doctype.DocType) {
	t.Helper()
	ctx := context.Background()
	var sale *doctype.DocType
	for _, dt := range dts {
		if dt.Name == "Sale" {
			sale = dt
		}
	}
	if sale == nil || sale.Facts == nil {
		t.Fatal("selling pack has no Sale fact model")
	}
	reg := doctype.NewRegistry()
	reg.Register(sale)
	builder := &facts.Builder{DB: conn, Dialect: dialect, Registry: reg}
	for i, s := range lineSales {
		items := make([]any, 0, len(s.lines))
		var total, cost decimal.Decimal
		for _, l := range s.lines {
			items = append(items, map[string]any{"name": l.name, "product": l.product, "quantity": l.quantity,
				"line_total": l.total, "line_cost": l.cost, "line_profit": "0"})
			total = total.Add(decimal.RequireFromString(l.total))
			cost = cost.Add(decimal.RequireFromString(l.cost))
		}
		data := map[string]any{"sale_date": s.date, "status": s.status, "register": "R1", "location": "Main",
			"total": total.String(), "base_total": total.String(), "cost_total": cost.String(), "profit": total.Sub(cost).String(),
			"discount_amount": "0", "tax_amount": "0", "items": items}
		event := contract.EventEnvelope{
			ID: fmt.Sprintf("evt-%d", i+1), Type: outbox.EventTypeName("Sale", eventstream.EventInsert),
			Sequence: contract.Revision(fmt.Sprint(i + 1)), Site: "test",
			OccurredAt: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
			Data:       contract.MustEncodeData(map[string]any{"table": "Sale", "record_id": s.record, "data": data}),
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := builder.Consume(ctx, tx, event); err != nil {
			_ = tx.Rollback()
			t.Fatalf("consume %s: %v", s.record, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}
