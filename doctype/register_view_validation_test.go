package doctype

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

const publishedRegisterViewYAML = `
name: Counter
route: /counter
type: register
layout: two_panel
resources:
  - id: items
    query: published.query
    params: {owner_module_id: shop.core, query_name: item_search}
  - id: groups
    query: published.query
    params: {owner_module_id: shop.core, query_name: groups}
  - id: session
    query: published.query
    params: {owner_module_id: shop.core, query_name: my_session}
components:
  - id: groups
    type: category_tabs
    version: 2
    resource_bindings: {items: items, categories: groups}
    bindings: {group_field: group, category_label_field: label}
  - id: products
    type: product_grid
    version: 2
    resource_bindings: {items: items}
    bindings: {title: item_name, price: price, sku: barcode}
  - id: cart
    type: cart_panel
    version: 2
    region: side
  - id: tender
    type: tender_panel
    version: 2
    region: side
    bindings:
      submit_operation_ref: shop.core~shop.checkout~sell
      methods: "literal:Cash"
    context:
      session_ref: session.name
`

func publishedRegisterModule(lineEntry, payment bool) contract.ModuleDescriptor {
	query := func(name string, fields ...string) contract.PublishedQuery {
		return contract.PublishedQuery{OwnerModuleID: "shop.core", Name: name, AllowedFields: fields}
	}
	operation := contract.PublishedOperation{Name: "sell", Capability: "shop.checkout", Operation: "sell"}
	if lineEntry {
		operation.Presentation = contract.SurfacePresentation{Layout: "line_entry", LineEntry: &contract.LineEntryLayout{LinesPath: "lines", QuantityField: "quantity"}}
	}
	if payment {
		operation.Payment = &contract.PaymentOperationContract{LinesField: "payments", AmountField: "amount"}
	}
	input := contract.SchemaRef{ID: "shop.sell.input", Version: 1}
	return contract.ModuleDescriptor{
		ID:                  "shop.core",
		Queries:             []contract.PublishedQuery{query("item_search", "name", "item_name", "barcode", "price", "group"), query("groups", "name", "label"), query("my_session", "name")},
		PublishedOperations: []contract.PublishedOperation{operation},
		Provides:            []contract.CapabilityOffer{{Name: "shop.checkout", Operations: []contract.OperationSpec{{Name: "sell", Input: input}}}},
		Schemas:             []contract.ModuleSchemaDocument{{Ref: input, Schema: json.RawMessage(`{"type":"object","properties":{"lines":{"type":"array"},"payments":{"type":"array"},"session_ref":{"type":"string"}}}`)}},
	}
}

func TestPublishedRegisterViewReferences(t *testing.T) {
	component := func(view *View, id string) *ViewComponent {
		for i := range view.Components {
			if view.Components[i].ID == id {
				return &view.Components[i]
			}
		}
		t.Fatalf("component %q missing", id)
		return nil
	}
	tests := []struct {
		name      string
		lineEntry bool
		payment   bool
		mutate    func(*View)
		wantCode  string
	}{
		{name: "valid published register", lineEntry: true, payment: true},
		{name: "grid field outside allowed fields", lineEntry: true, payment: true, mutate: func(v *View) { component(v, "products").Bindings["price"] = "cost" }, wantCode: "reference.published_field_missing"},
		{name: "category label outside allowed fields", lineEntry: true, payment: true, mutate: func(v *View) { component(v, "groups").Bindings["category_label_field"] = "title" }, wantCode: "reference.published_field_missing"},
		{name: "missing required grid binding", lineEntry: true, payment: true, mutate: func(v *View) { delete(component(v, "products").Bindings, "title") }, wantCode: "reference.component_binding_required"},
		{name: "unknown published query", lineEntry: true, payment: true, mutate: func(v *View) { v.Resources[0].Params["query_name"] = "nope" }, wantCode: "reference.published_query_missing"},
		{name: "document list resource on v2 role", lineEntry: true, payment: true, mutate: func(v *View) {
			v.Resources[0] = PageResource{ID: "items", Query: "document.list", Params: map[string]any{}}
		}, wantCode: "reference.component_resource_incompatible"},
		{name: "unknown operation", lineEntry: true, payment: true, mutate: func(v *View) {
			component(v, "tender").Bindings["submit_operation_ref"] = "shop.core~shop.checkout~refund"
		}, wantCode: "reference.operation_missing"},
		{name: "missing operation binding", lineEntry: true, payment: true, mutate: func(v *View) { delete(component(v, "tender").Bindings, "submit_operation_ref") }, wantCode: "reference.component_binding_required"},
		{name: "operation without line entry", payment: true, wantCode: "reference.operation_incompatible"},
		{name: "operation without payment", lineEntry: true, wantCode: "reference.operation_incompatible"},
		{name: "context target is not an operation input", lineEntry: true, payment: true, mutate: func(v *View) { component(v, "tender").Context = map[string]string{"shift": "session.name"} }, wantCode: "reference.operation_input_missing"},
		{name: "context source outside allowed fields", lineEntry: true, payment: true, mutate: func(v *View) { component(v, "tender").Context = map[string]string{"session_ref": "session.till"} }, wantCode: "reference.published_field_missing"},
		{name: "context source resource undeclared", lineEntry: true, payment: true, mutate: func(v *View) { component(v, "tender").Context = map[string]string{"session_ref": "shift.name"} }, wantCode: "reference.resource_missing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			view, err := ParseViewYAML([]byte(publishedRegisterViewYAML))
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(view)
			}
			snapshot := &ConfigSnapshot{Views: []*View{view}, Modules: []contract.ModuleDescriptor{publishedRegisterModule(tc.lineEntry, tc.payment)}}
			findings := ValidateSnapshotReferences(snapshot, nil)
			if tc.wantCode == "" {
				if len(findings) != 0 {
					t.Fatalf("findings = %+v", findings)
				}
				return
			}
			var codes []string
			for _, finding := range findings {
				codes = append(codes, finding.Code)
			}
			if !strings.Contains(strings.Join(codes, ","), tc.wantCode) {
				t.Fatalf("codes = %v, want %s", codes, tc.wantCode)
			}
		})
	}
}

func TestPublishedRegisterViewManifestCarriesVersionAndContext(t *testing.T) {
	view, err := ParseViewYAML([]byte(publishedRegisterViewYAML))
	if err != nil {
		t.Fatal(err)
	}
	manifest := PageManifestFromView(view)
	tender := manifest.Spec.Layout.Children[3]
	if tender.Version != 2 || tender.Props["context"].(map[string]string)["session_ref"] != "session.name" {
		t.Fatalf("tender component = %+v", tender)
	}
}
