package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
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

func TestHistoricalBusinessSnapshotProjectsWithoutProviderResolution(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	snapshot := contract.HistoricalSnapshot{Source: contract.BusinessRef{ProviderID: "warehouse-old", ResourceType: "warehouse.location", ID: "same-id", Scope: scope}, BusinessScope: scope, CapturedAt: time.Now().UTC(), Schema: contract.SchemaRef{ID: "warehouse.location", Version: 1}, Data: json.RawMessage(`{"label":"Issued location"}`)}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dt := &doctype.DocType{Name: "Issued Invoice", Fields: []doctype.Field{{Fieldname: "warehouse_snapshot", Fieldtype: "JSON"}}}
	doc := doctype.NewDocument(dt.Name)
	doc.Name = "INV-1"
	doc.Set("warehouse_snapshot", json.RawMessage(raw))
	projected := docToMap(doc, dt, nil, nil)
	var got contract.HistoricalSnapshot
	encoded, err := json.Marshal(projected["warehouse_snapshot"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil || got.Source.ProviderID != "warehouse-old" || string(got.Data) != `{"label":"Issued location"}` {
		t.Fatalf("retained historical snapshot was not readable: %#v, %v", got, err)
	}
}
