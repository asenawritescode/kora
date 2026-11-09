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

func TestDocTypeDerivedMetadataCachesFieldGroups(t *testing.T) {
	dt := &DocType{
		Name: "Order",
		Fields: []Field{
			{Fieldname: "section", Fieldtype: "Section Break"},
			{Fieldname: "title", Fieldtype: "Data", InListView: true},
			{Fieldname: "total", Fieldtype: "Currency"},
			{Fieldname: "items", Fieldtype: "Table", Options: "Order Item", InListView: true},
		},
	}
	dt.RebuildDerivedMetadata()

	if got := len(dt.DataFields()); got != 3 {
		t.Fatalf("DataFields len = %d, want 3", got)
	}
	if got := len(dt.NonTableDataFields()); got != 2 {
		t.Fatalf("NonTableDataFields len = %d, want 2", got)
	}
	if got := len(dt.TableFields()); got != 1 {
		t.Fatalf("TableFields len = %d, want 1", got)
	}
	if got := len(dt.ListFields()); got != 1 {
		t.Fatalf("ListFields len = %d, want 1", got)
	}
	validSortColumns := dt.ValidSortColumns()
	if !validSortColumns["title"] || !validSortColumns["total"] || !validSortColumns["modified"] {
		t.Fatalf("ValidSortColumns missing expected scalar/system columns: %v", validSortColumns)
	}
	if validSortColumns["items"] || validSortColumns["section"] {
		t.Fatalf("ValidSortColumns includes non-sortable fields: %v", validSortColumns)
	}
}

func TestRegistryRegisterRebuildsDerivedMetadata(t *testing.T) {
	reg := NewRegistry()
	dt := &DocType{
		Name:   "Customer",
		Fields: []Field{{Fieldname: "title", Fieldtype: "Data"}},
	}
	reg.Register(dt)

	dt.Fields = append(dt.Fields, Field{Fieldname: "items", Fieldtype: "Table"})
	reg.Register(dt)

	registered := reg.Get("Customer")
	if got := len(registered.TableFields()); got != 1 {
		t.Fatalf("TableFields len after re-register = %d, want 1", got)
	}
}
