package analytics

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceFileAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.wal")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := replaceFileAtomically(dir, path, "new\n"); err != nil {
		t.Fatalf("replaceFileAtomically() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new\n" {
		t.Fatalf("file = %q, %v; want %q", got, err, "new\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v, %v; want only the replaced file (no leftover temp file)", entries, err)
	}
}

func TestReplaceFileAtomicallyLeavesOriginalWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	// A directory at the target path makes the final rename fail after the temp file was written.
	target := filepath.Join(dir, "events.wal")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceFileAtomically(dir, target, "new\n"); err == nil {
		t.Fatal("replaceFileAtomically() succeeded; want the rename error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory entries = %v, %v; want the temp file removed after a failed replace", entries, err)
	}
}
