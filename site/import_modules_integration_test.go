//go:build integration

package site

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

func TestImportConfigCommitsPackModulesAtomicallyAcrossDialects(t *testing.T) {
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
			if tc.driver == "postgres" {
				// Postgres schema introspection reads the public schema only
				// (LoadSchema), so use the disposable server's public schema.
				var err error
				if conn, err = sql.Open("postgres", dsn); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				if _, err := conn.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
					t.Fatal(err)
				}
			}
			runImportModules(t, tc.driver, conn, name)
		})
	}
}
