package doctype

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestConfigPacksMeetUATFoundationContract audits every config pack that is
// shipped in the repository. It keeps template polish from being limited to
// the ERP Kenya demo site: all packs must parse, expose coherent permissions,
// keep views bound to local doctypes, and have workflows whose transitions
// point to real states and documents.
func TestConfigPacksMeetUATFoundationContract(t *testing.T) {
	packPaths, err := filepath.Glob(filepath.Join("..", "config", "*"))
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
		if !doctypeNames[permission.Doctype] {
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

	workflows, err := ParseWorkflowDirectory(filepath.Join(root, "doctypes"))
	if err != nil {
		t.Fatalf("parse workflows: %v", err)
	}
	for _, workflow := range workflows {
		if !doctypeNames[workflow.DocumentType] {
			t.Errorf("workflow %q references unknown doctype %q", workflow.Name, workflow.DocumentType)
		}
		states := make(map[string]bool, len(workflow.States))
		for _, state := range workflow.States {
			if strings.TrimSpace(state.State) == "" {
				t.Errorf("workflow %q has an unnamed state", workflow.Name)
			}
			if states[state.State] {
				t.Errorf("workflow %q repeats state %q", workflow.Name, state.State)
			}
			states[state.State] = true
		}
		for _, transition := range workflow.Transitions {
			if transition.Action == "" || !states[transition.From] || !states[transition.To] {
				t.Errorf("workflow %q has invalid transition %q (%q -> %q)", workflow.Name, transition.Action, transition.From, transition.To)
			}
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
