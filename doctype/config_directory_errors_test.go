package doctype

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOptionalConfigDirectoriesDistinguishMissingFromReadFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if views, err := ParseViewsDirectory(missing); err != nil || len(views) != 0 {
		t.Fatalf("missing views directory = %v, %v; want empty, nil", views, err)
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseViewsDirectory(file); err == nil {
		t.Fatal("views directory read error was treated as an absent optional directory")
	}
}

func TestConfigDirectoryParsersRejectMalformedOptionalFiles(t *testing.T) {
	root := t.TempDir()
	views := filepath.Join(root, "views")
	if err := os.Mkdir(views, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(views, "broken.yaml"), []byte("name: [unterminated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseViewsDirectory(views); err == nil {
		t.Fatal("malformed view YAML was ignored")
	}
}
