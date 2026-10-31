package api

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestResourceReadProjectionsOmitHiddenAndPasswordFields(t *testing.T) {
	line := &doctype.DocType{Name: "Invoice Line", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "label", Fieldtype: "Data"},
		{Fieldname: "internal_cost", Fieldtype: "Currency", Hidden: true},
		{Fieldname: "credential", Fieldtype: "Password"},
	}}
	invoice := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "private_note", Fieldtype: "Data", Hidden: true},
		{Fieldname: "credential", Fieldtype: "Password"},
		{Fieldname: "lines", Fieldtype: "Table", Options: "Invoice Line"},
	}}
	registry := doctype.NewRegistry()
	registry.Register(line)
	registry.Register(invoice)

	child := doctype.NewDocument(line.Name)
	child.Name = "LINE-1"
	child.Fields = map[string]any{"label": "Visible line", "internal_cost": 29, "credential": "nested secret"}
	doc := doctype.NewDocument(invoice.Name)
	doc.Name = "INV-1"
	doc.Fields = map[string]any{"title": "Visible invoice", "private_note": "secret note", "credential": "secret", "owner": "alice"}
	doc.SetTable("lines", []*doctype.Document{child})

	for name, got := range map[string]map[string]any{
		"resource":                docToMap(doc, invoice, registry, nil),
		"view":                    documentToMap(doc, invoice),
		"explicit hidden request": docToMap(doc, invoice, registry, []string{"private_note", "credential", "title"}),
	} {
		t.Run(name, func(t *testing.T) {
			if got["title"] != "Visible invoice" {
				t.Fatalf("visible title missing: %#v", got)
			}
			for _, forbidden := range []string{"private_note", "credential"} {
				if _, exists := got[forbidden]; exists {
					t.Fatalf("projection exposed %q: %#v", forbidden, got)
				}
			}
			if name != "resource" {
				return // View projections and explicit field lists omit unrequested child tables.
			}
			rows, ok := got["lines"].([]map[string]any)
			if !ok || len(rows) != 1 || rows[0]["label"] != "Visible line" {
				t.Fatalf("visible child row missing: %#v", got["lines"])
			}
			for _, forbidden := range []string{"internal_cost", "credential"} {
				if _, exists := rows[0][forbidden]; exists {
					t.Fatalf("nested projection exposed %q: %#v", forbidden, rows[0])
				}
			}
		})
	}
}
