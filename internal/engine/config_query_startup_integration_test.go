//go:build integration

package engine

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// Importing a pack publishes its queries in the generated <pack>.core module
// descriptor, so they are no longer unpublished right after an import. A tenant
// whose module snapshot lacks those descriptors (here: the snapshot row is
// removed after the import, as when a descriptor is withdrawn) still has config
// queries without published module queries: it must start and only warn.
func TestTenantStartSkipsUnpublishedConfigQueries(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_ENGINE_STARTUP_MYSQL_DSN"},
		{"postgres", "KORA_ENGINE_STARTUP_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			fixture := disposabledb.Open(t, tc.driver, dsn)
			conn, name := fixture.DB, fixture.Name
			dialect := kdb.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			const siteName = "start-e2e.example.test"
			reg := doctype.NewRegistry()
			if err := site.ImportConfig(conn, reg, name, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatalf("import pos: %v", err)
			}
			if _, err := conn.Exec(`DELETE FROM _kora_module_snapshot`); err != nil {
				t.Fatalf("withdraw published module descriptors: %v", err)
			}
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			install, err := NewTenantServiceInstaller(TenantServiceConfig{
				Common:         &site.CommonConfig{DBType: tc.driver},
				ResolveStorage: func(*site.Config) (storage.Backend, error) { return &testStorageBackend{}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime := &knet.LoadedSite{SiteID: siteName, Name: siteName, DB: conn, Registry: reg, SessionManager: auth.NewOwnedSessionManager(conn)}
			if err := install(site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: tc.driver}, runtime); err != nil {
				t.Fatalf("tenant start failed: %v", err)
			}
			if !strings.Contains(logs.String(), "skipping config query") || !strings.Contains(logs.String(), "query.unpublished") {
				t.Fatalf("no unpublished-query warning in logs:\n%s", logs.String())
			}
		})
	}
}
