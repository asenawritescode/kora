//go:build integration

package site

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

const salesDoctype = "name: Sale\nmodule: Sales\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"

// scriptRows lists the site's script rows as name -> owner_pack, active only when active.
func scriptRows(t *testing.T, conn *sql.DB, dialect kdb.Dialect, site string) map[string]string {
	t.Helper()
	rows, err := conn.Query(kdb.Rebind(dialect, `SELECT name, owner_pack, is_active FROM _kora_script WHERE site = ?`), site)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, owner string
		var active bool
		if err := rows.Scan(&name, &owner, &active); err != nil {
			t.Fatal(err)
		}
		if active {
			out[name] = owner
		} else {
			out[name] = "(inactive) " + owner
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPackScriptOwnershipAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			dialect := kdb.Resolve(tc.driver)
			const site = "scripts-owner.example.test"
			fresh := func(t *testing.T) (*sql.DB, string) {
				t.Helper()
				conn, name := posDB(t, tc.driver, dsn)
				if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
					t.Fatal(err)
				}
				return conn, name
			}
			lib := func(body string) []PackFile {
				return []PackFile{{Path: "scripts/lib/books.js", ContentType: "script", Content: body}}
			}

			t.Run("library owned by its pack", func(t *testing.T) {
				conn, name := fresh(t)
				if err := ImportConfigFromPackForTenant(conn, doctype.NewRegistry(), name, site, site, "books-a", lib("function post() {}"), dialect); err != nil {
					t.Fatalf("first owner: %v", err)
				}
				err := ImportConfigFromPackForTenant(conn, doctype.NewRegistry(), name, site, site, "books-c", lib("function post() { return 1; }"), dialect)
				if err == nil || !strings.Contains(err.Error(), "pack.script_owned_elsewhere") {
					t.Fatalf("second pack took lib_books: %v", err)
				}
				if got := scriptRows(t, conn, dialect, site); len(got) != 1 || got["lib_books"] != "books-a" {
					t.Fatalf("rows after refused clash = %v, want only lib_books owned by books-a", got)
				}
				// The owner refreshing its own library is an ordinary upsert.
				own := packScriptSnapshot("books-a", "scripts/lib/books.js", []byte("function post() { return 2; }"))
				own.Source = "function post() { return 2; }"
				if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "books-a-refresh", &doctype.ConfigSnapshot{Scripts: []*doctype.ScriptSnapshot{own}}, dialect, nil); err != nil {
					t.Fatalf("owner refresh: %v", err)
				}
				if got := scriptRows(t, conn, dialect, site); len(got) != 1 || got["lib_books"] != "books-a" {
					t.Fatalf("rows after owner refresh = %v", got)
				}
			})

			t.Run("two owning packs keep one active row each", func(t *testing.T) {
				conn, name := fresh(t)
				core := packScriptSnapshot("core-pack", "scripts/Sale/before_save.js", []byte("return true;"))
				ext := packScriptSnapshot("ext", "scripts/Sale/before_save.js", []byte("return false;"))
				core.Source, ext.Source = "return true;", "return false;"
				snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Sale"}}, Scripts: []*doctype.ScriptSnapshot{core, ext}}
				if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "two-owners", snapshot, dialect, nil); err != nil {
					t.Fatalf("import: %v", err)
				}
				got := scriptRows(t, conn, dialect, site)
				if got["core-pack__Sale_before_save"] != "core-pack" || got["ext__Sale_before_save"] != "ext" || len(got) != 2 {
					t.Fatalf("rows = %v, want one active row per owning pack", got)
				}
			})

			t.Run("upgrade retires the unnamespaced row", func(t *testing.T) {
				conn, name := fresh(t)
				// The row an installed site already has from the unnamespaced naming.
				if _, err := conn.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_script (name, site, script_type, doctype, event, script, is_active) VALUES (?, ?, 'doc_event', 'Sale', 'before_save', 'return false;', ?)`), "Sale_before_save", site, true); err != nil {
					t.Fatal(err)
				}
				files := []PackFile{
					{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: salesDoctype},
					{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
				}
				importPackSnapshot(t, conn, dialect, name, site, "core-pack", files)
				got := scriptRows(t, conn, dialect, site)
				if got["core-pack__Sale_before_save"] != "core-pack" {
					t.Fatalf("namespaced row = %q, want active and owned by core-pack: %v", got["core-pack__Sale_before_save"], got)
				}
				if got["Sale_before_save"] != "(inactive) " {
					t.Fatalf("legacy row = %q, want inactive and unowned: %v", got["Sale_before_save"], got)
				}
				var active int
				if err := conn.QueryRow(kdb.Rebind(dialect, `SELECT COUNT(*) FROM _kora_script WHERE site = ? AND doctype = ? AND event = ? AND is_active = ?`), site, "Sale", "before_save", true).Scan(&active); err != nil {
					t.Fatal(err)
				}
				if active != 1 {
					t.Fatalf("active Sale before_save rows = %d, want 1", active)
				}
			})
		})
	}
}

// importPackSnapshot parses one pack's files and activates them, as the Engine does.
func importPackSnapshot(t *testing.T, conn *sql.DB, dialect kdb.Dialect, dbName, site, pack string, files []PackFile) {
	t.Helper()
	snapshot, bodies, err := ParseConfigPack(pack, files)
	if err != nil {
		t.Fatalf("parse %s: %v", pack, err)
	}
	if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), dbName, site, site, "pack-"+pack, snapshot, dialect, bodies); err != nil {
		t.Fatalf("import %s: %v", pack, err)
	}
}

func alphaFiles(withScripts bool) []PackFile {
	files := []PackFile{
		{Path: "pack.yaml", ContentType: "pack", Content: "name: alpha\nversion: \"1\"\n"},
		{Path: "doctypes/alpha_item.yaml", ContentType: "doctype", Content: "name: Alpha Item\nmodule: Sales\nmodule_owner_id: alpha.core\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"},
	}
	if withScripts {
		files = append(files,
			PackFile{Path: "scripts/lib/books.js", ContentType: "script", Content: "function post() {}"},
			PackFile{Path: "scripts/Alpha Item/before_save.js", ContentType: "script", Content: "return true;"})
	}
	return files
}

func TestPackScriptInstalledClosureAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			dialect := kdb.Resolve(tc.driver)
			const site = "closure-owner.example.test"
			conn, name := posDB(t, tc.driver, dsn)
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			importPackSnapshot(t, conn, dialect, name, site, "alpha", alphaFiles(false))

			gamma := []PackFile{{Path: "scripts/Alpha Item/before_save.js", ContentType: "script", Content: "return true;"}}
			if _, _, err := ParseConfigPack("gamma", gamma); err == nil || !strings.Contains(err.Error(), "pack.script_binding_undefined") {
				t.Fatalf("pack without requires bound an installed DocType: %v", err)
			}
			beta := []PackFile{
				{Path: "pack.yaml", ContentType: "pack", Content: "name: beta\nversion: \"1\"\nrequires:\n  - package_id: alpha\n    version: \"1\"\n"},
				{Path: "scripts/Alpha Item/before_save.js", ContentType: "script", Content: "return true;"},
			}
			importPackSnapshot(t, conn, dialect, name, site, "beta", beta)
			if got := scriptRows(t, conn, dialect, site); got["beta__Alpha_Item_before_save"] != "beta" {
				t.Fatalf("beta hook rows = %v, want active and owned by beta", got)
			}
		})
	}
}

func TestPackScriptUpgradeBackfillAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			dialect := kdb.Resolve(tc.driver)
			const site = "backfill-owner.example.test"
			conn, name := posDB(t, tc.driver, dsn)
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			importPackSnapshot(t, conn, dialect, name, site, "alpha", alphaFiles(true))
			// Make the database look as a pre-38 install left it: unowned, unnamespaced rows.
			for _, q := range []string{
				`UPDATE _kora_script SET owner_pack = ''`,
				`UPDATE _kora_script SET name = 'Alpha_Item_before_save' WHERE name = 'alpha__Alpha_Item_before_save'`,
				`UPDATE _kora_system_bootstrap SET version = 37`,
			} {
				if _, err := conn.Exec(kdb.Rebind(dialect, q)); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatalf("bootstrap upgrade: %v", err)
			}
			got := scriptRows(t, conn, dialect, site)
			if got["lib_books"] != "alpha" || got["Alpha_Item_before_save"] != "alpha" {
				t.Fatalf("backfilled rows = %v, want both owned by alpha", got)
			}
			snapshot, bodies, err := ParseConfigPack("alpha", alphaFiles(true))
			if err != nil {
				t.Fatal(err)
			}
			if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "alpha-reimport", &doctype.ConfigSnapshot{Scripts: snapshot.Scripts}, dialect, bodies); err != nil {
				t.Fatalf("re-import: %v", err)
			}
			got = scriptRows(t, conn, dialect, site)
			if got["alpha__Alpha_Item_before_save"] != "alpha" || got["Alpha_Item_before_save"] != "(inactive) alpha" || got["lib_books"] != "alpha" {
				t.Fatalf("after re-import rows = %v, want one active hook owned by alpha", got)
			}
			var active int
			if err := conn.QueryRow(kdb.Rebind(dialect, `SELECT COUNT(*) FROM _kora_script WHERE site = ? AND doctype = ? AND event = ? AND is_active = ?`), site, "Alpha Item", "before_save", true).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if active != 1 {
				t.Fatalf("active Alpha Item before_save rows = %d, want 1", active)
			}
		})
	}
}
