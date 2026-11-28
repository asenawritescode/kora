package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestPackScriptNamesAreNamespacedByPack(t *testing.T) {
	for _, tc := range []struct {
		name, pack, path string
		want             string
	}{
		{"bound", "sales", "scripts/Sale Item/before_save.js", "sales__Sale_Item_before_save"},
		{"flat", "sales", "scripts/rules.js", "sales__rules"},
		{"library", "books", "scripts/lib/books.js", "lib_books"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := packScriptSnapshot(tc.pack, tc.path, []byte("return true;"))
			if got.Name != tc.want || got.OwnerPack != tc.pack {
				t.Fatalf("name, owner = %q, %q; want %q, %q", got.Name, got.OwnerPack, tc.want, tc.pack)
			}
		})
	}
}

func TestPackScriptBindingRule(t *testing.T) {
	docTypes := func(names ...string) []*doctype.DocType {
		var out []*doctype.DocType
		for _, n := range names {
			out = append(out, &doctype.DocType{Name: n})
		}
		return out
	}
	ref := func(name string) []contract.ManifestPackRef {
		return []contract.ManifestPackRef{{PackageID: name}}
	}
	bound := func(pack, doctypeName string) *doctype.ScriptSnapshot {
		return packScriptSnapshot(pack, "scripts/"+doctypeName+"/before_save.js", []byte("x"))
	}
	for _, tc := range []struct {
		name    string
		packs   map[string][]*doctype.DocType
		reqs    map[string][]contract.ManifestPackRef
		script  *doctype.ScriptSnapshot
		wantErr string
		legacy  string
		pending bool
	}{
		{name: "own doctype", packs: map[string][]*doctype.DocType{"sales": docTypes("Sale")},
			script: bound("sales", "Sale"), legacy: "Sale_before_save"},
		{name: "core doctype not defined", packs: map[string][]*doctype.DocType{"sales": docTypes("Sale Item")},
			script: bound("sales", "Customer"), wantErr: "pack.script_binding_undefined"},
		{name: "other pack doctype without requires", packs: map[string][]*doctype.DocType{"sales": docTypes("Sale"), "ext": docTypes("Contact")},
			script: bound("ext", "Sale"), wantErr: "pack.script_binding_undefined"},
		{name: "doctype of a required pack", packs: map[string][]*doctype.DocType{"core": docTypes("Sale"), "ext": nil},
			reqs: map[string][]contract.ManifestPackRef{"ext": ref("core")}, script: bound("ext", "Sale"), legacy: "Sale_before_save"},
		{name: "doctype of a transitively required pack", packs: map[string][]*doctype.DocType{"core": docTypes("Sale"), "mid": nil, "ext": nil},
			reqs: map[string][]contract.ManifestPackRef{"ext": ref("mid"), "mid": ref("core")}, script: bound("ext", "Sale"), legacy: "Sale_before_save"},
		{name: "requires cycle terminates", packs: map[string][]*doctype.DocType{"a": nil, "b": nil},
			reqs: map[string][]contract.ManifestPackRef{"a": ref("b"), "b": ref("a")}, script: bound("a", "Sale"), wantErr: "pack.script_binding_undefined"},
		{name: "requires a pack outside the artifact is resolved at activation", packs: map[string][]*doctype.DocType{"ext": nil},
			reqs: map[string][]contract.ManifestPackRef{"ext": ref("installed")}, script: bound("ext", "Sale"), legacy: "Sale_before_save", pending: true},
		{name: "row name over the column limit", packs: map[string][]*doctype.DocType{"sales": docTypes(strings.Repeat("A", 140))},
			script: bound("sales", strings.Repeat("A", 140)), wantErr: "pack.script_name_too_long"},
		{name: "library and flat scripts are not bound", packs: map[string][]*doctype.DocType{"books": nil},
			script: packScriptSnapshot("books", "scripts/lib/books.js", []byte("x"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := newPackScriptRule()
			for pack, dts := range tc.packs {
				rule.addPack(pack, dts, tc.reqs[pack])
			}
			err := rule.check([]*doctype.ScriptSnapshot{tc.script})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("check: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("check = %v; want %s", err, tc.wantErr)
			}
			if tc.wantErr == "" && tc.script.ReplacesLegacy != tc.legacy {
				t.Fatalf("ReplacesLegacy = %q; want %q", tc.script.ReplacesLegacy, tc.legacy)
			}
			if tc.wantErr == "" && tc.script.PendingBinding != tc.pending {
				t.Fatalf("PendingBinding = %v; want %v", tc.script.PendingBinding, tc.pending)
			}
		})
	}
}

// Every shipped config pack that ships scripts must still satisfy the binding
// rule: each scripts/<DocType>/<event>.js binds a DocType the pack defines.
func TestShippedPacksBindOnlyTheirOwnDocTypes(t *testing.T) {
	root := filepath.Join("..", "..", "config")
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || d.Name() != "scripts" {
			return err
		}
		dirs = append(dirs, filepath.Dir(p))
		return filepath.SkipDir
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no shipped pack scripts found")
	}
	for _, dir := range dirs {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			docTypes, err := doctype.ParseConfigTree(dir)
			if err != nil {
				t.Fatalf("parse doctypes: %v", err)
			}
			rule := newPackScriptRule()
			rule.addPack(name, docTypes, nil)
			var scripts []*doctype.ScriptSnapshot
			err = filepath.WalkDir(filepath.Join(dir, "scripts"), func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, err := filepath.Rel(dir, p)
				if err != nil {
					return err
				}
				body, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				scripts = append(scripts, packScriptSnapshot(name, filepath.ToSlash(rel), body))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := rule.check(scripts); err != nil {
				t.Fatalf("shipped pack %s violates the binding rule: %v", dir, err)
			}
		})
	}
}

// Config versions are stored as s-expressions, so ownership must survive them
// or a rollback would activate a pack's rows with no owner.
func TestPackScriptOwnershipSurvivesConfigVersionEncoding(t *testing.T) {
	owned := packScriptSnapshot("sales", "scripts/Sale/before_save.js", []byte("x"))
	owned.ReplacesLegacy = "Sale_before_save"
	lib := packScriptSnapshot("books", "scripts/lib/books.js", []byte("x"))
	snapshot := &doctype.ConfigSnapshot{Scripts: []*doctype.ScriptSnapshot{owned, lib}}
	decoded, err := doctype.FromSExpr(doctype.ToSExpr(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, s := range decoded.Scripts {
		got[s.Name] = [2]string{s.OwnerPack, s.ReplacesLegacy}
	}
	if got[owned.Name] != [2]string{"sales", "Sale_before_save"} || got["lib_books"] != [2]string{"books", ""} {
		t.Fatalf("decoded ownership = %v", got)
	}
}

func TestPackScriptRuleMergesDocTypesOfOnePack(t *testing.T) {
	// A pack whose files span a root manifest and a component without pack.yaml
	// reaches addPack twice under one name: the second call must not drop the first.
	rule := newPackScriptRule()
	rule.addPack("alpha", []*doctype.DocType{{Name: "Sale"}}, nil)
	rule.addPack("alpha", []*doctype.DocType{{Name: "Alpha Item"}}, nil)
	scripts := []*doctype.ScriptSnapshot{
		{Name: "alpha__Sale_before_save", OwnerPack: "alpha", DocType: "Sale", Event: "before_save", ScriptType: "doc_event"},
		{Name: "alpha__Alpha_Item_before_save", OwnerPack: "alpha", DocType: "Alpha Item", Event: "before_save", ScriptType: "doc_event"},
	}
	if err := rule.check(scripts); err != nil {
		t.Fatalf("check after merged addPack: %v", err)
	}
}
