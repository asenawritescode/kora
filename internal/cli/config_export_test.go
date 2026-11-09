package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestPublishConfigExportReplacesOwnedFilesAndPreservesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publishConfigExport(root, map[string][]byte{"doctypes/task.yaml": []byte("old"), "roles.yaml": []byte("roles")}); err != nil {
		t.Fatalf("initial export: %v", err)
	}
	if err := publishConfigExport(root, map[string][]byte{"doctypes/task.yaml": []byte("new")}); err != nil {
		t.Fatalf("replacement export: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "doctypes", "task.yaml"))
	if err != nil || string(got) != "new" {
		t.Fatalf("replacement file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "roles.yaml")); !os.IsNotExist(err) {
		t.Fatalf("stale owned file remains: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file = %q, %v", got, err)
	}
	manifest, err := readConfigExportManifestData(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 || manifest.Files[0].Path != "doctypes/task.yaml" || manifest.Files[0].SHA256 == "" {
		t.Fatalf("manifest does not describe the committed files: %+v", manifest)
	}
}

func TestPageManifestYAMLRoundTrip(t *testing.T) {
	manifest := &doctype.PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata:   doctype.PageManifestMetadata{Name: "Register", Version: "1.0.0", Package: "retail", Status: "active"},
		Spec: doctype.PageManifestSpec{
			Route: "/register", Runtime: ">=2.0.0 <3.0.0", Offline: "read_only",
			Layout: doctype.PageManifestLayout{Type: "single"},
		},
	}
	data, err := marshalPageManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "register.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := doctype.ParsePageManifestsDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 || parsed[0].APIVersion != manifest.APIVersion || parsed[0].Metadata.Name != manifest.Metadata.Name {
		t.Fatalf("unexpected round-tripped page manifest: %#v", parsed)
	}
}

func TestRecoverConfigExportRestoresInterruptedReplacement(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, configExportStagePrefix+"interrupted")
	backupDir := filepath.Join(stage, "backups")
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "roles.yaml"), []byte("partial-new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "000000"), []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := configExportJournal{Transaction: "interrupted", Items: []configExportJournalItem{{Path: "roles.yaml", Publish: true}}}
	data, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "journal.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := recoverConfigExports(root); err != nil {
		t.Fatalf("recover interrupted export: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "roles.yaml"))
	if err != nil || string(got) != "previous" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("recovery stage remains: %v", err)
	}
}

func TestPublishConfigExportRollsBackEarlierFilesWhenLaterTargetConflicts(t *testing.T) {
	root := t.TempDir()
	if err := publishConfigExport(root, map[string][]byte{"a.yaml": []byte("previous")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "b.yaml"), 0755); err != nil {
		t.Fatal(err)
	}
	err := publishConfigExport(root, map[string][]byte{"a.yaml": []byte("replacement"), "b.yaml": []byte("blocked")})
	if err == nil {
		t.Fatal("export succeeded despite a directory collision")
	}
	got, readErr := os.ReadFile(filepath.Join(root, "a.yaml"))
	if readErr != nil || string(got) != "previous" {
		t.Fatalf("earlier file was not rolled back: %q, %v", got, readErr)
	}
	manifest, manifestErr := readConfigExportManifestData(root)
	if manifestErr != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != "a.yaml" {
		t.Fatalf("prior manifest was not restored: %+v, %v", manifest, manifestErr)
	}
}

func TestConfigExportFilenameAndManifestRejectUnsafePaths(t *testing.T) {
	name, err := configExportFilename("doctypes", "Sale / Detail")
	if err != nil || name != "doctypes/sale_detail.yaml" {
		t.Fatalf("filename = %q, %v", name, err)
	}
	for _, path := range []string{"../outside.yaml", "doctypes/../../outside.yaml", `doctypes\\outside.yaml`, "C:/outside.yaml"} {
		if err := validateConfigExportPath(path); err == nil {
			t.Errorf("unsafe path %q accepted", path)
		}
	}
	files := map[string][]byte{}
	if err := addConfigExportFile(files, "doctypes/sale_detail.yaml", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := addConfigExportFile(files, "doctypes/sale_detail.yaml", []byte("two")); err == nil {
		t.Fatal("colliding generated filename accepted")
	}
}
