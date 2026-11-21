//go:build integration

package facts

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// TestShopSaleFactsFromPack loads the selling pack's Sale, writes six sales through the consumer and
// checks the daily summary against a hand computation. Held and Draft sales leave no rows; a sale rung up
// after 23:00 on the 18th with sale_date the 19th lands on the 19th.
func TestShopSaleFactsFromPack(t *testing.T) {
	tests := []struct {
		driver, dialect, envVar string
	}{
		{"mysql", "mysql", "KORA_FACTS_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_FACTS_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range tests {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.envVar)
			if dsn == "" {
				t.Skipf("set %s to run", tc.envVar)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			dialect := db.Resolve(tc.dialect)
			ctx := context.Background()
			for _, stmt := range []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
			} {
				if _, err := database.DB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			dts, err := doctype.ParseConfigTree("../../config/_parked/erpsell")
			if err != nil {
				t.Fatalf("parse selling pack: %v", err)
			}
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
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			refresher := &Refresher{DB: database.DB, Dialect: dialect, Registry: reg, Admission: openLane{}, Owner: "test-refresher"}

			// packSale is a pack Sale with one line; the line's cost and profit are the sale's.
			seq := int64(0)
			write := func(record, status, saleDate string, total, profit string, rung time.Time) {
				t.Helper()
				seq++
				data := map[string]any{
					"sale_date": saleDate, "status": status, "register": "R1", "location": "Main",
					"total": total, "base_total": total, "cost_total": "0", "profit": profit,
					"discount_amount": "0", "tax_amount": "0",
					"items": []any{map[string]any{"name": "L-" + record, "product": "P1", "quantity": "1", "line_total": total,
						"line_cost": "0", "line_profit": profit}},
				}
				ev := saleEventFor(record, seq, eventstream.EventInsert, data, nil)
				ev.OccurredAt = rung
				tx, err := database.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Consume(ctx, tx, ev); err != nil {
					_ = tx.Rollback()
					t.Fatalf("consume %s: %v", record, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			day18 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
			lateNight := time.Date(2026, 11, 18, 20, 30, 0, 0, time.UTC) // 23:30 in Nairobi (UTC+3)
			write("SAL-P1", "Paid", "2026-11-18", "100.50", "40.25", day18)
			write("SAL-P2", "Paid", "2026-11-18", "50.00", "20.00", day18)
			write("SAL-P3", "Paid", "2026-11-19", "30.00", "10.00", lateNight)
			write("SAL-R1", "Return", "2026-11-19", "-20.00", "-8.00", day18)
			write("SAL-H1", "Held", "2026-11-18", "999.00", "999.00", day18)
			write("SAL-D1", "Draft", "2026-11-18", "888.00", "888.00", day18)

			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh: %v", err)
			}
			got := dailyTotals(t, database.DB, dialect)
			want := map[string]string{
				"2026-11-18|Paid":   "2|150.5|60.25",
				"2026-11-19|Paid":   "1|30|10",
				"2026-11-19|Return": "1|-20|-8",
			}
			if len(got) != len(want) {
				t.Fatalf("daily totals = %v, want %v", got, want)
			}
			for key, w := range want {
				if got[key] != w {
					t.Errorf("%s = %q, want %q", key, got[key], w)
				}
			}
		})
	}
}

// dailyTotals reads the day-total grain of the Sale summary: "day|status" -> "rows|total|profit".
func dailyTotals(t *testing.T, conn *sql.DB, dialect db.Dialect) map[string]string {
	t.Helper()
	q := dialect.QuoteIdent
	query := fmt.Sprintf("SELECT %s, %s, %s, %s, %s FROM %s WHERE %s = ''",
		q("business_date"), q("status"), q("row_count"), q("total"), q("profit"),
		q("_kora_fact_summary_sale"), q("grain"))
	rows, err := conn.Query(query)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var day time.Time
		var status, count, total, profit string
		if err := rows.Scan(&day, &status, &count, &total, &profit); err != nil {
			t.Fatal(err)
		}
		out[day.Format("2006-01-02")+"|"+status] = count + "|" + norm(t, total) + "|" + norm(t, profit)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
