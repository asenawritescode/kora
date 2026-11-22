package doctype

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGeneratedFactsFollowHeaderFields(t *testing.T) {
	expense := &DocType{Name: "Expense", Fields: []Field{
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "qty", Fieldtype: "Int"},
		{Fieldname: "rate", Fieldtype: "Float"},
		{Fieldname: "supplier", Fieldtype: "Link"},
		{Fieldname: "kind", Fieldtype: "Select"},
		{Fieldname: "paid", Fieldtype: "Check"},
		{Fieldname: "approver", Fieldtype: "User"},
		{Fieldname: "status", Fieldtype: "Select"},
		{Fieldname: "owner", Fieldtype: "Link"},
		{Fieldname: "notes", Fieldtype: "Data"},
		{Fieldname: "lines", Fieldtype: "Table"},
	}}
	tests := []struct {
		name       string
		dt         *DocType
		wantNil    bool
		measures   []string
		dimensions []string
		status     string
	}{
		{name: "header fields become measures and dimensions", dt: expense,
			measures: []string{"amount", "qty", "rate"}, dimensions: []string{"supplier", "kind", "paid", "approver"}, status: "status"},
		{name: "child table gets no model", dt: &DocType{Name: "Line", IsChildTable: true, Fields: expense.Fields}, wantNil: true},
		{name: "single type gets no model", dt: &DocType{Name: "Settings", IsSingle: true, Fields: expense.Fields}, wantNil: true},
		{name: "no numeric field is a model with no measures", dt: &DocType{Name: "Customer", Fields: []Field{{Fieldname: "town", Fieldtype: "Link"}}},
			dimensions: []string{"town"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := generatedFacts(tc.dt)
			if tc.wantNil {
				if f != nil {
					t.Fatalf("model = %+v, want none", f)
				}
				return
			}
			if f == nil || !f.Generated || f.Time != "creation" {
				t.Fatalf("model = %+v, want generated on creation", f)
			}
			if got := fieldNames(f.Measures); strings.Join(got, ",") != strings.Join(tc.measures, ",") {
				t.Errorf("measures = %v, want %v", got, tc.measures)
			}
			if got := dimensionNames(f.Dimensions); strings.Join(got, ",") != strings.Join(tc.dimensions, ",") {
				t.Errorf("dimensions = %v, want %v", got, tc.dimensions)
			}
			if f.StatusField != tc.status {
				t.Errorf("status field = %q, want %q", f.StatusField, tc.status)
			}
		})
	}
}

func fieldNames(ms []FactMeasure) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Field)
	}
	return out
}

func dimensionNames(ds []FactDimension) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Field)
	}
	return out
}

func TestRegisterGeneratesOnlyWhereThePackDeclaresNone(t *testing.T) {
	declared := &FactModel{Time: "creation", Measures: []FactMeasure{{Field: "total"}}}
	reg := NewRegistry()
	reg.Register(&DocType{Name: "Expense", Fields: []Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	reg.Register(&DocType{Name: "Sale", Facts: declared, Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}})
	if dt := reg.Get("Expense"); dt.Facts == nil || !dt.Facts.Generated {
		t.Fatalf("Expense facts = %+v, want generated", dt.Facts)
	}
	if dt := reg.Get("Sale"); dt.Facts != declared || dt.Facts.Generated {
		t.Fatalf("Sale facts = %+v, want the declared model kept", dt.Facts)
	}
	// A later load that drops the qualifying field replaces the generated model, and a generated model
	// never carries over onto a record type that became single.
	reg.Register(&DocType{Name: "Expense", IsSingle: true, Fields: []Field{{Fieldname: "amount", Fieldtype: "Currency"}}})
	if dt := reg.Get("Expense"); dt.Facts != nil {
		t.Fatalf("single Expense facts = %+v, want none", dt.Facts)
	}
}

func TestGeneratedFactsAreNotSavedAsConfig(t *testing.T) {
	reg := NewRegistry()
	reg.LoadFull([]*DocType{
		{Name: "Expense", Fields: []Field{{Fieldname: "amount", Fieldtype: "Currency"}}},
		{Name: "Sale", Facts: &FactModel{Time: "creation", Measures: []FactMeasure{{Field: "total"}}}},
	}, nil, nil)
	expense := reg.Get("Expense")

	raw, err := json.Marshal(expense)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"facts"`) {
		t.Fatalf("json of generated record type has facts: %s", raw)
	}
	if strings.Contains(string(raw), `"generated"`) {
		t.Fatalf("json of generated record type names the flag: %s", raw)
	}
	out, err := yaml.Marshal(expense)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "measures") {
		t.Fatalf("yaml of generated record type has a facts block:\n%s", out)
	}
	if raw, err = json.Marshal(reg.Get("Sale")); err != nil || !strings.Contains(string(raw), `"facts"`) {
		t.Fatalf("json of declared record type = %s, %v; want facts kept", raw, err)
	}
	if expense.Facts == nil {
		t.Fatal("marshaling dropped the in-memory model")
	}
}
