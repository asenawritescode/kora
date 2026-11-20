package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The erpsell pack is the selling base that live shops upgrade in place. It is
// parked under config/_parked, so the skip helpers used for the other parked
// packs would hide it. These tests run the install path over the pack as it
// ships and do not skip.

const erpsellPackDir = "../../config/_parked/erpsell"

// erpsellFiles reads the pack the way the publish step does: one PackFile per
// YAML file, typed by its directory, plus the pack manifest.
func erpsellFiles(t *testing.T) []PackFile {
	t.Helper()
	dirs := map[string]string{
		"doctypes":      "doctype",
		"views":         "view",
		"reports":       "report",
		"queries":       "query",
		"commands":      "command",
		"print_formats": "print_format",
	}
	var files []PackFile
	for dir, contentType := range dirs {
		entries, err := os.ReadDir(filepath.Join(erpsellPackDir, dir))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			files = append(files, readErpsellFile(t, dir+"/"+entry.Name(), contentType))
		}
	}
	for _, name := range []string{"roles.yaml", "permissions.yaml"} {
		files = append(files, readErpsellFile(t, name, strings.TrimSuffix(name, ".yaml")))
	}
	files = append(files, readErpsellFile(t, "pack.yaml", "pack"))
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func readErpsellFile(t *testing.T, rel, contentType string) PackFile {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(erpsellPackDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return PackFile{Path: rel, ContentType: contentType, Content: string(body)}
}

func erpsellManifest(t *testing.T) PackManifest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(erpsellPackDir, "pack.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, err := parsePackManifest(body, "erpsell")
	if err != nil {
		t.Fatalf("pack.yaml: %v", err)
	}
	// pack.yaml carries no schema version; the publish step sets the v1 manifest.
	manifest.SchemaVersion = 1
	return manifest
}

// erpsellFilesValidated runs the publish-path validation and fails on any
// finding, so a bad reference is reported here and not at publish time.
func erpsellFilesValidated(t *testing.T) []PackFile {
	t.Helper()
	files := erpsellFiles(t)
	manifest := erpsellManifest(t)
	result, err := ValidatePack(manifest, files)
	if err != nil {
		t.Fatalf("ValidatePack: %v", err)
	}
	for _, finding := range result.Findings {
		if finding.Severity == "error" {
			t.Errorf("%s %s: %s", finding.Code, finding.Path, finding.Message)
		}
	}
	return files
}

func TestErpsellPackParsesAndOwnsItsModule(t *testing.T) {
	files := erpsellFilesValidated(t)
	manifest := erpsellManifest(t)
	snapshot, _, err := ParseConfigPack("erpsell", files)
	if err != nil {
		t.Fatalf("ParseConfigPack: %v", err)
	}
	modules, err := GeneratePackModules(manifest, snapshot)
	if err != nil {
		t.Fatalf("GeneratePackModules: %v", err)
	}
	if len(modules) == 0 || string(modules[0].ID) != "erpsell.core" {
		t.Fatalf("first module = %v, want erpsell.core", modules)
	}
	for _, raw := range append(append([]json.RawMessage{}, snapshot.Commands...), snapshot.Queries...) {
		var head struct {
			Name          string `json:"name"`
			ModuleOwnerID string `json:"module_owner_id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		if head.ModuleOwnerID != "erpsell.core" {
			t.Errorf("%s is owned by %q, want erpsell.core", head.Name, head.ModuleOwnerID)
		}
	}
}
