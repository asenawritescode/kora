package kernel

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/doctype"
)

// applyDataRegistry is a parent with a child table; the child has an editable label and a
// read-only count (with a default on a second read-only field).
func applyDataRegistry(t *testing.T) (*doctype.DocType, *doctype.Registry) {
	t.Helper()
	parent := &doctype.DocType{Name: "Tally", Fields: []doctype.Field{
		{Fieldname: "items", Fieldtype: "Table", Options: "Tally Item"},
	}}
	child := &doctype.DocType{Name: "Tally Item", Fields: []doctype.Field{
		{Fieldname: "label", Fieldtype: "Data"},
		{Fieldname: "counted", Fieldtype: "Float", ReadOnly: true},
		{Fieldname: "status", Fieldtype: "Data", ReadOnly: true, Default: "open"},
	}}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{parent, child}, nil, nil)
	tally := reg.Get("Tally")
	return tally, reg
}

func storedTally() *doctype.Document {
	row := doctype.NewDocument("Tally Item")
	row.Name = "row-1"
	row.Set("label", "old")
	row.Set("counted", 7.0)
	row.Set("status", "closed")
	doc := doctype.NewDocument("Tally")
	doc.SetTable("items", []*doctype.Document{row})
	return doc
}

func TestApplyDataChildReadOnlyFields(t *testing.T) {
	dt, reg := applyDataRegistry(t)
	cases := []struct {
		name         string
		body         string
		skipReadOnly bool
		wantCounted  any
		wantStatus   any
		wantLabel    string
	}{
		{
			name:         "person keeps stored read-only values on a matching row",
			body:         `{"items":[{"name":"row-1","label":"new","counted":99,"status":"x"}]}`,
			skipReadOnly: true, wantCounted: 7.0, wantStatus: "closed", wantLabel: "new",
		},
		{
			name:         "person's new row gets default and empty read-only fields",
			body:         `{"items":[{"label":"fresh","counted":5}]}`,
			skipReadOnly: true, wantCounted: nil, wantStatus: "open", wantLabel: "fresh",
		},
		{
			name:         "unknown name counts as a new row",
			body:         `{"items":[{"name":"ghost","label":"fresh","counted":5}]}`,
			skipReadOnly: true, wantCounted: nil, wantStatus: "open", wantLabel: "fresh",
		},
		{
			name:         "trusted writer sets read-only fields as sent",
			body:         `{"items":[{"name":"row-1","label":"new","counted":99,"status":"x"}]}`,
			skipReadOnly: false, wantCounted: 99.0, wantStatus: "x", wantLabel: "new",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := storedTally()
			if err := applyData(dt, doc, json.RawMessage(tc.body), reg, tc.skipReadOnly); err != nil {
				t.Fatal(err)
			}
			rows := doc.GetTable("items")
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			row := rows[0]
			if got := row.Get("counted"); got != tc.wantCounted {
				t.Fatalf("counted = %v, want %v", got, tc.wantCounted)
			}
			if got := row.Get("status"); got != tc.wantStatus {
				t.Fatalf("status = %v, want %v", got, tc.wantStatus)
			}
			if got := row.Get("label"); got != tc.wantLabel {
				t.Fatalf("label = %v, want %v", got, tc.wantLabel)
			}
		})
	}
}
