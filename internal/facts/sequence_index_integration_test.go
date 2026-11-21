//go:build integration

package facts

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestSequenceIndexAddedToExistingFactTablesAcrossDialects simulates fact tables created before the
// source_sequence index existed, then runs the ensure step twice: the first run adds the index to the
// header and line tables, the second finds it already there and must not fail.
func TestSequenceIndexAddedToExistingFactTablesAcrossDialects(t *testing.T) {
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
			dt := saleFacts()
			for _, stmt := range DDL(dialect, dt) {
				if strings.HasPrefix(stmt, "CREATE INDEX") && strings.Contains(stmt, sequenceColumn) {
					continue // the table predates the index
				}
				if _, err := database.DB.Exec(stmt); err != nil {
					t.Fatalf("DDL %q: %v", stmt, err)
				}
			}
			b := &Builder{DB: database.DB, Dialect: dialect}
			for run := 1; run <= 2; run++ {
				if err := b.ensureSequenceIndexes(context.Background(), dt); err != nil {
					t.Fatalf("ensure run %d: %v", run, err)
				}
			}
			for _, table := range []string{dt.Facts.TableName(dt), dt.Facts.LineTableName(dt)} {
				if !hasSequenceIndex(t, database.DB, dialect, tc.driver, table) {
					t.Fatalf("%s has no index on %s after ensure", table, sequenceColumn)
				}
			}
		})
	}
}

// hasSequenceIndex probes the system catalog of the driver for an index on the sequence column of table.
func hasSequenceIndex(t *testing.T, conn *sql.DB, dialect db.Dialect, driver, table string) bool {
	t.Helper()
	var query string
	switch driver {
	case "mysql":
		query = "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?"
	default:
		query = "SELECT COUNT(*) FROM pg_indexes WHERE tablename = ? AND indexdef LIKE ?"
	}
	args := []any{table, sequenceColumn}
	if driver != "mysql" {
		args[1] = "%" + sequenceColumn + "%"
	}
	var n int
	if err := conn.QueryRowContext(context.Background(), db.Rebind(dialect, query), args...).Scan(&n); err != nil {
		t.Fatalf("probe index on %s: %v", table, err)
	}
	return n > 0
}
