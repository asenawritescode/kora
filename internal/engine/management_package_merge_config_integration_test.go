//go:build integration

package engine

import (
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
)

// A package carrying commands, queries and print formats must still have them
// in the persisted tenant snapshot after the merge and activation.
func TestMergedPackageKeepsCommandsQueriesAndPrintFormatsAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, info, cleanup := openManagementPackageTestDatabase(t, tc.driver, dsn)
			defer cleanup()
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(conn, dialect); err != nil {
				t.Fatal(err)
			}
			pkg := configSnapshot(t, nil, nil, nil)
			pkg.Commands = append(pkg.Commands, cmdRaw("shop", "sell", 1))
			pkg.Queries = append(pkg.Queries, queryRaw(t, "shop.core", "items", "name"))
			pkg.PrintFormats = append(pkg.PrintFormats, printRaw("receipt", "r"))
			merged, err := mergeManagementPackageSnapshot(configSnapshot(t, nil, nil, nil), pkg)
			if err != nil {
				t.Fatal(err)
			}
			if err := site.ImportConfigFromSnapshotContext(t.Context(), conn, doctype.NewRegistry(), info.DBName, "t288.example", "t288.example", "install", merged, dialect, nil); err != nil {
				t.Fatal(err)
			}
			got, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot("t288.example")
			if err != nil {
				t.Fatal(err)
			}
			c, q, p := rawKeys(t, got)
			if strings.Join(c, ",") != "shop.sell@1" || strings.Join(q, ",") != "shop.core/items@name" || strings.Join(p, ",") != "receipt@r" {
				t.Fatalf("persisted snapshot lost config: %v %v %v", c, q, p)
			}
		})
	}
}
