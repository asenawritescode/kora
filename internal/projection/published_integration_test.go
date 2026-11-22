//go:build integration

package projection

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// openDisposable creates an isolated MySQL database or PostgreSQL schema from a
// loopback root DSN and drops it in Cleanup. It skips when the DSN is unset.
func openDisposable(t *testing.T, driver, envName string) (*sql.DB, db.Dialect) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(envName))
	if dsn == "" {
		t.Skipf("set %s to a disposable loopback %s service", envName, driver)
	}
	name := fmt.Sprintf("kora_projection_%x", time.Now().UnixNano())
	if driver == "mysql" {
		rootConfig, err := mysql.ParseDSN(dsn)
		if err != nil || rootConfig.Net != "tcp" {
			t.Fatal("disposable MySQL test requires a TCP DSN")
		}
		host, _, err := net.SplitHostPort(rootConfig.Addr)
		if err != nil {
			t.Fatal("parse disposable MySQL address:", err)
		}
		if !strings.EqualFold(host, "localhost") {
			if address, parseErr := netip.ParseAddr(strings.Trim(host, "[]")); parseErr != nil || !address.IsLoopback() {
				t.Fatal("refusing projection integration test against non-loopback MySQL")
			}
		}
		root, err := sql.Open("mysql", dsn)
		if err != nil {
			t.Fatal("open disposable MySQL:", err)
		}
		if err = root.Ping(); err != nil {
			_ = root.Close()
			t.Skipf("disposable MySQL unavailable: %v", err)
		}
		if _, err = root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			_ = root.Close()
			t.Fatal("create isolated MySQL database:", err)
		}
		t.Cleanup(func() {
			_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
			_ = root.Close()
		})
		config := *rootConfig
		config.DBName = name
		database, err := sql.Open("mysql", config.FormatDSN())
		if err != nil {
			t.Fatal("open isolated MySQL database:", err)
		}
		t.Cleanup(func() { _ = database.Close() })
		return database, db.Resolve("mysql")
	}
	rootURL, err := url.Parse(dsn)
	if err != nil || rootURL.Scheme != "postgres" && rootURL.Scheme != "postgresql" {
		t.Fatal("disposable PostgreSQL test requires a postgres:// DSN")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	if err = root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("disposable PostgreSQL unavailable: %v", err)
	}
	if _, err = root.Exec(`CREATE SCHEMA "` + name + `"`); err != nil {
		_ = root.Close()
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(`DROP SCHEMA "` + name + `" CASCADE`)
		_ = root.Close()
	})
	schemaURL := *rootURL
	query := schemaURL.Query()
	query.Set("search_path", name)
	schemaURL.RawQuery = query.Encode()
	database, err := sql.Open("postgres", schemaURL.String())
	if err != nil {
		t.Fatal("open isolated PostgreSQL schema:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, db.Resolve("postgres")
}

func TestPublishedProjectionRoundTripAcrossDialects(t *testing.T) {
	cases := []struct {
		name    string
		driver  string
		envName string
	}{
		{name: "mysql", driver: "mysql", envName: "KORA_ANALYTICS_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", driver: "postgres", envName: "KORA_PROJECTION_DISPOSABLE_POSTGRES_DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, dialect := openDisposable(t, tc.driver, tc.envName)
			ctx := context.Background()
			if err := EnsureSchema(ctx, database, dialect); err != nil {
				t.Fatal("ensure projection schema:", err)
			}
			if err := EnsureSchema(ctx, database, dialect); err != nil {
				t.Fatal("ensure projection schema twice:", err)
			}
			definition := contract.ProjectionDefinition{ID: "task_snapshot", OwnerModuleID: "analytics.task", ScopePolicy: contract.ScopeTenantShared, FactKey: "id", AllowedFields: []string{"id"}, Sources: []contract.ProjectionSource{{OwnerModuleID: "task.domain", QueryName: "tasks", OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}}}, OutputSchema: contract.SchemaRef{ID: "task.output", Version: 1}, RebuildStrategy: "consistent_snapshot"}
			scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
			store := Store{DB: database, Dialect: dialect, Site: "tenant-a"}
			snapshot := Snapshot{Definition: definition, Scope: scope, SourceID: "published_sources", SourceRevision: "task-r1", Cursor: `{"task.domain/tasks":"cursor-1"}`, AsOf: time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh, Data: []byte(`[{"id":"task-1"}]`)}
			if err := store.Save(ctx, snapshot); err != nil {
				t.Fatal("save projection snapshot:", err)
			}
			if err := store.MarkStale(ctx, definition, scope, "published_sources"); err != nil {
				t.Fatal("mark projection stale:", err)
			}
			loaded, err := store.Load(ctx, definition, scope, "published_sources")
			if err != nil || string(loaded.Data) != string(snapshot.Data) || loaded.Cursor != snapshot.Cursor || loaded.Freshness != contract.QueryStale {
				t.Fatalf("retained projection = %#v, error=%v", loaded, err)
			}
			next := snapshot
			next.SourceRevision = "task-r2"
			next.Data = []byte(`[{"id":"task-2"}]`)
			next.Freshness = contract.QueryFresh
			if err = store.Save(ctx, next); err != nil {
				t.Fatal("save second projection revision:", err)
			}
			latest, err := store.Load(ctx, definition, scope, "published_sources")
			if err != nil || latest.SourceRevision != "task-r2" || string(latest.Data) != string(next.Data) {
				t.Fatalf("latest projection = %#v, error=%v", latest, err)
			}
		})
	}
}
