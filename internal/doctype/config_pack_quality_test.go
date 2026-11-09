package doctype

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"gopkg.in/yaml.v3"
)

// TestConfigPacksMeetUATFoundationContract audits every config pack that is
// shipped in the repository. It keeps template polish from being limited to
// the ERP Kenya demo site: all packs must parse, expose coherent permissions,
// keep views bound to local doctypes, and have workflows whose transitions
// point to real states and documents.
func TestConfigPacksMeetUATFoundationContract(t *testing.T) {
	packPaths, err := filepath.Glob(filepath.Join("..", "..", "config", "*"))
	if err != nil {
		t.Fatalf("find config packs: %v", err)
	}
	packCount := 0
	for _, root := range packPaths {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		if filepath.Base(root) == "v0" {
			legacyPacks, err := filepath.Glob(filepath.Join(root, "*"))
			if err != nil {
				t.Fatalf("find v0 config packs: %v", err)
			}
			for _, legacyPack := range legacyPacks {
				if legacyInfo, statErr := os.Stat(legacyPack); statErr == nil && legacyInfo.IsDir() {
					packCount++
					t.Run("v0/"+filepath.Base(legacyPack), func(t *testing.T) { auditConfigPack(t, legacyPack) })
				}
			}
			continue
		}
		packCount++
		t.Run(filepath.Base(root), func(t *testing.T) {
			auditConfigPack(t, root)
		})
	}
	if packCount == 0 {
		t.Fatal("expected config packs")
	}
	t.Logf("audited %d config packs", packCount)
}

func auditConfigPack(t *testing.T, root string) {
	t.Helper()
	doctypes, err := ParseConfigTree(root)
	if err != nil {
		t.Fatalf("parse doctypes: %v", err)
	}
	if len(doctypes) == 0 {
		t.Fatal("pack must contain at least one doctype")
	}

	// Some mature packs intentionally ship a generic scaffold and a
	// *_primary override with the same DocType name. ParseConfigTree preserves
	// that precedence order; the last definition is the effective one.
	doctypeNames := make(map[string]bool, len(doctypes))
	duplicates := 0
	for _, docType := range doctypes {
		if docType == nil {
			continue
		}
		if doctypeNames[docType.Name] {
			duplicates++
		}
		doctypeNames[docType.Name] = true
	}
	if duplicates > 0 {
		t.Logf("pack contains %d override doctype definitions; last definition is effective", duplicates)
	}

	for _, dep := range requiredPackDirs(t, root, map[string]bool{}) {
		depTypes, err := ParseConfigTree(dep)
		if err != nil {
			t.Fatalf("parse required pack %s: %v", dep, err)
		}
		for _, docType := range depTypes {
			doctypeNames[docType.Name] = true
		}
	}

	roles, permissions, err := ParseRolesDirectory(filepath.Join(root, "doctypes"))
	if err != nil {
		t.Fatalf("parse roles and permissions: %v", err)
	}
	roleNames := make(map[string]bool, len(roles))
	for _, role := range roles {
		if role == nil || strings.TrimSpace(role.Name) == "" {
			t.Fatal("role must have a name")
		}
		if roleNames[role.Name] {
			t.Fatalf("duplicate role name %q", role.Name)
		}
		roleNames[role.Name] = true
	}
	for _, permission := range permissions {
		if permission == nil {
			continue
		}
		if permission.Role != AdminRole && !roleNames[permission.Role] {
			t.Errorf("permission references unknown role %q", permission.Role)
		}
		if !doctypeNames[permission.Doctype] && !strings.HasPrefix(permission.Doctype, "module.") {
			t.Errorf("permission references unknown doctype %q", permission.Doctype)
		}
	}

	views, err := ParseViewsDirectory(filepath.Join(root, "views"))
	if err != nil {
		t.Fatalf("parse views: %v", err)
	}
	for _, view := range views {
		if err := view.Validate(); err != nil {
			t.Errorf("view %q: %v", view.Name, err)
		}
		checkConfigPackViewBindings(t, view.Components, doctypeNames)
	}

	workflowFiles, err := filepath.Glob(filepath.Join(root, "workflows", "*.json"))
	if err != nil {
		t.Fatalf("find workflow definitions: %v", err)
	}
	for _, workflowPath := range workflowFiles {
		body, err := os.ReadFile(workflowPath)
		if err != nil {
			t.Fatalf("read workflow definition %s: %v", workflowPath, err)
		}
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal(body, &definition); err != nil {
			t.Fatalf("parse workflow definition %s: %v", workflowPath, err)
		}
		if definition.ID == "" || definition.Version == 0 || len(definition.Steps) == 0 {
			t.Errorf("workflow definition %q has no ID, version, or steps", workflowPath)
		}
		if definition.Trigger != nil && !doctypeNames[definition.Trigger.Table] {
			t.Errorf("workflow definition %q references unknown trigger table %q", workflowPath, definition.Trigger.Table)
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("parse workflow definition fields %s: %v", workflowPath, err)
		}
		if _, retired := raw["lifecycle"]; retired {
			t.Errorf("workflow definition %q uses retired lifecycle policy configuration", workflowPath)
		}
	}

	checkConfigPackFixtures(t, root, doctypes)
}

type demoFixture struct {
	Records []map[string]any `yaml:"records"`
}

func checkConfigPackFixtures(t *testing.T, root string, doctypes []*DocType) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "fixtures", "*.yaml"))
	if err != nil {
		t.Errorf("find fixtures: %v", err)
		return
	}
	doctypeFields := make(map[string]map[string]bool)
	for _, docType := range doctypes {
		fields := doctypeFields[docType.Name]
		if fields == nil {
			fields = map[string]bool{"name": true, "owner": true, "creation": true, "modified": true, "modified_by": true, "doc_status": true}
			doctypeFields[docType.Name] = fields
		}
		for _, field := range docType.Fields {
			fields[field.Fieldname] = true
		}
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read: %v", path, err)
			continue
		}
		var fixture demoFixture
		if err := yaml.Unmarshal(data, &fixture); err != nil {
			t.Errorf("%s: parse: %v", path, err)
			continue
		}
		for index, record := range fixture.Records {
			doctypeName, _ := record["doctype"].(string)
			fields := doctypeFields[doctypeName]
			if fields == nil {
				t.Errorf("%s record %d references unknown doctype %q", path, index+1, doctypeName)
				continue
			}
			for key := range record {
				// `reference` is the fixture identity alias used by the demo
				// seeder when a record does not have a domain-specific name field.
				if key == "doctype" || key == "name" || key == "reference" {
					continue
				}
				if !fields[key] {
					t.Errorf("%s record %d (%s) uses unknown field %q", path, index+1, doctypeName, key)
				}
			}
		}
	}
}

func checkConfigPackViewBindings(t *testing.T, components []ViewComponent, doctypeNames map[string]bool) {
	t.Helper()
	for _, component := range components {
		if component.SourceDocType != "" && !doctypeNames[component.SourceDocType] {
			t.Errorf("component %q (%s) references unknown doctype %q", component.ID, component.Type, component.SourceDocType)
		}
		checkConfigPackViewBindings(t, component.Components, doctypeNames)
	}
}

// requiredPackDirs is a small local copy of site.ResolvePackTree (site imports
// doctype, so the resolver cannot be called from here).
func requiredPackDirs(t *testing.T, root string, seen map[string]bool) []string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "pack.yaml"))
	if err != nil {
		return nil
	}
	var manifest struct {
		Requires []struct {
			PackageID string `yaml:"package_id"`
		} `yaml:"requires"`
	}
	if err := yaml.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("parse pack.yaml: %v", err)
	}
	var dirs []string
	for _, ref := range manifest.Requires {
		dep := filepath.Join(filepath.Dir(root), ref.PackageID)
		if seen[dep] {
			continue
		}
		seen[dep] = true
		dirs = append(dirs, dep)
		dirs = append(dirs, requiredPackDirs(t, dep, seen)...)
	}
	return dirs
}
