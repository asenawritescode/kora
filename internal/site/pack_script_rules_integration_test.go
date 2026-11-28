//go:build integration

package site

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// scriptDialects lists the two databases every script-ownership rule must pass on.
var scriptDialects = []struct{ driver, env string }{
	{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
	{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
}

// forEachScriptDialect runs fn on a fresh, bootstrapped database of each dialect.
func forEachScriptDialect(t *testing.T, fn func(t *testing.T, conn *sql.DB, dbName string, dialect kdb.Dialect)) {
	t.Helper()
	for _, tc := range scriptDialects {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			dialect := kdb.Resolve(tc.driver)
			conn, name := posDB(t, tc.driver, dsn)
			if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			fn(t, conn, name, dialect)
		})
	}
}

// insertLegacyScript writes an unowned or owned row as an earlier install left it.
func insertLegacyScript(t *testing.T, conn *sql.DB, dialect kdb.Dialect, site, name, owner, docType, event, body string) {
	t.Helper()
	if _, err := conn.Exec(kdb.Rebind(dialect, `INSERT INTO _kora_script (name, site, script_type, doctype, event, script, is_active, owner_pack) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		name, site, "doc_event", docType, event, body, true, owner); err != nil {
		t.Fatal(err)
	}
}

// activeHooks counts active rows bound to docType and event, per owner.
func activeHooks(t *testing.T, conn *sql.DB, dialect kdb.Dialect, site, docType, event string) map[string]int {
	t.Helper()
	rows, err := conn.Query(kdb.Rebind(dialect, `SELECT owner_pack FROM _kora_script WHERE site = ? AND doctype = ? AND event = ? AND is_active = ?`), site, docType, event, true)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			t.Fatal(err)
		}
		out[owner]++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sha(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

const etimsDoctype = "name: eTIMS Invoice\nmodule: Accounts\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"

// Legacy row name sorts after the namespaced name; exactly one hook stays active.
func TestPackScriptLegacyRowSortingAfterNamespacedRow(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "order-owner.example.test"
		insertLegacyScript(t, conn, dialect, site, "eTIMS_Invoice_before_save", "", "eTIMS Invoice", "before_save", "return true;")
		files := []PackFile{
			{Path: "doctypes/etims_invoice.yaml", ContentType: "doctype", Content: etimsDoctype},
			{Path: "scripts/eTIMS Invoice/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "accounts", files)
		got := scriptRows(t, conn, dialect, site)
		if got["accounts__eTIMS_Invoice_before_save"] != "accounts" || got["eTIMS_Invoice_before_save"] != "(inactive) accounts" {
			t.Fatalf("rows = %v, want namespaced row active and legacy row inactive", got)
		}
		if hooks := activeHooks(t, conn, dialect, site, "eTIMS Invoice", "before_save"); len(hooks) != 1 || hooks["accounts"] != 1 {
			t.Fatalf("active hooks = %v, want exactly one owned by accounts", hooks)
		}
	})
}

// A snapshot written before ownership (empty OwnerPack) keeps the owner the database
// already records, and its unnamespaced hook loses to the owned one.
func TestPackScriptPreOwnershipSnapshotKeepsOwner(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "stale-owner.example.test"
		files := []PackFile{
			{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: salesDoctype},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "core-pack", files)
		stale := &doctype.ScriptSnapshot{Name: "Sale_before_save", ScriptType: "doc_event", DocType: "Sale", Event: "before_save", IsActive: true, Source: "return false;", ScriptHash: sha("return false;")}
		if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "stale-draft", &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Sale"}}, Scripts: []*doctype.ScriptSnapshot{stale}}, dialect, nil); err != nil {
			t.Fatalf("pre-ownership snapshot: %v", err)
		}
		got := scriptRows(t, conn, dialect, site)
		if got["core-pack__Sale_before_save"] != "core-pack" || got["Sale_before_save"] != "(inactive) " {
			t.Fatalf("rows after stale snapshot = %v, want one active hook owned by core-pack", got)
		}
		owned := &doctype.ScriptSnapshot{Name: "core-pack__Sale_before_save", DocType: "Sale", Event: "before_save", ScriptType: "doc_event", IsActive: true, Source: "return true;", ScriptHash: sha("return true;")}
		if err := ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "unowned-copy", &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Sale"}}, Scripts: []*doctype.ScriptSnapshot{owned}}, dialect, nil); err != nil {
			t.Fatalf("owned name with empty OwnerPack: %v", err)
		}
		if got := scriptRows(t, conn, dialect, site); got["core-pack__Sale_before_save"] != "core-pack" {
			t.Fatalf("owner changed by empty snapshot owner: %v", got)
		}
	})
}

// A pack binding a DocType that another pack defines replaces its own legacy row.
func TestPackScriptRequiredPackReplacesOwnLegacyRow(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "beta-owner.example.test"
		alpha := []PackFile{
			{Path: "pack.yaml", ContentType: "pack", Content: "name: alpha\nversion: \"1\"\n"},
			{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: "name: Sale\nmodule: Sales\nmodule_owner_id: alpha.core\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "alpha", alpha)
		insertLegacyScript(t, conn, dialect, site, "Sale_before_save", "beta", "Sale", "before_save", "return true;")
		beta := []PackFile{
			{Path: "pack.yaml", ContentType: "pack", Content: "name: beta\nversion: \"1\"\nrequires:\n  - package_id: alpha\n    version: \"1\"\n"},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "beta", beta)
		got := scriptRows(t, conn, dialect, site)
		if got["beta__Sale_before_save"] != "beta" || got["Sale_before_save"] != "(inactive) beta" {
			t.Fatalf("rows = %v, want beta's hook active and its legacy row retired", got)
		}
	})
}

// Upgrading an installed site must not attribute its unnamespaced rows to a pack.
func TestPackScriptUpgradeLeavesLegacyRowsUnowned(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "no-guess-owner.example.test"
		importPackSnapshot(t, conn, dialect, name, site, "alpha", alphaFiles(true))
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
		if owner, ok := got["Alpha_Item_before_save"]; !ok || owner != "" {
			t.Fatalf("legacy row owner = %q (present %v), want active and unowned: %v", owner, ok, got)
		}
		if owner, ok := got["lib_books"]; !ok || owner != "" {
			t.Fatalf("lib_books owner = %q (present %v), want active and unowned: %v", owner, ok, got)
		}
	})
}

// An unowned row is adopted by an owned import only when its content matches the script.
func TestPackScriptUnownedRowAdoptionNeedsMatchingContent(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "adopt-owner.example.test"
		insertLegacyScript(t, conn, dialect, site, "lib_books", "", "", "", "function post() { return 0; }")
		snapshot, bodies, err := ParseConfigPack("alpha", alphaFiles(true))
		if err != nil {
			t.Fatal(err)
		}
		err = ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "alpha-adopt", snapshot, dialect, bodies)
		if err == nil || !strings.Contains(err.Error(), "pack.script_unowned_mismatch") {
			t.Fatalf("import over mismatching unowned row: %v, want pack.script_unowned_mismatch", err)
		}
		if got := scriptRows(t, conn, dialect, site); got["lib_books"] != "" {
			t.Fatalf("mismatching unowned row was adopted: %v", got)
		}
	})
}

// Re-importing two packs after an upgrade adopts the matching library and leaves one
// active hook per pack.
func TestPackScriptTwoPacksReimportAfterUpgrade(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "two-packs-owner.example.test"
		insertLegacyScript(t, conn, dialect, site, "lib_books", "", "", "", "function post() {}")
		insertLegacyScript(t, conn, dialect, site, "Sale_before_save", "", "Sale", "before_save", "return true;")
		alpha := []PackFile{
			{Path: "pack.yaml", ContentType: "pack", Content: "name: alpha\nversion: \"1\"\n"},
			{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: "name: Sale\nmodule: Sales\nmodule_owner_id: alpha.core\nfields:\n  - fieldname: total\n    fieldtype: Currency\n"},
			{Path: "scripts/lib/books.js", ContentType: "script", Content: "function post() {}"},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "alpha", alpha)
		beta := []PackFile{
			{Path: "pack.yaml", ContentType: "pack", Content: "name: beta\nversion: \"1\"\nrequires:\n  - package_id: alpha\n    version: \"1\"\n"},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "beta", beta)
		got := scriptRows(t, conn, dialect, site)
		if got["lib_books"] != "alpha" || got["alpha__Sale_before_save"] != "alpha" || got["beta__Sale_before_save"] != "beta" {
			t.Fatalf("rows = %v, want lib_books owned by alpha and one hook per pack", got)
		}
		if hooks := activeHooks(t, conn, dialect, site, "Sale", "before_save"); len(hooks) != 2 || hooks["alpha"] != 1 || hooks["beta"] != 1 {
			t.Fatalf("active Sale hooks = %v, want one per pack", hooks)
		}
	})
}

// An unowned legacy row named by the pack's document-event hook (empty stored body, as
// the import path leaves it) is adopted by the importing pack, and retired behind the
// namespaced hook.
func TestPackScriptLegacyRowAdoptedByName(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "adopt-name.example.test"
		insertLegacyScript(t, conn, dialect, site, "Sale_before_save", "", "Sale", "before_save", "")
		files := []PackFile{
			{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: salesDoctype},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		importPackSnapshot(t, conn, dialect, name, site, "erpsell", files)
		got := scriptRows(t, conn, dialect, site)
		if got["Sale_before_save"] != "(inactive) erpsell" || got["erpsell__Sale_before_save"] != "erpsell" {
			t.Fatalf("rows = %v, want legacy row adopted by erpsell and inactive, namespaced hook active", got)
		}
	})
}

// An unowned library row with an empty stored body is adopted by the pack that ships it.
func TestPackScriptEmptyLibraryRowAdopted(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "adopt-empty-lib.example.test"
		insertLegacyScript(t, conn, dialect, site, "lib_books", "", "", "", "")
		importPackSnapshot(t, conn, dialect, name, site, "alpha", alphaFiles(true))
		if got := scriptRows(t, conn, dialect, site); got["lib_books"] != "alpha" {
			t.Fatalf("lib_books owner = %q, want alpha: %v", got["lib_books"], got)
		}
	})
}

// A legacy row whose stored body differs from the pack's script is never adopted: the
// import is refused at planning and the row is left as it was.
func TestPackScriptLegacyRowMismatchRefused(t *testing.T) {
	forEachScriptDialect(t, func(t *testing.T, conn *sql.DB, name string, dialect kdb.Dialect) {
		const site = "mismatch-name.example.test"
		insertLegacyScript(t, conn, dialect, site, "Sale_before_save", "", "Sale", "before_save", "return 1;")
		files := []PackFile{
			{Path: "doctypes/sale.yaml", ContentType: "doctype", Content: salesDoctype},
			{Path: "scripts/Sale/before_save.js", ContentType: "script", Content: "return true;"},
		}
		snapshot, bodies, err := ParseConfigPack("erpsell", files)
		if err != nil {
			t.Fatal(err)
		}
		err = ImportConfigFromSnapshotForTenant(conn, doctype.NewRegistry(), name, site, site, "erpsell-mismatch", snapshot, dialect, bodies)
		if err == nil || !strings.Contains(err.Error(), "pack.script_unowned_mismatch") || !strings.Contains(err.Error(), "Sale_before_save") {
			t.Fatalf("import over mismatching legacy row: %v, want pack.script_unowned_mismatch naming the row", err)
		}
		if got := scriptRows(t, conn, dialect, site); len(got) != 1 || got["Sale_before_save"] != "" {
			t.Fatalf("rows after refusal = %v, want only the untouched legacy row", got)
		}
	})
}
