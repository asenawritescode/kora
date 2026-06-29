package doctype

import (
	"testing"
)

func TestFieldDependencyScope(t *testing.T) {
	f := &Field{
		Fieldname:       "total",
		Fieldtype:       "Currency",
		Computed:        "(* qty price)",
		DependencyScope: "self",
	}
	if f.DependencyScope != "self" {
		t.Errorf("expected self, got %s", f.DependencyScope)
	}

	// Default should be empty string when not set.
	f2 := &Field{Fieldname: "name"}
	if f2.DependencyScope != "" {
		t.Errorf("expected empty string default, got %s", f2.DependencyScope)
	}
}

func TestFieldDependencyScopeValidValues(t *testing.T) {
	tests := []struct {
		scope string
		valid bool
	}{
		{"self", true},
		{"children", true},
		{"cross_doctype", true},
		{"", true},
		{"invalid", false},
	}

	for _, tt := range tests {
		f := &Field{Fieldname: "f", DependencyScope: tt.scope}
		switch f.DependencyScope {
		case "self", "children", "cross_doctype", "":
			if !tt.valid {
				t.Errorf("expected invalid for scope %q, but it was accepted", tt.scope)
			}
		default:
			if tt.valid {
				t.Errorf("expected valid for scope %q, but it was rejected", tt.scope)
			}
		}
	}
}
