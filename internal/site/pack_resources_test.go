package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packFilesWithResources(t *testing.T, root string) []PackFile {
	t.Helper()
	files := loadPackFilesAtRoot(t, root)
	types := map[string]string{"queries/": "query", "commands/": "command", "print_formats/": "print_format"}
	for prefix, ct := range types {
		matches, _ := filepath.Glob(filepath.Join(root, prefix, "*.y*ml"))
		for _, m := range matches {
			body, err := os.ReadFile(m)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, PackFile{Path: prefix + filepath.Base(m), ContentType: ct, Content: string(body)})
		}
	}
	return files
}

func TestParseConfigPackResourcesMatchTreeImport(t *testing.T) {
	const root = "../../config/inventory"
	want, err := parsePackSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	files := packFilesWithResources(t, root)
	got, _, err := parseConfigPack("inventory", files)
	if err != nil {
		t.Fatal(err)
	}
	manifest, ok, err := ParsePackFile(root)
	if err != nil || !ok {
		t.Fatalf("pack.yaml: %v %v", ok, err)
	}
	if got.Modules, err = GeneratePackModules(manifest, got); err != nil {
		t.Fatal(err)
	}
	applyPackOwnership(got, got.Modules)
	if len(want.Queries) == 0 || len(want.Commands) == 0 {
		t.Fatalf("fixture lacks queries/commands: %d %d", len(want.Queries), len(want.Commands))
	}
	for name, pair := range map[string][2][]json.RawMessage{
		"commands": {got.Commands, want.Commands}, "queries": {got.Queries, want.Queries}, "print_formats": {got.PrintFormats, want.PrintFormats},
	} {
		a, _ := json.Marshal(pair[0])
		b, _ := json.Marshal(pair[1])
		if string(a) != string(b) {
			t.Errorf("%s differ:\n got %s\nwant %s", name, a, b)
		}
	}
}

func TestValidatePackRejectsInvalidResourceFiles(t *testing.T) {
	cases := []struct{ name, path, ct, content string }{
		{"query unknown key", "queries/q.yaml", "query", "bogus: 1\n"},
		{"command invalid", "commands/c.yaml", "command", "bogus: 1\n"},
		{"print format invalid", "print_formats/p.yaml", "print_format", "bogus: 1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := PackFile{Path: tc.path, ContentType: tc.ct, Content: tc.content}
			if err := validatePackFileSyntax(file); err == nil {
				t.Fatal("expected syntax error")
			}
			if _, _, err := parseConfigPack("x", []PackFile{file}); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
	if err := newPackResourceNames().add(PackFile{Path: "a", ContentType: "query", Content: "name: q\nmodule_owner_id: m\n"}); err != nil {
		t.Fatal(err)
	}
	names := newPackResourceNames()
	f := PackFile{Path: "queries/a.yaml", ContentType: "query", Content: "name: q\nmodule_owner_id: m\n"}
	g := f
	g.Path = "queries/b.yaml"
	if err := names.add(f); err != nil {
		t.Fatal(err)
	}
	if err := names.add(g); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate query = %v", err)
	}
}
