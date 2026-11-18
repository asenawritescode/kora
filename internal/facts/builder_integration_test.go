//go:build integration

package facts

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
	"github.com/asenawritescode/kora/internal/outbox"
)

// TestBuilderKeepsFactsCurrent drives one Sale through insert, line removal, a stale
// redelivery, backdating, delete and a redelivered delete, then rejects a missing date.
func TestBuilderKeepsFactsCurrent(t *testing.T) {
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
			setup := []string{
				fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) NOT NULL PRIMARY KEY, owner VARCHAR(140) NOT NULL, creation TIMESTAMP NOT NULL)", dialect.QuoteIdent("tabSale")),
				"CREATE TABLE _kora_site_setting (site VARCHAR(140) NOT NULL, setting_key VARCHAR(140) NOT NULL, setting_value VARCHAR(140) NOT NULL, PRIMARY KEY (site, setting_key))",
				db.Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name, owner, creation) VALUES (?, ?, ?)", dialect.QuoteIdent("tabSale"))),
			}
			for i, stmt := range setup {
				args := []any{}
				if i == len(setup)-1 {
					args = []any{"SAL-1", "u1", "2026-11-01 10:00:00"}
				}
				if _, err := database.DB.ExecContext(ctx, stmt, args...); err != nil {
					t.Fatalf("setup %q: %v", stmt, err)
				}
			}
			reg := doctype.NewRegistry()
			reg.Register(saleFacts())
			builder := &Builder{DB: database.DB, Dialect: dialect, Registry: reg}
			consume := func(seq int64, op eventstream.EventOp, data, old map[string]any) error {
				tx, err := database.DB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := builder.Consume(ctx, tx, saleEvent(seq, op, data, old)); err != nil {
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
			header := dialect.QuoteIdent("_kora_fact_sale")
			lines := dialect.QuoteIdent("_kora_fact_sale_line")
			dirty := dialect.QuoteIdent(dirtyTable)
			expectHeader := func(t *testing.T, seq int64, deleted bool, date string) {
				t.Helper()
				if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-1' AND line_id = '' AND source_sequence = ? AND deleted = ? AND business_date = ?", header), seq, deleted, date); n != 1 {
					t.Fatalf("header seq %d deleted %v date %s: %d rows, want 1", seq, deleted, date, n)
				}
			}
			expectLines := func(t *testing.T, active int) {
				t.Helper()
				if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE record_id = 'SAL-1' AND deleted = ?", lines), false); n != active {
					t.Fatalf("active lines = %d, want %d", n, active)
				}
			}
			expectDirty := func(t *testing.T, date string) {
				t.Helper()
				if n := count(fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE model = 'Sale' AND business_date = ?", dirty), date); n != 1 {
					t.Fatalf("dirty marker for %s: %d rows, want 1", date, n)
				}
			}

			l1, l2 := line("L1", "I1", "40.25"), line("L2", "I2", "60.25")
			insert := sale("2026-11-18", l1, l2)
			if err := consume(1, eventstream.EventInsert, insert, nil); err != nil {
				t.Fatalf("insert: %v", err)
			}
			expectHeader(t, 1, false, "2026-11-18")
			expectLines(t, 2)
			expectDirty(t, "2026-11-18")

			if err := consume(2, eventstream.EventUpdate, sale("2026-11-18", l1), insert); err != nil {
				t.Fatalf("remove line: %v", err)
			}
			expectHeader(t, 2, false, "2026-11-18")
			expectLines(t, 1)

			if err := consume(1, eventstream.EventInsert, insert, nil); err != nil {
				t.Fatalf("stale redelivery: %v", err)
			}
			expectHeader(t, 2, false, "2026-11-18")
			expectLines(t, 1)

			backdated := sale("2026-11-10", l1)
			if err := consume(3, eventstream.EventUpdate, backdated, sale("2026-11-18", l1)); err != nil {
				t.Fatalf("backdate: %v", err)
			}
			expectHeader(t, 3, false, "2026-11-10")
			expectDirty(t, "2026-11-10")
			expectDirty(t, "2026-11-18")

			if err := consume(4, eventstream.EventDelete, nil, backdated); err != nil {
				t.Fatalf("delete: %v", err)
			}
			expectHeader(t, 4, true, "2026-11-10")
			expectLines(t, 0)
			for i := 0; i < 2; i++ {
				if err := consume(4, eventstream.EventDelete, nil, backdated); err != nil {
					t.Fatalf("redelivered delete %d: %v", i, err)
				}
			}
			expectHeader(t, 4, true, "2026-11-10")

			if err := consume(5, eventstream.EventUpdate, sale("", l1), backdated); err == nil {
				t.Fatal("update with an empty business date was stored")
			}
			expectHeader(t, 4, true, "2026-11-10")
		})
	}
}

// saleEvent builds the outbox envelope the orm writes for a Sale change.
func saleEvent(seq int64, op eventstream.EventOp, data, old map[string]any) contract.EventEnvelope {
	return saleEventFor("SAL-1", seq, op, data, old)
}

// saleEventFor is a Sale change event for one record.
func saleEventFor(record string, seq int64, op eventstream.EventOp, data, old map[string]any) contract.EventEnvelope {
	payload := map[string]any{"data": data, "old_data": old, "table": "Sale", "record_id": record}
	return contract.EventEnvelope{
		ID: fmt.Sprintf("evt-%d", seq), Type: outbox.EventTypeName("Sale", op),
		Sequence: contract.Revision(strconv.FormatInt(seq, 10)), Site: "test",
		BusinessScope: contract.BusinessScope{LegalEntityID: "le1"},
		OccurredAt:    time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		Data:          contract.MustEncodeData(payload),
	}
}

// sale is a Sale payload with the given posting date and lines.
func sale(date string, items ...map[string]any) map[string]any {
	rows := make([]any, len(items))
	for i, item := range items {
		rows[i] = item
	}
	return map[string]any{"posting_date": date, "total": "100.50", "status": "Paid", "register": "R1", "items": rows}
}
