//go:build integration

package db

import (
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestRetiredAnalyticsTablesDropIdempotentlyKeepingProjectionStorage creates
// the retired analytics tables and the two projection tables on disposable
// MySQL and PostgreSQL, runs the drop twice, and expects only the projection
// tables to survive.
func TestRetiredAnalyticsTablesDropIdempotentlyKeepingProjectionStorage(t *testing.T) {
	cases := []struct {
		name    string
		driver  string
		env     string
		dialect Dialect
	}{
		{"mysql", "mysql", "KORA_DB_DISPOSABLE_MYSQL_DSN", &MySQLDialect{}},
		{"postgres", "postgres", "KORA_DB_DISPOSABLE_POSTGRES_DSN", &PostgresDialect{}},
	}
	kept := []string{"_kora_analytics_rebuild_lock", "_kora_analytics_module_projection"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.name)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			for _, table := range append(append([]string{}, retiredAnalyticsTables...), kept...) {
				if _, err := database.DB.Exec("CREATE TABLE " + tc.dialect.QuoteIdent(table) + " (id INT NOT NULL)"); err != nil {
					t.Fatalf("create %s: %v", table, err)
				}
			}
			for pass := 1; pass <= 2; pass++ {
				for _, stmt := range RetiredAnalyticsDropDDL(tc.dialect) {
					if _, err := database.DB.Exec(stmt); err != nil {
						t.Fatalf("drop pass %d: %s: %v", pass, stmt, err)
					}
				}
			}
			for _, table := range retiredAnalyticsTables {
				_, err := database.DB.Exec("SELECT 1 FROM " + tc.dialect.QuoteIdent(table) + " LIMIT 1")
				if err == nil || !strings.Contains(strings.ToLower(err.Error()), "exist") {
					t.Fatalf("retired table %s survived the drop: %v", table, err)
				}
			}
			for _, table := range kept {
				if _, err := database.DB.Exec("SELECT 1 FROM " + tc.dialect.QuoteIdent(table) + " LIMIT 1"); err != nil {
					t.Fatalf("projection table %s was dropped: %v", table, err)
				}
			}
		})
	}
}
