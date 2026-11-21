package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgriculturePackIsUniqueImportableAndHasUserAssignmentsAndReport(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "agriculture")
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "Farm Operations", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.Valid {
		t.Fatalf("farm pack findings: %+v", validation.Findings)
	}
	manifest, err := CanonicalManifestJSON(PackManifest{SchemaVersion: 1, Name: "Farm Operations", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PackDigest(manifest, files); err != nil {
		t.Fatalf("report-containing farm package did not produce a digest: %v", err)
	}
	snapshot, _, err := parseConfigPack("farm-operations", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.DocTypes) != 11 {
		t.Fatalf("got %d farm doctypes; want 11", len(snapshot.DocTypes))
	}
	byName := make(map[string]map[string]string, len(snapshot.DocTypes))
	for _, docType := range snapshot.DocTypes {
		fields := make(map[string]string, len(docType.Fields))
		for _, field := range docType.Fields {
			fields[field.Fieldname] = field.Fieldtype
		}
		if _, exists := byName[docType.Name]; exists {
			t.Fatalf("duplicate DocType %q", docType.Name)
		}
		byName[docType.Name] = fields
	}
	for doctype, field := range map[string]string{"Crop Plan": "owner_user", "Farm Task": "assigned_to", "Harvest Batch": "recorded_by"} {
		if got := byName[doctype][field]; got != "User" {
			t.Errorf("%s.%s fieldtype = %q; want User picker", doctype, field, got)
		}
	}
}

func TestAgricultureTraceabilityScriptRequiresFieldSeasonAndHarvestBatch(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "agriculture")
	var script string
	for _, file := range files {
		if file.Path == "scripts/agriculture_traceability.js" {
			script = file.Content
			break
		}
	}
	if script == "" {
		t.Fatal("farm pack is missing its harvest traceability guard")
	}
	for _, required := range []string{"doc.status === \"harvested\"", "!doc.field", "!doc.season", "!doc.harvest_batch", "throw new Error"} {
		if !strings.Contains(script, required) {
			t.Errorf("harvest traceability guard does not contain %q", required)
		}
	}

}

func TestFarmTeamControlsPackValidatesItsReportsAndUsesWorkspaceUsers(t *testing.T) {
	files := loadConfigProfilePackFiles(t, "agriculture_team")
	validation, err := ValidatePack(PackManifest{SchemaVersion: 1, Name: "Farm Team Controls", Version: "1.0.0"}, files)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.Valid {
		t.Fatalf("farm team pack findings: %+v", validation.Findings)
	}
	snapshot, _, err := parseConfigPack("farm-team-controls", files)
	if err != nil {
		t.Fatal(err)
	}
	fieldsByName := make(map[string]map[string]string, len(snapshot.DocTypes))
	for _, docType := range snapshot.DocTypes {
		fields := make(map[string]string, len(docType.Fields))
		for _, field := range docType.Fields {
			fields[field.Fieldname] = field.Fieldtype
		}
		fieldsByName[docType.Name] = fields
	}
	for doctype, field := range map[string]string{"Farm Labor Entry": "worker", "Field Observation": "observed_by", "Farm Cost Entry": "recorded_by"} {
		if got := fieldsByName[doctype][field]; got != "User" {
			t.Errorf("%s.%s fieldtype = %q; want User picker", doctype, field, got)
		}
	}
}

func loadConfigProfilePackFiles(t *testing.T, profile string) []PackFile {
	t.Helper()
	root := filepath.Join("..", "..", "config", "v0", profile)
	var files []PackFile
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		contentType := ""
		switch {
		case rel == "roles.yaml":
			contentType = "roles"
		case rel == "permissions.yaml":
			contentType = "permissions"
		case strings.HasPrefix(rel, "workflows/") && strings.HasSuffix(rel, ".json"):
			contentType = "workflow_definition"
		case strings.HasPrefix(rel, "doctypes/"):
			contentType = "doctype"
		case strings.HasPrefix(rel, "views/"):
			contentType = "view"
		case strings.HasPrefix(rel, "reports/"):
			contentType = "report"
		case strings.HasPrefix(rel, "scripts/"):
			contentType = "script"
		default:
			return nil
		}
		files = append(files, PackFile{Path: rel, ContentType: contentType, Content: string(content)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
