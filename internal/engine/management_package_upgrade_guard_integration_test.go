//go:build integration

package engine

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
)

// The record count uses each dialect's own quoting, including a table name with a space.
// Run against disposable servers: KORA_PACKAGE_GUARD_MYSQL_DSN and KORA_PACKAGE_GUARD_POSTGRES_DSN.
func TestCountTableRowsAcrossDialects(t *testing.T) {
	for _, c := range []struct{ driver, env string }{{"mysql", "KORA_PACKAGE_GUARD_MYSQL_DSN"}, {"postgres", "KORA_PACKAGE_GUARD_POSTGRES_DSN"}} {
		t.Run(c.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(c.env))
			if dsn == "" {
				t.Skip("set " + c.env + " to a disposable server")
			}
			database := disposabledb.Open(t, c.driver, dsn)
			dialect := db.Resolve(c.driver)
			quote := dialect.(db.SchemaDialect).QuoteIdent
			table := "tabPOS Sale"
			if _, err := database.DB.Exec("CREATE TABLE " + quote(table) + " (name VARCHAR(40) PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			count := countTableRows(database.DB, dialect)
			if got, err := count(context.Background(), table); err != nil || got != 0 {
				t.Fatalf("empty table = %d, %v, want 0", got, err)
			}
			for _, name := range []string{"PS-1", "PS-2", "PS-3"} {
				if _, err := database.DB.Exec("INSERT INTO " + quote(table) + " (name) VALUES ('" + name + "')"); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := count(context.Background(), table); err != nil || got != 3 {
				t.Fatalf("three records = %d, %v, want 3", got, err)
			}
			if _, err := count(context.Background(), "tabMissing"); err == nil {
				t.Fatal("counting a table that does not exist must fail, not report 0")
			}
		})
	}
}
