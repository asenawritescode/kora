//go:build integration

package dbaccess

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestDatabaseLockContentionMetricsAgainstSupportedDialects(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		env    string
	}{
		{name: "mysql", driver: "mysql", env: "KORA_DBACCESS_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", env: "KORA_DBACCESS_DISPOSABLE_POSTGRES_DSN"},
		{name: "libsql-http", driver: "libsql", env: "KORA_DBACCESS_DISPOSABLE_LIBSQL_DSN"},
	}
	configured := false
	for _, tc := range cases {
		configured = configured || os.Getenv(tc.env) != ""
	}
	if !configured {
		t.Skip("set KORA_DBACCESS_DISPOSABLE_{MYSQL,POSTGRES,LIBSQL}_DSN to fresh disposable databases or a disposable LibSQL HTTP endpoint")
	}
	for _, tc := range cases {
		dsn := os.Getenv(tc.env)
		if dsn == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			database, err := Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open budgeted disposable database:", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping disposable database:", err)
			}
			table := fmt.Sprintf("_kora_lock_metric_%x", time.Now().UnixNano())
			if _, err := database.ExecContext(ctx, `CREATE TABLE `+table+` (id INTEGER PRIMARY KEY, value INTEGER NOT NULL)`); err != nil {
				t.Fatal("create disposable lock fixture:", err)
			}
			t.Cleanup(func() { _, _ = database.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+table) })
			if _, err := database.ExecContext(ctx, `INSERT INTO `+table+` (id,value) VALUES (1,0)`); err != nil {
				t.Fatal("insert lock fixture:", err)
			}

			first, err := database.Conn(ctx)
			if err != nil {
				t.Fatal("acquire first database session:", err)
			}
			defer first.Close()
			second, err := database.Conn(ctx)
			if err != nil {
				t.Fatal("acquire second database session:", err)
			}
			defer second.Close()

			budget, err := budgetFor(ServerKey(tc.driver, dsn))
			if err != nil {
				t.Fatal("resolve endpoint budget:", err)
			}
			before := budget.Snapshot().LockContention
			err = induceRowLockTimeout(ctx, tc.driver, first, second, table)
			if err == nil {
				if tc.driver != "libsql" {
					t.Fatal("second writer unexpectedly acquired a row locked by the first session")
				}
				// LibSQL's HTTP service serializes this pair of writes instead of
				// returning a SQLITE_BUSY/LOCKED signal. The driver-shaped unit tests
				// cover those classifier inputs; this transport must not report a
				// contention error that it did not receive.
				if got := budget.Snapshot().LockContention; got != before {
					t.Fatalf("LibSQL lock contention counter = %d, before=%d; write serialization returned no lock error", got, before)
				}
				return
			}
			if !isLockContention(err) {
				t.Fatalf("database returned unclassified lock error %T: %v", err, err)
			}
			if got := budget.Snapshot().LockContention; got <= before {
				t.Fatalf("observed lock contention counter = %d, before=%d; want an increment", got, before)
			}
		})
	}
}

func induceRowLockTimeout(ctx context.Context, driver string, first, second *sql.Conn, table string) error {
	if driver == "libsql" {
		firstTx, err := first.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin first transaction: %w", err)
		}
		defer firstTx.Rollback()
		if _, err := firstTx.ExecContext(ctx, `UPDATE `+table+` SET value=value+1 WHERE id=1`); err != nil {
			return fmt.Errorf("lock fixture row: %w", err)
		}
		secondTx, err := second.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin competing transaction: %w", err)
		}
		defer secondTx.Rollback()
		_, err = secondTx.ExecContext(ctx, `UPDATE `+table+` SET value=value+1 WHERE id=1`)
		return err
	}
	if _, err := first.ExecContext(ctx, `BEGIN`); err != nil {
		return fmt.Errorf("begin first transaction: %w", err)
	}
	defer first.ExecContext(context.Background(), `ROLLBACK`)
	if _, err := first.ExecContext(ctx, `UPDATE `+table+` SET value=value+1 WHERE id=1`); err != nil {
		return fmt.Errorf("lock fixture row: %w", err)
	}
	if _, err := second.ExecContext(ctx, `BEGIN`); err != nil {
		return fmt.Errorf("begin competing transaction: %w", err)
	}
	defer second.ExecContext(context.Background(), `ROLLBACK`)
	if driver == "mysql" {
		if _, err := second.ExecContext(ctx, `SET innodb_lock_wait_timeout=1`); err != nil {
			return fmt.Errorf("set short MySQL lock timeout: %w", err)
		}
	} else {
		if _, err := second.ExecContext(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
			return fmt.Errorf("set short PostgreSQL lock timeout: %w", err)
		}
	}
	_, err := second.ExecContext(ctx, `UPDATE `+table+` SET value=value+1 WHERE id=1`)
	return err
}
