//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/query"
	"github.com/asenawritescode/kora/site"
	"github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// Importing the pos pack (requires inventory) must make its queries and print
// formats load per tenant from the persisted snapshot, and the queries must
// return the seeded rows.
func TestTenantPackQueriesPrintAndRequiresAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, dbName := isolatedDB(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			siteName := fmt.Sprintf("tenant-packs-%d.example.test", time.Now().UnixNano())

			// Missing required pack is rejected and leaves no active config.
			bad := filepath.Join(t.TempDir(), "lonely")
			if err := os.MkdirAll(bad, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bad, "pack.yaml"), []byte("name: lonely\nversion: \"1\"\nrequires:\n  - package_id: absent\n    version: \"1\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			err := site.ImportConfig(database, doctype.NewRegistry(), "x", siteName, bad, dialect)
			if err == nil || !strings.Contains(err.Error(), "pack.requires_missing") {
				t.Fatalf("missing requires: err = %v", err)
			}
			if qs, err := tenantConfigQueries(ctx, database, dialect, siteName, doctype.NewRegistry()); err != nil || len(qs) != 0 {
				t.Fatalf("rejected import left queries: %d, %v", len(qs), err)
			}

			reg := doctype.NewRegistry()
			if err := site.ImportConfig(database, reg, dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatalf("import pos+inventory: %v", err)
			}
			if reg.Get("Item") == nil || reg.Get("Sale") == nil {
				t.Fatal("required inventory pack was not imported with pos")
			}

			// Snapshot round-trip: queries and print formats survive the persisted S-expr.
			if persisted, err := tenantConfigQueries(ctx, database, dialect, siteName, reg); err != nil || len(persisted) == 0 {
				t.Fatalf("persisted queries = %d, %v", len(persisted), err)
			}
			if formats, err := tenantPrintFormats(ctx, database, dialect, siteName); err != nil || formats["sale_receipt"] == nil {
				t.Fatalf("print formats = %v, %v", formats, err)
			}
			defs, err := query.LoadDir(filepath.Join("..", "..", "config", "pos", "queries"), reg)
			if err != nil {
				t.Fatal(err)
			}
			byName := map[string]*query.Definition{}
			for _, d := range defs {
				byName[d.Name] = d
			}

			// Seed sales directly and run the pack queries.
			sale := dialect.QuoteIdent(reg.Get("Sale").RawTableName())
			till := dialect.QuoteIdent(reg.Get("Till Session").RawTableName())
			ts := time.Now().UTC().Format("2006-01-02 15:04:05")
			for i, total := range []int{100, 250} {
				_, err := database.Exec(db.Rebind(dialect, "INSERT INTO "+sale+" (name, owner, creation, modified, modified_by, doc_status, idx, register, total, status) VALUES (?,?,?,?,?,?,?,?,?,?)"),
					fmt.Sprintf("S-%d-%d", time.Now().UnixNano(), i), "Administrator", ts, ts, "Administrator", 0, 0, "REG1", total, "Paid")
				if err != nil {
					t.Fatalf("seed sale: %v", err)
				}
			}
			if _, err := database.Exec(db.Rebind(dialect, "INSERT INTO "+till+" (name, owner, creation, modified, modified_by, doc_status, idx, shift, opening_float) VALUES (?,?,?,?,?,?,?,?,?)"),
				"T-1", "Administrator", ts, ts, "Administrator", 0, 0, "SH1", 500); err != nil {
				t.Fatalf("seed till: %v", err)
			}
			run := func(name string, in map[string]any) []map[string]any {
				p, err := query.Run(ctx, database, dialect, byName[name], query.Request{FullRead: true, Input: in, Actor: "Administrator", Now: time.Now()})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return p.Rows
			}
			if rows := run("recent_sales", map[string]any{"register": "REG1"}); len(rows) != 2 {
				t.Fatalf("recent_sales rows = %v", rows)
			}
			if rows := run("recent_sales", map[string]any{"register": "OTHER"}); len(rows) != 0 {
				t.Fatalf("recent_sales other register = %v", rows)
			}
			if rows := run("open_till", map[string]any{"shift": "SH1"}); len(rows) != 1 || fmt.Sprint(rows[0]["name"]) != "T-1" {
				t.Fatalf("open_till rows = %v", rows)
			}
		})
	}
}

// isolatedDB gives the run its own MySQL database or Postgres database because
// pack imports write global per-database DocType rows.
func isolatedDB(t *testing.T, driver, dsn string) (*sql.DB, string) {
	t.Helper()
	admin, err := sql.Open(driver, dsn)
	if err != nil || admin.Ping() != nil {
		t.Fatalf("connect %s: %v", driver, err)
	}
	name := fmt.Sprintf("kora_packs_%x", time.Now().UnixNano())
	target := dsn
	if driver == "postgres" {
		if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`); admin.Close() })
		u, _ := url.Parse(dsn)
		u.Path = "/" + name
		target = u.String()
	} else {
		if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.DBName = name // DSNs naming a database must not be shared across tenants
		target = cfg.FormatDSN()
	}
	conn, err := sql.Open(driver, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, name
}
