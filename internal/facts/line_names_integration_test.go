//go:build integration

package facts

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// TestConsumeKeysEveryLineByItsName consumes a three-line sale as the kernel emits it, with each line under
// its stored name. The header is one row, the lines are three rows with distinct ids, and per item the line
// sums are the item totals. A line without a name fails the event and writes nothing.
func TestConsumeKeysEveryLineByItsName(t *testing.T) {
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
			q := dialect.QuoteIdent
			setup := []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", q("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
				fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES ('SAL-1', 'u1', '2026-11-18 09:00:00')", q("tabSale")),
				fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES ('SAL-2', 'u1', '2026-11-18 09:00:00')", q("tabSale")),
			}
			for _, stmt := range setup {
				if _, err := database.DB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			consume := func(ev eventstream.EventOp, record string, data map[string]any) error {
				tx, err := database.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Consume(ctx, tx, saleEventFor(record, 1, ev, data, nil)); err != nil {
					_ = tx.Rollback()
					return err
				}
				return tx.Commit()
			}
			count := func(query string, args ...any) int {
				t.Helper()
				var n int
				if err := database.DB.QueryRowContext(ctx, db.Rebind(dialect, query), args...).Scan(&n); err != nil {
					t.Fatalf("count %q: %v", query, err)
				}
				return n
			}
			header := q("_kora_fact_sale")
			lines := q("_kora_fact_sale_line")

			// Three lines, named as the kernel stores them: distinct ids, two of one item.
			named := func(name, item string, amount float64) map[string]any {
				return map[string]any{"name": name, "item": item, "amount": amount}
			}
			data := map[string]any{"posting_date": "2026-11-18", "total": "100.50", "status": "Paid", "register": "R1",
				"items": []any{named("L1", "I1", 40.25), named("L2", "I2", 30), named("L3", "I1", 30.25)}}
			if err := consume(eventstream.EventInsert, "SAL-1", data); err != nil {
				t.Fatalf("consume sale with named lines: %v", err)
			}
			if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-1' AND line_id = ''", header)); n != 1 {
				t.Fatalf("header rows = %d, want 1", n)
			}
			if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-1'", lines)); n != 3 {
				t.Fatalf("line rows = %d, want 3", n)
			}
			if n := count(fmt.Sprintf("SELECT COUNT(DISTINCT line_id) FROM %s WHERE record_id = 'SAL-1' AND line_id <> ''", lines)); n != 3 {
				t.Fatalf("distinct non-empty line ids = %d, want 3", n)
			}
			if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE line_id = ''", lines)); n != 0 {
				t.Fatalf("line rows with empty id = %d, want 0", n)
			}
			sums := map[string]float64{}
			rows, err := database.DB.QueryContext(ctx, fmt.Sprintf("SELECT item, SUM(line_amount) FROM %s WHERE record_id = 'SAL-1' GROUP BY item", lines))
			if err != nil {
				t.Fatalf("sum by item: %v", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var item string
				var sum float64
				if err := rows.Scan(&item, &sum); err != nil {
					t.Fatalf("scan sum: %v", err)
				}
				sums[item] = sum
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate sums: %v", err)
			}
			if sums["I1"] != 70.5 || sums["I2"] != 30 || len(sums) != 2 {
				t.Fatalf("line sums by item = %v, want I1 70.5 and I2 30", sums)
			}

			// A line without a name is refused, and nothing is written for the record.
			nameless := map[string]any{"posting_date": "2026-11-18", "total": "5", "status": "Paid", "register": "R1",
				"items": []any{map[string]any{"item": "I9", "amount": 5}}}
			if err := consume(eventstream.EventInsert, "SAL-2", nameless); err == nil {
				t.Fatal("consume sale with a nameless line succeeded, want an error")
			}
			if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-2'", header)); n != 0 {
				t.Fatalf("header rows of refused record = %d, want 0", n)
			}
			if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-2'", lines)); n != 0 {
				t.Fatalf("line rows of refused record = %d, want 0", n)
			}
		})
	}
}
