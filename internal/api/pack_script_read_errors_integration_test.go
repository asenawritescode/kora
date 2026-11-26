//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// A before_save read that the provider rejects must fail the save, so a guard cannot
// fail open. A read of a real type with no rows is not an error and the save goes through.
func TestPackScriptsReadErrorFailsSave(t *testing.T) {
	pack := writeReadErrorPack(t)
	eachPackDialect(t, []string{pack}, func(admin *packFixture) {
		admin.run("empty real type read saves", func(f *packFixture) {
			name := f.create("Probe Source", jsonBody(f.t, map[string]string{"note": "empty"}))
			f.expect("Probe Source", name, "seen", "0")
		})

		admin.run("failed read refuses the save", func(f *packFixture) {
			f.refused(httpMethodPost, "Probe Source", "", jsonBody(f.t, map[string]string{"note": "missing"}), "kora.getList")
			if n := f.count("tabProbe Source", "note = ?", "missing"); n != 0 {
				f.t.Fatalf("refused save left %d row(s) behind", n)
			}
		})
	})
}

// writeReadErrorPack writes a pack whose Probe Source before_save reads Probe Target
// normally, or reads a type that does not exist when the note asks for it.
func writeReadErrorPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/probe-source.yaml": `name: Probe Source
module: Reads
fields:
  - fieldname: note
    fieldtype: Data
    label: Note
  - fieldname: seen
    fieldtype: Data
    label: Seen
`,
		"doctypes/probe-target.yaml": `name: Probe Target
module: Reads
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
`,
		"scripts/Probe Source/before_save.js": `var doc = __kora_event__.doc;
if (doc.note === "missing") {
  kora.getList("No Such Type", {limit: 1});
}
doc.seen = String(kora.getList("Probe Target", {limit: 10}).length);
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
