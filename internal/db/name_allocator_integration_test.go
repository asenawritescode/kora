//go:build integration

package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/asenawritescode/kora/internal/disposabledb"
)

// TestNameAllocationIssuesUniqueNamesUnderConcurrency runs parallel creates on
// a fresh counter and expects each one to get its own PREFIX-NNNN name.
func TestNameAllocationIssuesUniqueNamesUnderConcurrency(t *testing.T) {
	cases := []struct {
		name    string
		driver  string
		env     string
		dialect Dialect
	}{
		{"mysql", "mysql", "KORA_DB_DISPOSABLE_MYSQL_DSN", &MySQLDialect{}},
		{"postgres", "postgres", "KORA_DB_DISPOSABLE_POSTGRES_DSN", &PostgresDialect{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.name)
			}
			database := disposabledb.Open(t, tc.driver, dsn)
			var counter []string
			for _, stmt := range tc.dialect.SystemTableSQL() {
				if strings.Contains(stmt, "_kora_naming_series") {
					counter = append(counter, stmt)
				}
			}
			if err := tc.dialect.ExecuteBatch(database.DB, counter); err != nil {
				t.Fatal("create naming counter table:", err)
			}
			if _, err := database.DB.Exec(fmt.Sprintf("CREATE TABLE %s (name VARCHAR(140) PRIMARY KEY)", tc.dialect.QuoteIdent("tabTask"))); err != nil {
				t.Fatal("create record table:", err)
			}

			const creates = 20
			names := make([]string, creates)
			errs := make([]error, creates)
			var wg sync.WaitGroup
			for i := range creates {
				wg.Add(1)
				go func() {
					defer wg.Done()
					names[i], errs[i] = createNamedRecord(database.DB, tc.dialect)
				}()
			}
			wg.Wait()

			seen := map[string]bool{}
			for i, name := range names {
				if errs[i] != nil {
					t.Fatalf("create %d: %v", i, errs[i])
				}
				if seen[name] {
					t.Fatalf("duplicate name %q issued", name)
				}
				seen[name] = true
			}
			if len(seen) != creates {
				t.Fatalf("got %d unique names, want %d", len(seen), creates)
			}
		})
	}
}

// createNamedRecord allocates the next TASK name and inserts the record in one
// transaction, the way the ORM does.
func createNamedRecord(sqlDB *sql.DB, dialect Dialect) (string, error) {
	tx, err := sqlDB.BeginTx(context.Background(), nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	n, err := dialect.AllocateNameNumber(tx, "Task", "TASK", "tabTask")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("TASK-%04d", n)
	if _, err := tx.Exec(Rebind(dialect, fmt.Sprintf("INSERT INTO %s (name) VALUES (?)", dialect.QuoteIdent("tabTask"))), name); err != nil {
		return "", err
	}
	return name, tx.Commit()
}
