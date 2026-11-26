//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/script"
)

// A pack's scripts/lib/*.js files are libraries: shared code every script of the site
// can call, loaded in name order before the script that runs. TestPackScriptsLibrary
// checks a document-event script calls a library, the libraries load in order, a
// library that throws fails the save with its name, and a second record type sees
// the same libraries.
func TestPackScriptsLibrary(t *testing.T) {
	pack := writeLibraryPack(t)
	eachPackDialect(t, []string{pack}, func(f *packFixture) {
		f.run("before_save calls a library function", func(f *packFixture) {
			name := f.create("Ledger Entry", `{"amount":12.5}`)
			f.expect("Ledger Entry", name, "posted", "base! 12.5")
		})

		f.run("libraries load in name order and both are visible", func(f *packFixture) {
			name := f.create("Ledger Entry", `{"amount":3}`)
			f.expect("Ledger Entry", name, "posted", "base! 3")
			f.expect("Ledger Entry", name, "tag", "base")
		})

		f.run("library that throws fails the save and names it", func(f *packFixture) {
			f.exec(`INSERT INTO _kora_script (name, site, script_type, doctype, event, method_path, schedule, priority, is_active, run_as, timeout_ms, script, created_by, updated_by)
				VALUES (?, ?, ?, '', '', '', '', 10, ?, '', 5000, ?, 'pack', 'pack')`,
				"lib_zz_broken", f.site, string(script.TypeLibrary), true, `throw new Error("rate table missing");`)
			f.refused(httpMethodPost, "Ledger Entry", "", `{"amount":1}`, `lib_zz_broken\" failed to load`)
			f.exec(`UPDATE _kora_script SET is_active = ? WHERE site = ? AND name = ?`, false, f.site, "lib_zz_broken")
			name := f.create("Ledger Entry", `{"amount":2}`)
			f.expect("Ledger Entry", name, "posted", "base! 2")
		})

		f.run("second document-event type sees the libraries", func(f *packFixture) {
			name := f.create("Audit Note", `{"memo":"hello"}`)
			f.expect("Audit Note", name, "memo", "base:hello")
		})
	})
}

// writeLibraryPack writes the tiny pack this test loads: two record types whose
// before_save scripts call functions defined in scripts/lib.
func writeLibraryPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/ledger-entry.yaml": `name: Ledger Entry
module: Library
fields:
  - fieldname: amount
    fieldtype: Float
    label: Amount
  - fieldname: posted
    fieldtype: Data
    label: Posted
  - fieldname: tag
    fieldtype: Data
    label: Tag
`,
		"doctypes/audit-note.yaml": `name: Audit Note
module: Library
fields:
  - fieldname: memo
    fieldtype: Data
    label: Memo
`,
		"scripts/lib/base.js": `var BASE_TAG = "base";
`,
		"scripts/lib/util.js": `function tagged(v) { return BASE_TAG + "! " + money(v); }
function money(v) { return Math.round(Number(v || 0) * 100) / 100; }
`,
		"scripts/Ledger Entry/before_save.js": `var doc = __kora_event__.doc;
doc.posted = tagged(doc.amount);
doc.tag = BASE_TAG;
return {doc: doc};
`,
		"scripts/Audit Note/before_save.js": `var doc = __kora_event__.doc;
doc.memo = BASE_TAG + ":" + doc.memo;
return {doc: doc};
`,
	}
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
