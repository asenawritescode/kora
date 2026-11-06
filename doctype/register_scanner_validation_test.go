package doctype

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

const scannerRegisterViewYAML = `
name: Counter
route: /counter
type: register
layout: two_panel
resources:
  - id: lookup
    query: published.query
    params: {owner_module_id: shop.core, query_name: item_lookup, input: {}}
components:
  - id: scan
    type: scanner_input
    version: 2
    resource_bindings: {lookup: lookup}
    bindings: {term_input: term, title: item_name, price: price, sku: barcode}
`

func scannerRegisterModule() contract.ModuleDescriptor {
	module := publishedRegisterModule(true, true)
	input := contract.SchemaRef{ID: "shop.lookup.input", Version: 1}
	module.Queries = append(module.Queries, contract.PublishedQuery{
		OwnerModuleID: "shop.core", Name: "item_lookup", InputSchema: input,
		AllowedFields: []string{"name", "item_name", "price", "barcode"},
	})
	module.Schemas = append(module.Schemas, contract.ModuleSchemaDocument{Ref: input, Schema: json.RawMessage(`{"type":"object","properties":{"term":{"type":"string"}}}`)})
	return module
}

func TestPublishedScannerInputReferences(t *testing.T) {
	scan := func(view *View) *ViewComponent { return &view.Components[0] }
	tests := []struct {
		name     string
		mutate   func(*View)
		wantCode string
	}{
		{name: "valid scanner"},
		{name: "lookup through data resource", mutate: func(v *View) {
			scan(v).ResourceBindings = nil
			scan(v).Data = "lookup.rows"
		}},
		{name: "sku is optional", mutate: func(v *View) { delete(scan(v).Bindings, "sku") }},
		{name: "term_input is not a query input", mutate: func(v *View) { scan(v).Bindings["term_input"] = "barcode" }, wantCode: "reference.published_input_missing"},
		{name: "title outside allowed fields", mutate: func(v *View) { scan(v).Bindings["title"] = "label" }, wantCode: "reference.published_field_missing"},
		{name: "price outside allowed fields", mutate: func(v *View) { scan(v).Bindings["price"] = "cost" }, wantCode: "reference.published_field_missing"},
		{name: "sku outside allowed fields", mutate: func(v *View) { scan(v).Bindings["sku"] = "ean" }, wantCode: "reference.published_field_missing"},
		{name: "missing term_input", mutate: func(v *View) { delete(scan(v).Bindings, "term_input") }, wantCode: "reference.component_binding_required"},
		{name: "missing title", mutate: func(v *View) { delete(scan(v).Bindings, "title") }, wantCode: "reference.component_binding_required"},
		{name: "missing price", mutate: func(v *View) { delete(scan(v).Bindings, "price") }, wantCode: "reference.component_binding_required"},
		{name: "missing lookup resource", mutate: func(v *View) { scan(v).ResourceBindings = nil }, wantCode: "reference.component_resource_required"},
		{name: "undeclared lookup resource", mutate: func(v *View) { scan(v).ResourceBindings["lookup"] = "nope" }, wantCode: "reference.resource_missing"},
		{name: "unsupported binding", mutate: func(v *View) { scan(v).Bindings["search_fields"] = "barcode" }, wantCode: "reference.component_binding_unsupported"},
		{name: "unknown published query", mutate: func(v *View) { v.Resources[0].Params["query_name"] = "nope" }, wantCode: "reference.published_query_missing"},
		{name: "document list resource", mutate: func(v *View) {
			v.Resources[0] = PageResource{ID: "lookup", Query: "document.list", Params: map[string]any{}}
		}, wantCode: "reference.component_resource_incompatible"},
		{name: "version 1 keeps legacy bindings", mutate: func(v *View) {
			scan(v).Version = 0
			scan(v).ResourceBindings = nil
			scan(v).Bindings = map[string]string{"placeholder": "Scan", "search_fields": "barcode"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view, err := ParseViewYAML([]byte(scannerRegisterViewYAML))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(view)
			}
			snapshot := &ConfigSnapshot{Views: []*View{view}, Modules: []contract.ModuleDescriptor{scannerRegisterModule()}}
			findings := ValidateSnapshotReferences(snapshot, nil)
			var codes []string
			for _, finding := range findings {
				codes = append(codes, finding.Code)
			}
			if tc.wantCode == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v", findings)
				}
				return
			}
			if !strings.Contains(strings.Join(codes, ","), tc.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, tc.wantCode)
			}
		})
	}
}

func TestPublishedScannerInputManifestCarriesVersionAndBindings(t *testing.T) {
	view, err := ParseViewYAML([]byte(scannerRegisterViewYAML))
	if err != nil {
		t.Fatal(err)
	}
	manifest := PageManifestFromView(view)
	scan := manifest.Spec.Layout.Children[0]
	bindings, _ := scan.Props["bindings"].(map[string]string)
	if scan.Component != "scanner_input" || scan.Version != 2 || scan.ResourceBindings["lookup"] != "lookup" ||
		bindings["term_input"] != "term" || bindings["title"] != "item_name" || bindings["price"] != "price" || bindings["sku"] != "barcode" {
		t.Fatalf("scanner component = %+v", scan)
	}
}
