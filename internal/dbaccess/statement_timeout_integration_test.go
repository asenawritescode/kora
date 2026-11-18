//go:build integration

package dbaccess

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// TestStatementTimeoutCancelsSlowReadsOnDisposableDatabases proves a request
// pool cancels a ~2 s read under a 500 ms limit, while a maintenance pool does not.
// MySQL max_execution_time only bounds SELECT and reports SLEEP() as a silent
// interruption, so its slow read is a recursive CTE that fails with error 3024.
func TestStatementTimeoutCancelsSlowReadsOnDisposableDatabases(t *testing.T) {
	const mysqlSlowRead = "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < 3000000) SELECT COUNT(*) FROM n"
	cases := []struct {
		name, driver, env, setup, slow, wantErr string
	}{
		{name: "mysql", driver: "mysql", env: "KORA_DBACCESS_DISPOSABLE_MYSQL_DSN", setup: "SET SESSION cte_max_recursion_depth=10000000", slow: mysqlSlowRead, wantErr: "maximum statement execution time exceeded"},
		{name: "postgres", driver: "postgres", env: "KORA_DBACCESS_DISPOSABLE_POSTGRES_DSN", slow: "SELECT COUNT(*) FROM pg_sleep(2)", wantErr: "statement timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a fresh disposable %s database", tc.env, tc.driver)
			}
			t.Setenv("KORA_DB_STATEMENT_TIMEOUT_MS", "500")

			request, err := Open(tc.driver, dsn)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer request.Close()
			start := time.Now()
			var got int
			err = slowRead(context.Background(), request, tc.setup, tc.slow, &got)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("request pool error = %v, want one containing %q", err, tc.wantErr)
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Fatalf("request pool took %v, want cancellation near 500 ms", elapsed)
			}

			maintenance, err := OpenMaintenance(tc.driver, dsn)
			if err != nil {
				t.Fatalf("OpenMaintenance() error = %v", err)
			}
			defer maintenance.Close()
			if err := slowRead(context.Background(), maintenance, tc.setup, tc.slow, &got); err != nil {
				t.Fatalf("maintenance pool error = %v, want the slow read to complete", err)
			}
		})
	}
}

// slowRead runs one read on a single pooled connection, applying the optional
// session setup first so the timeout still governs the slow statement itself.
func slowRead(ctx context.Context, database *sql.DB, setup, query string, got *int) error {
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if setup != "" {
		if _, err := conn.ExecContext(ctx, setup); err != nil {
			return err
		}
	}
	return conn.QueryRowContext(ctx, query).Scan(got)
}
