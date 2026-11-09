//go:build integration

package site

import (
	"path/filepath"
	"reflect"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"os"
)

// Importing the same pack twice must be idempotent: the second import sees the
// live schema (current schema on Postgres) and creates nothing.
func TestPackReimportIsIdempotentAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(conn, dialect); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("..", "..", "config", "pos") // pulls in inventory via requires
			if err := ImportConfig(conn, doctype.NewRegistry(), name, "reimport.example.test", path, dialect); err != nil {
				t.Fatalf("first import: %v", err)
			}
			first, err := dialect.LoadSchema(conn, name)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Tables) == 0 {
				t.Fatal("introspection saw no tables after first import")
			}
			if err := ImportConfig(conn, doctype.NewRegistry(), name, "reimport.example.test", path, dialect); err != nil {
				t.Fatalf("second import must not re-create tables: %v", err)
			}
			second, err := dialect.LoadSchema(conn, name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatal("second import changed the live schema")
			}
		})
	}
}
