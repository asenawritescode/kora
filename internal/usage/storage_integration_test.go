//go:build integration

package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

func TestStorageBytesAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_USAGE_MYSQL_DSN"},
		{"postgres", "KORA_USAGE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			dialect := db.Resolve(tc.driver)
			table := dialect.QuoteIdent(fmt.Sprintf("usage_storage_%d", time.Now().UnixNano()))
			if _, err = database.Exec("DROP TABLE IF EXISTS " + table); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = database.Exec("DROP TABLE IF EXISTS " + table) })
			if _, err = database.Exec("CREATE TABLE " + table + " (name VARCHAR(200))"); err != nil {
				t.Fatal(err)
			}
			query := db.Rebind(dialect, "INSERT INTO "+table+" (name) VALUES (?)")
			for i := range 100 {
				if _, err = database.Exec(query, fmt.Sprintf("row-%d", i)); err != nil {
					t.Fatalf("insert row: %v", err)
				}
			}

			got, err := Storage{DB: database, Dialect: dialect}.Bytes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got <= 0 {
				t.Fatalf("database size = %d bytes, want > 0", got)
			}
		})
	}
}
