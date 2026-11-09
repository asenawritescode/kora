package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func writePack(t *testing.T, root, name, manifest string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestResolvePackTree(t *testing.T) {
	req := func(id, v string) string { return "  - package_id: " + id + "\n    version: \"" + v + "\"\n" }
	tests := []struct {
		name  string
		packs map[string]string
		want  []string
		err   string
	}{
		{"order and dedupe", map[string]string{
			"a": "name: a\nversion: \"1\"\nrequires:\n" + req("b", "1") + req("c", "1"),
			"b": "name: b\nversion: \"1\"\nrequires:\n" + req("c", "1"),
			"c": "name: c\nversion: \"1\"\n"}, []string{"c", "b", "a"}, ""},
		{"no manifest", map[string]string{"a": ""}, []string{"a"}, ""},
		{"missing", map[string]string{"a": "name: a\nversion: \"1\"\nrequires:\n" + req("zz", "1")}, nil, "pack.requires_missing"},
		{"cycle", map[string]string{
			"a": "name: a\nversion: \"1\"\nrequires:\n" + req("b", "1"),
			"b": "name: b\nversion: \"1\"\nrequires:\n" + req("a", "1")}, nil, "pack.requires_cycle"},
		{"version", map[string]string{
			"a": "name: a\nversion: \"1\"\nrequires:\n" + req("b", "2"),
			"b": "name: b\nversion: \"1\"\n"}, nil, "pack.requires_version"},
		{"name mismatch", map[string]string{"a": "name: x\nversion: \"1\"\n"}, nil, "must equal directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for n, m := range tt.packs {
				writePack(t, root, n, m)
			}
			got, err := ResolvePackTree(filepath.Join(root, "a"))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, d := range got {
				names = append(names, filepath.Base(d))
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("got %v want %v", names, tt.want)
			}
		})
	}
}

func TestMergePackSnapshot(t *testing.T) {
	a := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Item"}}, Roles: []*doctype.Role{{Name: "R", Description: "first"}}, Permissions: []*doctype.Permission{{Doctype: "Item"}}}
	b := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Sale"}}, Roles: []*doctype.Role{{Name: "R", Description: "second"}}, Permissions: []*doctype.Permission{{Doctype: "Sale"}}}
	dst := &doctype.ConfigSnapshot{}
	names := map[string]bool{}
	if err := mergePackSnapshot(dst, a, names); err != nil {
		t.Fatal(err)
	}
	if err := mergePackSnapshot(dst, b, names); err != nil {
		t.Fatal(err)
	}
	if len(dst.DocTypes) != 2 || len(dst.Permissions) != 2 || len(dst.Roles) != 1 || dst.Roles[0].Description != "first" {
		t.Fatalf("bad merge: %+v", dst)
	}
	err := mergePackSnapshot(dst, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Item"}}}, names)
	if err == nil || !strings.Contains(err.Error(), "pack.resource_conflict") {
		t.Fatalf("err = %v", err)
	}
}

func TestShippedPOSPackResolvesInventory(t *testing.T) {
	got, err := ResolvePackTree(filepath.Join("..", "..", "config", "pos"))
	if err != nil || len(got) != 2 || filepath.Base(got[0]) != "inventory" {
		t.Fatalf("got %v err %v", got, err)
	}
}
