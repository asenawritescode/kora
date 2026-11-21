package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/internal/doctype"
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

// erpsellRefs lists every operation and query the pack publishes, keyed the way
// views and print formats name them.
func erpsellRefs(t *testing.T, snapshot *doctype.ConfigSnapshot) (operations, queries map[string]bool) {
	t.Helper()
	operations, queries = map[string]bool{}, map[string]bool{}
	for _, raw := range snapshot.Commands {
		var c struct {
			Owner      string `json:"module_owner_id"`
			Capability string `json:"capability"`
			Operation  string `json:"operation"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		operations[c.Owner+"~"+c.Capability+"~"+c.Operation] = true
	}
	for _, raw := range snapshot.Queries {
		var q struct {
			Owner string `json:"module_owner_id"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal(raw, &q); err != nil {
			t.Fatal(err)
		}
		queries[q.Owner+"/"+q.Name] = true
	}
	return operations, queries
}

var erpsellOperationRef = regexp.MustCompile(`erpsell\.core~[a-z_.]+~[a-z_]+`)

// TestErpsellRegisterViewResolves checks that the register view, the one the
// shops open at /pos, only names commands and queries the pack publishes.
func TestErpsellRegisterViewResolves(t *testing.T) {
	files := erpsellFilesValidated(t)
	snapshot, _, err := ParseConfigPack("erpsell", files)
	if err != nil {
		t.Fatalf("ParseConfigPack: %v", err)
	}
	operations, queries := erpsellRefs(t, snapshot)

	body, err := os.ReadFile(filepath.Join(erpsellPackDir, "views", "register.yaml"))
	if err != nil {
		t.Fatalf("register view: %v", err)
	}
	var view struct {
		Route     string `yaml:"route"`
		Resources []struct {
			Params struct {
				OwnerModuleID string `yaml:"owner_module_id"`
				QueryName     string `yaml:"query_name"`
			} `yaml:"params"`
		} `yaml:"resources"`
	}
	if err := yaml.Unmarshal(body, &view); err != nil {
		t.Fatalf("parse register view: %v", err)
	}
	if view.Route != "/pos" {
		t.Errorf("register view route = %q, want /pos", view.Route)
	}
	for _, resource := range view.Resources {
		if resource.Params.QueryName == "" {
			continue
		}
		key := resource.Params.OwnerModuleID + "/" + resource.Params.QueryName
		if !queries[key] {
			t.Errorf("register view resource names unknown query %s", key)
		}
	}
	for _, ref := range erpsellOperationRef.FindAllString(string(body), -1) {
		if !operations[ref] {
			t.Errorf("register view names unknown operation %s", ref)
		}
	}
}

// TestErpsellPrintFormatsResolve checks the query references inside the print
// formats against the pack's own queries.
func TestErpsellPrintFormatsResolve(t *testing.T) {
	files := erpsellFilesValidated(t)
	snapshot, _, err := ParseConfigPack("erpsell", files)
	if err != nil {
		t.Fatalf("ParseConfigPack: %v", err)
	}
	_, queries := erpsellRefs(t, snapshot)
	entries, err := os.ReadDir(filepath.Join(erpsellPackDir, "print_formats"))
	if err != nil {
		t.Fatal(err)
	}
	ref := regexp.MustCompile(`query: (erpsell\.core/[a-z_]+)`)
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(erpsellPackDir, "print_formats", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range ref.FindAllStringSubmatch(string(body), -1) {
			if !queries[match[1]] {
				t.Errorf("%s names unknown query %s", entry.Name(), match[1])
			}
		}
	}
}
