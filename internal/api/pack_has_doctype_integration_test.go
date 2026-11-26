//go:build integration

package api

import (
	"os"
	"path/filepath"
	"testing"
)

// A before_save script asks kora.hasDocType about its own record type and about a
// type that is not installed. The answers are stored as text in two Data fields.
func TestPackScriptsHasDocType(t *testing.T) {
	pack := writeHasDocTypePack(t)
	eachPackDialect(t, []string{pack}, func(f *packFixture) {
		f.run("own and unknown record types", func(f *packFixture) {
			name := f.create("Has Type Probe", `{}`)
			f.expect("Has Type Probe", name, "has_own", "true")
			f.expect("Has Type Probe", name, "has_missing", "false")
		})
	})
}

// writeHasDocTypePack writes a pack with one record type and its before_save script.
func writeHasDocTypePack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"doctypes/has-type-probe.yaml": `name: Has Type Probe
module: Probes
fields:
  - fieldname: has_own
    fieldtype: Data
    label: Has Own
  - fieldname: has_missing
    fieldtype: Data
    label: Has Missing
`,
		"scripts/Has Type Probe/before_save.js": `var doc = __kora_event__.doc;
doc.has_own = String(kora.hasDocType("Has Type Probe"));
doc.has_missing = String(kora.hasDocType("No Such Type"));
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
