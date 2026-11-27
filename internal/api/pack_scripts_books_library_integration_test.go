//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// The erp-accounting pack ships scripts/lib/books.js (installed as lib_books): the one
// shared posting block the selling, stock and purchasing packs call. This test proves the
// library mechanism carries the real block before any pack script depends on it. A tiny
// probe pack's before_save calls postEntry(...) when the books are installed; with the
// books present one balanced Posted entry is written (and only one), with the standard
// accounts auto-created; with the books absent the same save succeeds and posts nothing.

// TestPackScriptsBooksLibrary runs with erp-accounting loaded: the lib_books library is
// present, so the probe's guarded postEntry call writes to the books.
func TestPackScriptsBooksLibrary(t *testing.T) {
	probe := writeBooksProbePack(t)
	packs := []string{"../../config/_parked/erpsell", "../../config/_parked/erp-accounting", probe}
	eachPackDialect(t, packs, func(f *packFixture) {
		f.run("a save posts one balanced entry with the standard accounts", func(f *packFixture) {
			name := f.create("Posting Probe", `{"memo":"first"}`)
			if got := f.count("tabJournal Entry", "source_reference = ? AND status = ?", name, "Posted"); got != 1 {
				f.t.Fatalf("Posted journal entries for %s = %d, want 1", name, got)
			}
			if got := f.count("tabJournal Entry__lines", ""); got != 2 {
				f.t.Fatalf("journal lines = %d, want 2 (one Cash debit, one Sales credit)", got)
			}
			if got := f.count("tabAccount", "code = ? AND posting_role = ? AND status = ?", "1000", "Cash", "Active"); got != 1 {
				f.t.Fatalf("active Cash account 1000 = %d, want 1 (auto-created)", got)
			}
			if got := f.count("tabAccount", "code = ? AND posting_role = ? AND status = ?", "4000", "Sales", "Active"); got != 1 {
				f.t.Fatalf("active Sales account 4000 = %d, want 1 (auto-created)", got)
			}
		})

		f.run("re-saving the source posts nothing more", func(f *packFixture) {
			name := f.create("Posting Probe", `{"memo":"second"}`)
			if got := f.count("tabJournal Entry", "source_reference = ? AND status = ?", name, "Posted"); got != 1 {
				f.t.Fatalf("Posted entries after create for %s = %d, want 1", name, got)
			}
			accounts := f.count("tabAccount", "")
			f.update("Posting Probe", name, `{"memo":"second edited"}`)
			if got := f.count("tabJournal Entry", "source_reference = ? AND status = ?", name, "Posted"); got != 1 {
				f.t.Fatalf("Posted entries after re-save for %s = %d, want 1 (idempotent)", name, got)
			}
			if got := f.count("tabAccount", ""); got != accounts {
				f.t.Fatalf("accounts after re-save = %d, want %d (no new accounts)", got, accounts)
			}
		})
	})
}

// TestPackScriptsBooksLibraryWithoutBooks runs without erp-accounting: lib_books is absent,
// so hasDocType("Journal Entry") is false and postEntry is never referenced. The save still
// succeeds. (A separate top-level test keeps the dialect the first subtest level, which the
// CI filter -run 'TestPackScripts/postgres' depends on.)
func TestPackScriptsBooksLibraryWithoutBooks(t *testing.T) {
	probe := writeBooksProbePack(t)
	packs := []string{"../../config/_parked/erpsell", probe}
	eachPackDialect(t, packs, func(f *packFixture) {
		f.run("the save succeeds and posts nothing when the books are absent", func(f *packFixture) {
			name := f.create("Posting Probe", `{"memo":"no books"}`)
			f.expect("Posting Probe", name, "memo", "no books")
		})
	})
}

// writeBooksProbePack writes a one-doctype pack whose before_save posts to the books through
// the lib_books library when the books are installed.
func writeBooksProbePack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/posting-probe.yaml": `name: Posting Probe
module: Books Probe
fields:
  - fieldname: memo
    fieldtype: Data
    label: Memo
`,
		"scripts/Posting Probe/before_save.js": `var doc = __kora_event__.doc;
if (kora.hasDocType("Journal Entry")) {
  postEntry("Posting Probe", doc.name, "2026-11-26", "Posting Probe", [
    { role: "Cash", debit: 100, credit: 0 },
    { role: "Sales", debit: 0, credit: 100 }
  ]);
}
return { doc: doc };
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
