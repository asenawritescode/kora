package site

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The marketplace refuses a release when the Engine's own pack validation finds
// a blocking problem. These tests run that same validation over every ERP module
// exactly as the publish step sends it, alone and composed, so a bad pack is
// found here and not at publish time.

var erpModules = []string{"pos-base", "erp-inventory", "erp-purchasing", "erp-accounting"}

func erpModuleFiles(t *testing.T, pack string) []PackFile {
	t.Helper()
	root := filepath.Join("..", "config", pack)
	types := map[string]string{"doctypes": "doctype", "views": "view", "reports": "report"}
	var files []PackFile
	for dir, contentType := range types {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			body, err := os.ReadFile(filepath.Join(root, dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, PackFile{Path: dir + "/" + entry.Name(), ContentType: contentType, Content: string(body)})
		}
	}
	for name, contentType := range map[string]string{"roles.yaml": "roles", "permissions.yaml": "permissions"} {
		if body, err := os.ReadFile(filepath.Join(root, name)); err == nil {
			files = append(files, PackFile{Path: name, ContentType: contentType, Content: string(body)})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

func requireValidPack(t *testing.T, name string, files []PackFile) {
	t.Helper()
	result, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: name, Version: "1.0.0"}, files)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	for _, finding := range result.Findings {
		if finding.Severity == "error" {
			t.Errorf("%s: %s %s: %s", name, finding.Code, finding.Path, finding.Message)
		}
	}
}

func TestEachERPModulePassesMarketplaceValidation(t *testing.T) {
	for _, pack := range erpModules {
		t.Run(pack, func(t *testing.T) {
			requireValidPack(t, pack, erpModuleFiles(t, pack))
		})
	}
}

// The template installs the modules together, so they must also validate and
// import as one: no clashing paths, no dangling links, no conflicting roles.
func TestERPModulesPassValidationAndImportWhenComposed(t *testing.T) {
	var all []PackFile
	for _, pack := range erpModules {
		for _, file := range erpModuleFiles(t, pack) {
			if file.ContentType == "roles" || file.ContentType == "permissions" {
				continue // merged below, as composition does
			}
			all = append(all, file)
		}
	}
	for _, kind := range []string{"roles", "permissions"} {
		merged := ""
		for _, pack := range erpModules {
			for _, file := range erpModuleFiles(t, pack) {
				if file.ContentType == kind {
					merged += file.Content
				}
			}
		}
		all = append(all, PackFile{Path: kind + ".yaml", ContentType: kind, Content: merged})
	}
	requireValidPack(t, "composed", all)
	if _, _, err := ParseConfigPack("composed", all); err != nil {
		t.Fatalf("the composed modules do not import: %v", err)
	}
}
