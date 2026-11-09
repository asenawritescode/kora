//go:build integration

package payment

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

func openStore(t *testing.T, driver, envKey string, d db.Dialect, ddl []string) *SQLStore {
	t.Helper()
	dsn := os.Getenv(envKey)
	if dsn == "" {
		t.Skipf("set %s", envKey)
	}
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, _ = conn.Exec(db.Rebind(d, `DROP TABLE IF EXISTS _kora_payment_intent`))
	for _, stmt := range ddl {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("ddl: %v\n%s", err, stmt)
		}
	}
	return &SQLStore{DB: conn, Dialect: d}
}

func TestSQLStore(t *testing.T) {
	for _, tc := range []struct {
		name, driver, env string
		d                 db.Dialect
		ddl               []string
	}{
		{"mysql", "mysql", "KORA_PAYMENT_MYSQL_DSN", &db.MySQLDialect{}, db.PaymentTablesMySQL()},
		{"postgres", "postgres", "KORA_PAYMENT_POSTGRES_DSN", &db.PostgresDialect{}, db.PaymentTablesPostgres()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t, tc.driver, tc.env, tc.d, tc.ddl)
			svc := &Service{Store: st, Providers: NewRegistry()}
			ctx := context.Background()
			stub := &asyncProv{}
			_ = svc.Providers.Register(stub)
			in := RequestInput{Provider: "async", Amount: decimal.RequireFromString("12.50"), Currency: "KES", IdempotencyKey: "k1"}
			a, err := svc.Request(ctx, "s1", "u", in)
			if err != nil || a.State != StatePending {
				t.Fatalf("request: %v %+v", err, a)
			}
			b, err := svc.Request(ctx, "s1", "u", in)
			if err != nil || b.ID != a.ID || stub.calls != 1 {
				t.Fatalf("replay: %v %+v calls=%d", err, b, stub.calls)
			}
			if !b.Amount.Equal(a.Amount) {
				t.Fatalf("amount %s", b.Amount)
			}
			var ce *contract.Error
			in.Amount = decimal.NewFromInt(1)
			if _, err := svc.Request(ctx, "s1", "u", in); !errors.As(err, &ce) || ce.Type != contract.CodeConflict {
				t.Fatalf("want conflict, got %v", err)
			}
			if _, err := st.GetByKey(ctx, "s2", "k1"); err != ErrNotFound {
				t.Fatalf("site scoping: %v", err)
			}
			got, err := st.GetByProviderRequest(ctx, "s1", "async", "req-1")
			if err != nil || got.ID != a.ID {
				t.Fatalf("by provider req: %v", err)
			}
			var applied, dup int32
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, r, err := svc.Confirm(ctx, "s1", Confirmation{Provider: "async", ProviderRequestID: "req-1", Success: true, ProviderReference: "R1"})
					if err != nil {
						t.Error(err)
					}
					switch r {
					case ConfirmApplied:
						atomic.AddInt32(&applied, 1)
					case ConfirmDuplicate:
						atomic.AddInt32(&dup, 1)
					}
				}()
			}
			wg.Wait()
			if applied != 1 || dup != 7 {
				t.Fatalf("applied=%d dup=%d", applied, dup)
			}
			_, r, _ := svc.Confirm(ctx, "s1", Confirmation{Provider: "async", ProviderRequestID: "req-1", Success: false})
			if r != ConfirmConflict {
				t.Fatalf("want conflict, got %s", r)
			}
		})
	}
}

type asyncProv struct{ calls int }

func (*asyncProv) Name() string { return "async" }
func (p *asyncProv) Request(context.Context, Intent) (Outcome, error) {
	p.calls++
	return Outcome{State: StatePending, ProviderRequestID: "req-1"}, nil
}
