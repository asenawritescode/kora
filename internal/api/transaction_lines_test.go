package api

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestConfiguredTransactionLineRecords(t *testing.T) {
	registry := doctype.NewRegistry()
	ledger := &doctype.DocType{Name: "Ledger Entry", Fields: []doctype.Field{
		{Fieldname: "item", Fieldtype: "Data"}, {Fieldname: "recorded_by", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Float"},
	}}
	registry.Register(ledger)
	lineDT := &doctype.DocType{Name: "Order Line", Fields: []doctype.Field{{Fieldname: "item", Fieldtype: "Data"}, {Fieldname: "quantity", Fieldtype: "Float"}}}
	parentDT := &doctype.DocType{Name: "Order", Fields: []doctype.Field{{Fieldname: "customer", Fieldtype: "Data"}}}
	newLine := func(item string, qty float64) *doctype.Document {
		line := doctype.NewDocument("Order Line")
		line.Set("item", item)
		line.Set("quantity", qty)
		return line
	}
	binding := map[string]any{
		"key": "ledger", "doctype": "Ledger Entry",
		"fields":   map[string]any{"item": "$line.item", "recorded_by": "$actor"},
		"quantity": map[string]any{"field": "qty", "source_field": "quantity", "sign": float64(-1)},
	}
	lines := []*doctype.Document{newLine("soda", 2), newLine("water", 3)}
	full := transactionLines{Registry: registry, ParentDT: parentDT, Parent: doctype.NewDocument("Order"), LineDT: lineDT, Lines: lines, Actor: "clerk"}

	tests := []struct {
		name     string
		config   map[string]any
		src      transactionLines
		wantKeys []string
		wantErr  bool
	}{
		{"no line_records configured creates nothing", map[string]any{}, full, nil, false},
		{"zero-value lines are rejected when records are configured", map[string]any{"line_records": []any{binding}}, transactionLines{Registry: registry}, nil, true},
		{"one record per line", map[string]any{"line_records": []any{binding}}, full, []string{"ledger_1", "ledger_2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := configuredTransactionLineRecords(tt.config, tt.src)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(items) != len(tt.wantKeys) {
				t.Fatalf("items = %d, want %d", len(items), len(tt.wantKeys))
			}
			for i, key := range tt.wantKeys {
				if items[i].Key != key {
					t.Errorf("item %d key = %q, want %q", i, items[i].Key, key)
				}
			}
		})
	}

	items, err := configuredTransactionLineRecords(map[string]any{"line_records": []any{binding}}, full)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(items[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["item"] != "soda" || data["recorded_by"] != "clerk" || data["qty"] != float64(-2) {
		t.Fatalf("record data = %v", data)
	}
}

func TestSavePageManifestDraftRequiresStoreAndSnapshot(t *testing.T) {
	tests := []struct {
		name  string
		store *configstore.Store
		draft draftVersion
	}{
		{"zero draft", &configstore.Store{}, draftVersion{}},
		{"missing store", nil, draftVersion{Snapshot: &doctype.ConfigSnapshot{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := savePageManifestDraft(tt.store, &doctype.PageManifest{}, tt.draft); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
