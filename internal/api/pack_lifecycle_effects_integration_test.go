//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// A document-event script writes other records as the system's effect: the Clerk
// may save an Effect Source but only read an Effect Target and an Effect Log. The
// Source's before_save script updates, creates or deletes targets and edits the log.
// The Clerk's direct writes to the target are refused, and the script's delete and
// append-only edit roll the Source save back.
func TestPackScriptsLifecycleEffects(t *testing.T) {
	pack := writeLifecycleEffectPack(t)
	eachPackDialect(t, []string{pack}, func(admin *packFixture) {
		clerk := admin.as("Clerk")
		target := admin.create("Effect Target", `{"label":"first"}`)
		logRow := admin.create("Effect Log", `{"entry":"original"}`)
		scoped := func(name string, step func(t *testing.T, admin, clerk *packFixture)) {
			admin.t.Run(name, func(t *testing.T) {
				a, c := *admin, *clerk
				a.t, c.t = t, t
				step(t, &a, &c)
			})
		}

		scoped("update of read-only target from script", func(t *testing.T, _, clerk *packFixture) {
			clerk.create("Effect Source", `{"target":"`+target+`","note":"hit"}`)
			clerk.expect("Effect Target", target, "hits", "1")
		})

		scoped("create of target from script keeps the clerk as owner and modifier", func(t *testing.T, admin, clerk *packFixture) {
			source := clerk.create("Effect Source", `{"target":"`+target+`","note":"create"}`)
			if got := admin.count("tabEffect Target", "label = ?", "made by script"); got != 1 {
				t.Fatalf("script-created targets = %d, want 1", got)
			}
			var scriptTarget string
			query := kdb.Rebind(kdb.Resolve(admin.dialect), "SELECT name FROM "+admin.quote("tabEffect Target")+" WHERE label = ?")
			if err := admin.db.QueryRow(query, "made by script").Scan(&scriptTarget); err != nil {
				t.Fatalf("find script-created target: %v", err)
			}
			sourceBy := admin.value("Effect Source", source, "modified_by")
			if sourceBy == "" || sourceBy == "system" {
				t.Fatalf("source modified_by = %q, want the clerk's user", sourceBy)
			}
			admin.expect("Effect Target", scriptTarget, "modified_by", sourceBy)
			admin.expect("Effect Target", scriptTarget, "owner", sourceBy)
		})

		scoped("direct save of target refused for clerk", func(t *testing.T, _, clerk *packFixture) {
			clerk.refused(httpMethodPut, "Effect Target", target, `{"label":"direct"}`, "not permitted")
			clerk.refused(httpMethodPost, "Effect Target", "", `{"label":"direct"}`, "not permitted")
		})

		scoped("delete from script refused and source save rolled back", func(t *testing.T, admin, clerk *packFixture) {
			clerk.refused(httpMethodPost, "Effect Source", "", `{"target":"`+target+`","note":"delete"}`, "not permitted")
			if got := admin.count("tabEffect Source", "note = ?", "delete"); got != 0 {
				t.Fatalf("sources saved with delete note = %d, want 0", got)
			}
			admin.expect("Effect Target", target, "label", "first")
		})

		scoped("append-only log edit from script refused", func(t *testing.T, admin, clerk *packFixture) {
			clerk.refused(httpMethodPost, "Effect Source", "", `{"target":"`+target+`","log":"`+logRow+`","note":"log-edit"}`, "append-only")
			admin.expect("Effect Log", logRow, "entry", "original")
			if got := admin.count("tabEffect Source", "note = ?", "log-edit"); got != 0 {
				t.Fatalf("sources saved with log-edit note = %d, want 0", got)
			}
		})
	})
}

// writeLifecycleEffectPack writes the tiny pack this test loads: two record types with a
// lifecycle script on the source, an append-only log, and the Clerk's permissions.
func writeLifecycleEffectPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/effect-source.yaml": `name: Effect Source
module: Effects
fields:
  - fieldname: target
    fieldtype: Link
    label: Target
    options: Effect Target
  - fieldname: log
    fieldtype: Link
    label: Log
    options: Effect Log
  - fieldname: note
    fieldtype: Data
    label: Note
`,
		"doctypes/effect-target.yaml": `name: Effect Target
module: Effects
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: hits
    fieldtype: Int
    label: Hits
    read_only: true
`,
		"doctypes/effect-log.yaml": `name: Effect Log
module: Effects
append_only: true
fields:
  - fieldname: entry
    fieldtype: Data
    label: Entry
`,
		"roles.yaml": `- name: Clerk
  workspace_access: true
`,
		"permissions.yaml": `- doctype: Effect Source
  role: Clerk
  read: true
  write: true
  create: true
  delete: false
- doctype: Effect Target
  role: Clerk
  read: true
  write: false
  create: false
  delete: false
- doctype: Effect Log
  role: Clerk
  read: true
  write: false
  create: true
  delete: false
`,
		"scripts/Effect Source/before_save.js": `var doc = __kora_event__.doc;
if (doc.note === "delete") {
  kora.deleteDoc("Effect Target", doc.target);
} else if (doc.note === "create") {
  kora.createDoc("Effect Target", {label: "made by script"});
} else if (doc.note === "log-edit") {
  kora.saveDoc("Effect Log", {name: doc.log, entry: "edited by script"});
} else {
  var current = kora.getDoc("Effect Target", doc.target);
  kora.saveDoc("Effect Target", {name: doc.target, hits: (current.hits || 0) + 1});
}
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
