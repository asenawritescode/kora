package doctype

import (
	"encoding/json"
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

func TestDocTypeJSONListsAreNeverNull(t *testing.T) {
	tests := []struct {
		name       string
		doctype    DocType
		wantFields int
	}{
		{"no fields and no constraints", DocType{Name: "POS Sale"}, 0},
		{"empty slices", DocType{Name: "POS Sale", Fields: []Field{}, DocConstraints: []DocConstraint{}}, 0},
		{"fields kept", DocType{Name: "POS Sale", Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A value, a pointer and a list of pointers are all ways an API response holds one.
			for what, v := range map[string]any{"value": tt.doctype, "pointer": &tt.doctype} {
				raw, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("%s: marshal: %v", what, err)
				}
				var got map[string]any
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatalf("%s: unmarshal: %v", what, err)
				}
				fields, ok := got["fields"].([]any)
				if !ok || len(fields) != tt.wantFields {
					t.Fatalf("%s: fields = %#v, want an array of %d", what, got["fields"], tt.wantFields)
				}
				if _, ok := got["doc_constraints"].([]any); !ok {
					t.Fatalf("%s: doc_constraints = %#v, want an array", what, got["doc_constraints"])
				}
				if got["name"] != "POS Sale" {
					t.Fatalf("%s: name = %v, want it kept", what, got["name"])
				}
			}
			raw, err := json.Marshal([]*DocType{&tt.doctype})
			if err != nil {
				t.Fatal(err)
			}
			var list []map[string]any
			if err := json.Unmarshal(raw, &list); err != nil || len(list) != 1 {
				t.Fatalf("list: %v %s", err, raw)
			}
			if _, ok := list[0]["fields"].([]any); !ok {
				t.Fatalf("list: fields = %#v, want an array", list[0]["fields"])
			}
		})
	}
}

func TestDocTypeJSONRoundTripKeepsEverythingElse(t *testing.T) {
	in := DocType{Name: "Sale", Module: "Selling", IsSubmittable: true, TitleField: "name", Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out DocType
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Module != in.Module || !out.IsSubmittable || out.TitleField != in.TitleField || len(out.Fields) != 1 || out.Fields[0].Fieldname != "total" {
		t.Fatalf("round trip lost data: %+v", out)
	}
}
