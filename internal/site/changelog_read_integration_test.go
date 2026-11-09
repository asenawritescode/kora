//go:build integration

package site

import (
	"context"
	"os"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestReadChangelogJSONColumns(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, _ := posDB(t, tc.driver, dsn)
			d := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(conn, d); err != nil {
				t.Fatal(err)
			}
			for ver, row := range []struct{ id, want string }{{"v-null", ""}, {"v-json", `{"changes":[]}`}} {
				id, want := row.id, row.want
				var cl any
				if want != "" {
					cl = want
				}
				if _, err := conn.Exec(kdb.Rebind(d, "INSERT INTO _kora_config_version (id, site, version, changelog) VALUES (?, 's', ?, ?)"), id, ver+1, cl); err != nil {
					t.Fatal(err)
				}
				got, err := readChangelog(context.Background(), conn, d, id)
				if err != nil {
					t.Fatalf("%s: %v", id, err)
				}
				if want != "" && got == "" {
					t.Fatalf("%s: empty changelog", id)
				}
				if want == "" && got != "" {
					t.Fatalf("%s: got %q", id, got)
				}
			}
		})
	}
}
