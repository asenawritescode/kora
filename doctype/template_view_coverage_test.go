package doctype

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAllTemplateViewsMeetRuntimeContract prevents a new template from
// reaching the runtime with a structurally valid YAML file that is still
// difficult to use on a phone or impossible to understand in a list.
//
// This intentionally checks presentation contracts only. Binding a field to
// a real DocType belongs to the registry/API validation tests, where the
// complete site configuration is available.
func TestAllTemplateViewsMeetRuntimeContract(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "config", "*", "views", "*.yaml"))
	if err != nil {
		t.Fatalf("find template views: %v", err)
	}
	legacyPaths, err := filepath.Glob(filepath.Join("..", "config", "v0", "*", "views", "*.yaml"))
	if err != nil {
		t.Fatalf("find v0 views: %v", err)
	}
	paths = append(paths, legacyPaths...)
	if len(paths) == 0 {
		t.Fatal("expected at least one template view")
	}

	var viewCount, recordTableCount int
	for _, path := range paths {
		view, err := ParseViewFile(path)
		if err != nil {
			t.Errorf("%s: parse: %v", path, err)
			continue
		}
		viewCount++
		if !strings.HasPrefix(view.Route, "/") {
			t.Errorf("%s: route %q must start with /", path, view.Route)
		}
		if strings.TrimSpace(view.Label) == "" {
			t.Errorf("%s: label is required for a user-facing view", path)
		}
		if strings.TrimSpace(view.Module) == "" {
			t.Errorf("%s: module is required for navigation", path)
		}
		checkTemplateComponents(t, path, view.Components, &recordTableCount)
	}

	t.Logf("validated %d template views and %d record tables", viewCount, recordTableCount)
}

func checkTemplateComponents(t *testing.T, path string, components []ViewComponent, recordTableCount *int) {
	t.Helper()
	for _, component := range components {
		if strings.TrimSpace(component.Label) == "" {
			t.Errorf("%s: component %q (%s) needs a user-facing label", path, component.ID, component.Type)
		}
		if component.Type == "record_table" {
			(*recordTableCount)++
			if strings.TrimSpace(component.SourceDocType) == "" {
				t.Errorf("%s: record table %q needs source_doctype", path, component.ID)
			}
			if len(component.DesktopColumns) == 0 {
				t.Errorf("%s: record table %q needs desktop_columns", path, component.ID)
			}
			if len(component.MobileColumns) == 0 {
				t.Errorf("%s: record table %q needs mobile_columns", path, component.ID)
			}
		}
		checkTemplateComponents(t, path, component.Components, recordTableCount)
	}
}
