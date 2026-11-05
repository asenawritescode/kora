//go:build integration

package site

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// A composed POS+inventory package installs to the same modules and
// DocType tables as `kora config import` of the pack tree.
func TestComposedPackInstallMatchesImportAcrossDialects(t *testing.T) {
	pos, inv := filepath.Join("..", "config", "pos"), filepath.Join("..", "config", "inventory")
	composed, _, err := ParseConfigPack("ignored", append(readPackFiles(t, inv, "aaaaaaaaaaaaaaaa"), readPackFiles(t, pos, "bbbbbbbbbbbbbbbb")...))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			dialect := kdb.Resolve(tc.driver)
			var revs, actives [2]int
			for i, mode := range []string{"import", "install"} {
				conn, name := posDB(t, tc.driver, dsn)
				if err := BootstrapSystemTables(conn, dialect); err != nil {
					t.Fatal(err)
				}
				site := "m287.example"
				if mode == "import" {
					err = ImportConfig(conn, doctype.NewRegistry(), name, site, pos, dialect)
				} else {
					err = ImportConfigFromSnapshotContext(context.Background(), conn, doctype.NewRegistry(), name, site, site, "install", composed, dialect, nil)
				}
				if err != nil {
					t.Fatalf("%s: %v", mode, err)
				}
				rev, active := moduleRevision(t, conn, dialect, site)
				revs[i], actives[i] = int(rev), active
			}
			if actives[0] != 4 || actives[1] != 4 || revs[0] != revs[1] {
				t.Fatalf("active modules import=%d install=%d revisions %v", actives[0], actives[1], revs)
			}
		})
	}
}
