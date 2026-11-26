//go:build integration

package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// nameBeforeHooksTypes are the record types of the naming pack: default numbering, a
// date-only series, a series that reads a field, and the default again for refusals.
var nameBeforeHooksTypes = []struct{ name, series, extra string }{
	{name: "NameDefault"},
	{name: "NameDated", series: "NDT-.YYYY.-.####"},
	{name: "NameFielded", series: "NFL-.{branch}.-.###", extra: "  - fieldname: branch\n    fieldtype: Data\n"},
}

// nameBeforeHooksPack writes a tiny pack into a temp dir. Every record type has a
// before_save script that refuses mode "refuse" and copies doc.name into seen_name.
func nameBeforeHooksPack(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rt := range nameBeforeHooksTypes {
		doctypeYAML := "name: " + rt.name + "\nmodule: NameBeforeHooks\n"
		if rt.series != "" {
			doctypeYAML += "naming_series: " + rt.series + "\n"
		}
		doctypeYAML += "fields:\n" + rt.extra +
			"  - fieldname: mode\n    fieldtype: Data\n" +
			"  - fieldname: seen_name\n    fieldtype: Data\n"
		script := `var doc = __kora_event__.doc;
if (doc.mode === "refuse") { throw new Error("Refused by the pack script."); }
doc.seen_name = doc.name || "";
return { doc: doc };
`
		for path, body := range map[string]string{
			filepath.Join(root, "doctypes", rt.name+".yaml"):          doctypeYAML,
			filepath.Join(root, "scripts", rt.name, "before_save.js"): script,
		} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func nameBeforeHooksBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode %v: %v", fields, err)
	}
	return string(out)
}

var nameBeforeHooksCounterSuffix = regexp.MustCompile(`(\d+)$`)

// nameBeforeHooksCounter returns the number a generated name ends with.
func nameBeforeHooksCounter(t *testing.T, name string) int {
	t.Helper()
	m := nameBeforeHooksCounterSuffix.FindStringSubmatch(name)
	if m == nil {
		t.Fatalf("name %q has no counter", name)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("name %q counter: %v", name, err)
	}
	return n
}

func TestPackScriptsNameBeforeHooks(t *testing.T) {
	eachPackDialect(t, []string{nameBeforeHooksPack(t)}, func(f *packFixture) {
		f.run("default naming is set before before_save", func(f *packFixture) {
			name := f.create("NameDefault", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			f.expect("NameDefault", name, "seen_name", name)
			f.expect("NameDefault", name, "idx", strconv.Itoa(nameBeforeHooksCounter(f.t, name)))
		})

		f.run("date-only series is set before before_save", func(f *packFixture) {
			name := f.create("NameDated", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			if !strings.HasPrefix(name, "NDT-") {
				t.Fatalf("name %q, want the NDT- series", name)
			}
			f.expect("NameDated", name, "seen_name", name)
			f.expect("NameDated", name, "idx", strconv.Itoa(nameBeforeHooksCounter(f.t, name)))
		})

		f.run("field-token series keeps today's order", func(f *packFixture) {
			name := f.create("NameFielded", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok", "branch": "north"}))
			if !strings.HasPrefix(name, "NFL-north-") {
				t.Fatalf("name %q, want the NFL-north- series", name)
			}
			// The name depends on a field, so it is still allocated after before_save.
			f.expect("NameFielded", name, "seen_name", "")
			f.expect("NameFielded", name, "idx", strconv.Itoa(nameBeforeHooksCounter(f.t, name)))
		})

		f.run("refused default save does not consume a number", func(f *packFixture) {
			first := f.create("NameDefault", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			f.refused(httpMethodPost, "NameDefault", "", nameBeforeHooksBody(f.t, map[string]any{"mode": "refuse"}), "Refused by the pack script.")
			next := f.create("NameDefault", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			if got, want := nameBeforeHooksCounter(f.t, next), nameBeforeHooksCounter(f.t, first)+1; got != want {
				t.Fatalf("next counter = %d, want %d (refused save consumed a number)", got, want)
			}
		})

		f.run("refused dated save does not consume a number", func(f *packFixture) {
			first := f.create("NameDated", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			f.refused(httpMethodPost, "NameDated", "", nameBeforeHooksBody(f.t, map[string]any{"mode": "refuse"}), "Refused by the pack script.")
			next := f.create("NameDated", nameBeforeHooksBody(f.t, map[string]any{"mode": "ok"}))
			if got, want := nameBeforeHooksCounter(f.t, next), nameBeforeHooksCounter(f.t, first)+1; got != want {
				t.Fatalf("next counter = %d, want %d (refused save consumed a number)", got, want)
			}
		})
	})
}
