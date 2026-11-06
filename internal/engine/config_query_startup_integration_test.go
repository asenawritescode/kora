//go:build integration

package engine

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
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
			admin, err := sql.Open(tc.driver, dsn)
			if err != nil || admin.Ping() != nil {
				t.Fatalf("connect %s: %v", tc.driver, err)
			}
			name := fmt.Sprintf("kora_start_%x", time.Now().UnixNano())
			target := dsn
			if tc.driver == "postgres" {
				admin.Exec(`CREATE SCHEMA "` + name + `"`)
				t.Cleanup(func() { admin.Exec(`DROP SCHEMA "` + name + `" CASCADE`); admin.Close() })
				u, _ := url.Parse(dsn)
				q := u.Query()
				q.Set("search_path", name)
				u.RawQuery = q.Encode()
				target = u.String()
			} else {
				admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
				t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
				target = strings.Replace(dsn, "/?", "/"+name+"?", 1)
			}
			conn, err := sql.Open(tc.driver, target)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			dialect := kdb.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(conn, dialect); err != nil {
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
				ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return &testStorageBackend{}, nil },
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
