//go:build integration

package kernel

import (
	"database/sql"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestConsumePaymentIntent(t *testing.T) {
	for _, tc := range []struct {
		name, driver, env string
		d                 db.Dialect
		ddl               []string
	}{
		{"mysql", "mysql", "KORA_PAYMENT_MYSQL_DSN", &db.MySQLDialect{}, db.PaymentTablesMySQL()},
		{"postgres", "postgres", "KORA_PAYMENT_POSTGRES_DSN", &db.PostgresDialect{}, db.PaymentTablesPostgres()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetMaxOpenConns(16)
			_, _ = conn.Exec("DROP TABLE IF EXISTS _kora_payment_intent")
			for _, stmt := range tc.ddl {
				if _, err := conn.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			seed := func(id, state, amount, currency string, consumed any) {
				_, err := conn.Exec(db.Rebind(tc.d, `INSERT INTO _kora_payment_intent
				(id, site, provider, state, amount, currency, idempotency_key, payload_hash, consumed_by, created_at, updated_at)
				VALUES (?, 's1', 'cash', ?, ?, ?, ?, 'h', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`), id, state, amount, currency, id, consumed)
				if err != nil {
					t.Fatal(err)
				}
			}
			seed("ok1", "confirmed", "100.00", "KES", nil)
			seed("pend", "pending", "100.00", "KES", nil)
			seed("fail", "failed", "100.00", "KES", nil)
			seed("used", "confirmed", "100.00", "KES", "OTHEROP")
			seed("race", "confirmed", "100.00", "KES", nil)

			consume := func(id, op, amount, currency string) *contract.Error {
				tx, err := conn.Begin()
				if err != nil {
					t.Fatal(err)
				}
				cerr := consumePaymentIntent(tx, tc.d, "s1", op, id, decimal.RequireFromString(amount), currency)
				if cerr != nil {
					_ = tx.Rollback()
					return cerr
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			for _, bad := range []struct{ id, amount, currency string }{
				{"pend", "100", "KES"}, {"fail", "100", "KES"}, {"used", "100", "KES"},
				{"ok1", "99.99", "KES"}, {"ok1", "100", "USD"}, {"missing", "100", "KES"},
			} {
				if cerr := consume(bad.id, "OP1", bad.amount, bad.currency); cerr == nil || cerr.Type != contract.CodeValidationFailed {
					t.Errorf("%+v: want validation rejection, got %v", bad, cerr)
				}
			}
			if cerr := consume("ok1", "OP1", "100", "KES"); cerr != nil {
				t.Fatalf("confirmed+matching: %v", cerr)
			}
			if cerr := consume("ok1", "OP2", "100", "KES"); cerr == nil {
				t.Fatal("reuse by second command must be rejected")
			}
			var by string
			_ = conn.QueryRow("SELECT consumed_by FROM _kora_payment_intent WHERE id = 'ok1'").Scan(&by)
			if by != "OP1" {
				t.Fatalf("consumed_by = %q", by)
			}

			var wins int32
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					if consume("race", string(rune('A'+i)), "100", "KES") == nil {
						atomic.AddInt32(&wins, 1)
					}
				}(i)
			}
			wg.Wait()
			if wins != 1 {
				t.Fatalf("race winners = %d, want 1", wins)
			}
		})
	}
}
