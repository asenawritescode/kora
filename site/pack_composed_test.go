package site

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

// readPackFiles loads a pack dir as typed PackFiles; ns != "" namespaces paths
// like Cloud's namespaceComponentFile.
func readPackFiles(t *testing.T, root, ns string) []PackFile {
	t.Helper()
	var files []PackFile
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		ct, ok := contract.PackageFileType(rel)
		if !ok {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if ns != "" {
			if !strings.Contains(rel, "/") {
				rel = path.Join("components", ns, rel)
			} else {
				dir, rest, _ := strings.Cut(rel, "/")
				rel = path.Join(dir, "components", ns, rest)
			}
		}
		files = append(files, PackFile{Path: rel, ContentType: ct, Content: string(body)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func snapshotPieces(t *testing.T, s *doctype.ConfigSnapshot) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"m": s.Modules, "c": s.Commands, "q": s.Queries, "p": s.PrintFormats, "perm": s.Permissions})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseConfigPackComposedMatchesTreeImport(t *testing.T) {
	pos, inv := filepath.Join("..", "config", "pos"), filepath.Join("..", "config", "inventory")
	dirs, err := ResolvePackTree(pos)
	if err != nil {
		t.Fatal(err)
	}
	want := &doctype.ConfigSnapshot{}
	names := map[string]bool{}
	for _, d := range dirs {
		s, err := parsePackSnapshot(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := mergePackSnapshot(want, s, names); err != nil {
			t.Fatal(err)
		}
	}
	if err := resolvePrintOperations(want); err != nil {
		t.Fatal(err)
	}
	if len(want.Modules) != 4 || len(want.Queries) != 10 || len(want.PrintFormats) != 4 {
		t.Fatalf("tree: %d modules %d queries %d print formats", len(want.Modules), len(want.Queries), len(want.PrintFormats))
	}

	invFiles, posFiles := readPackFiles(t, inv, ""), readPackFiles(t, pos, "")
	a, _, err := ParseConfigPack("inventory", invFiles)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := ParseConfigPack("pos", posFiles)
	if err != nil {
		t.Fatal(err)
	}
	standalone := &doctype.ConfigSnapshot{}
	names = map[string]bool{}
	for _, s := range []*doctype.ConfigSnapshot{a, b} {
		if err := mergePackSnapshot(standalone, s, names); err != nil {
			t.Fatal(err)
		}
	}
	if err := resolvePrintOperations(standalone); err != nil {
		t.Fatal(err)
	}
	if got := snapshotPieces(t, standalone); got != snapshotPieces(t, want) {
		t.Fatalf("standalone differs:\n%s\n%s", got, snapshotPieces(t, want))
	}

	composed := append(readPackFiles(t, inv, "aaaaaaaaaaaaaaaa"), readPackFiles(t, pos, "bbbbbbbbbbbbbbbb")...)
	got, _, err := ParseConfigPack("ignored", composed)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := snapshotPieces(t, got), snapshotPieces(t, want); g != w {
		t.Fatalf("composed differs:\n%s\n%s", g, w)
	}

	dup := append(readPackFiles(t, inv, "aaaaaaaaaaaaaaaa"), readPackFiles(t, inv, "cccccccccccccccc")...)
	if _, _, err := ParseConfigPack("x", dup); err == nil {
		t.Fatal("duplicate component pack name accepted")
	}
	var noManifest []PackFile
	for _, f := range composed {
		if f.ContentType != "pack" || !strings.Contains(f.Path, "bbbbbbbbbbbbbbbb") {
			noManifest = append(noManifest, f)
		}
	}
	if _, _, err := ParseConfigPack("x", noManifest); err == nil || !strings.Contains(err.Error(), "pack.manifest_required") {
		t.Fatalf("want pack.manifest_required, got %v", err)
	}
}

func TestValidatePackComposedInventoryPOSHasNoLinkTitleFindings(t *testing.T) {
	var files []PackFile
	for _, c := range []string{"inventory", "pos"} {
		files = append(files, readPackFiles(t, filepath.Join("..", "config", c), c)...)
	}
	v, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "composed", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range v.Findings {
		if f.Code == "link_title_unsearchable" {
			t.Errorf("%s: %s", f.Path, f.Message)
		}
	}
	for i := range files {
		if strings.HasSuffix(files[i].Path, "/register.yaml") {
			files[i].Content = strings.Replace(files[i].Content, "search_index: true", "", 1)
		}
	}
	v, _ = ValidatePack(PackManifest{SchemaVersion: 1, Name: "composed", Version: "1.0.0"}, files)
	found := false
	for _, f := range v.Findings {
		found = found || f.Code == "link_title_unsearchable"
	}
	if !found {
		t.Error("unindexed link title must still fail")
	}
}
