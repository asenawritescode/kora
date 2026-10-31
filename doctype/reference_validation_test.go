package doctype

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

func TestPageComponentContractsMatchRenderedComponentRegistry(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "ui", "src", "components", "views", "registry.tsx"))
	if err != nil {
		t.Fatalf("read UI component registry: %v", err)
	}
	pattern := regexp.MustCompile(`(?m)^\s{2}([a-z_]+): create(?:Static|Lazy)Entry\(`)
	matches := pattern.FindAllSubmatch(source, -1)
	uiNames := map[string]bool{}
	for _, match := range matches {
		uiNames[string(match[1])] = true
	}
	for name := range uiNames {
		if _, ok := PageComponentContractFor(name); !ok {
			t.Errorf("UI component %q has no Engine contract", name)
		}
	}
	for _, contract := range PageComponentContracts() {
		if !uiNames[contract.Name] {
			t.Errorf("Engine component contract %q has no rendered UI component", contract.Name)
		}
		if contract.Version != 1 {
			t.Errorf("component %q contract version = %d; expected current runtime version 1", contract.Name, contract.Version)
		}
	}
	policySource, err := os.ReadFile(filepath.Join("..", "ui", "src", "manifest", "runtime", "policy.ts"))
	if err != nil {
		t.Fatalf("read UI component policy: %v", err)
	}
	dataBlockPattern := regexp.MustCompile(`(?s)const DATA_DISPLAY_COMPONENTS = new Set\(\[(.*?)\]\)`)
	dataBlock := dataBlockPattern.FindSubmatch(policySource)
	if dataBlock == nil {
		t.Fatal("could not locate DATA_DISPLAY_COMPONENTS in UI policy")
	}
	policyDataNames := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(dataBlock[1], -1) {
		policyDataNames[string(match[1])] = true
	}
	for name := range uiNames {
		contract, _ := PageComponentContractFor(name)
		if contract.RequiresData != policyDataNames[name] {
			t.Errorf("component %q data requirement differs: Engine=%v UI=%v", name, contract.RequiresData, policyDataNames[name])
		}
	}
}

func TestPageActionContractsCoverManifestRuntimeAllowlist(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "ui", "src", "manifest", "runtime", "policy.ts"))
	if err != nil {
		t.Fatalf("read UI page action policy: %v", err)
	}
	blockPattern := regexp.MustCompile(`(?s)export const ALLOWED_ACTION_COMMANDS = \[(.*?)\] as const`)
	block := blockPattern.FindSubmatch(source)
	if block == nil {
		t.Fatal("could not locate ALLOWED_ACTION_COMMANDS in UI policy")
	}
	commandPattern := regexp.MustCompile(`'([a-z_.]+)'`)
	for _, match := range commandPattern.FindAllSubmatch(block[1], -1) {
		name := string(match[1])
		if _, ok := PageActionContractFor(name); !ok {
			t.Errorf("UI action command %q has no Engine contract", name)
		}
	}
}

func TestRuntimeContractsUseSupportedSchemaKinds(t *testing.T) {
	validValues := map[string]bool{"string": true, "boolean": true, "number": true, "object": true, "array": true, "string_array": true, "any": true}
	for _, component := range PageComponentContracts() {
		if component.Version < 1 || component.Props == nil {
			t.Errorf("component %q has incomplete version or props schema", component.Name)
		}
		for binding, definition := range component.Bindings {
			switch definition.Kind {
			case "field", "field_list", "enum", "literal", "boolean", "integer", "json":
			default:
				t.Errorf("component %q binding %q has unsupported kind %q", component.Name, binding, definition.Kind)
			}
			if definition.Kind == "field" && len(definition.FieldTypes) == 0 {
				t.Errorf("component %q field binding %q does not declare accepted DocType field types", component.Name, binding)
			}
		}
	}
	for _, action := range PageActionContracts() {
		if action.Name == "" || (action.Execution != "client" && action.Execution != "server") {
			t.Errorf("action contract is incomplete: %#v", action)
		}
		for name, definition := range action.Inputs {
			if !validValues[definition.Type] {
				t.Errorf("action %q input %q has unsupported type %q", action.Name, name, definition.Type)
			}
		}
	}
}

func TestPageManifestRejectsUnsupportedRegisteredComponentVersion(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Product", Fields: []Field{{Fieldname: "product_name", Fieldtype: "Data"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Catalog", Version: "1.0.0", Package: "retail", Status: "draft"},
		Spec:     PageManifestSpec{Route: "/catalog", Runtime: ">=2.0.0 <3.0.0", Resources: []PageResource{{ID: "products", Query: "document.list", Params: map[string]any{"doctype": "Product"}}}, Layout: PageManifestLayout{Type: "single", Children: []PageComponent{{ID: "products", Component: "product_grid", Version: 2, Data: "products.data", Props: map[string]any{"source_doctype": "Product", "bindings": map[string]any{"title": "product_name"}}}}}},
	}
	for _, finding := range ValidatePageManifestReferences(manifest, registry) {
		if finding.Code == "reference.component_version_unsupported" {
			return
		}
	}
	t.Fatal("expected unsupported component version finding")
}

func TestPageManifestComponentContractValidatesBindingsAndFieldTypes(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Product", Fields: []Field{
		{Fieldname: "product_name", Fieldtype: "Data"},
		{Fieldname: "selling_price", Fieldtype: "Data"},
		{Fieldname: "category", Fieldtype: "Data"},
	}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Catalog", Version: "1.0.0", Package: "retail", Status: "draft"},
		Spec: PageManifestSpec{
			Route: "/catalog", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{{ID: "products", Query: "document.list", Params: map[string]any{"doctype": "Product"}}},
			Layout: PageManifestLayout{Type: "single", Children: []PageComponent{{ID: "products", Component: "product_grid", Version: 1, Data: "products.data", Props: map[string]any{
				"source_doctype": "Product", "bindings": map[string]any{"title": "product_name", "price": "selling_price", "unknown": "category"},
			}}}},
		},
	}
	findings := ValidatePageManifestReferences(manifest, registry)
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["reference.component_binding_invalid"] {
		t.Errorf("expected incompatible price field type finding, got %#v", findings)
	}
	if !codes["reference.component_binding_unsupported"] {
		t.Errorf("expected undeclared binding finding, got %#v", findings)
	}
}

func TestPageManifestActionContractValidatesMappedFieldsAndUnknownInputs(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Sale", Fields: []Field{
		{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"},
		{Fieldname: "total_amount", Fieldtype: "Currency"},
	}})
	registry.Register(&DocType{Name: "Sale Item", IsChildTable: true, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Int"},
		{Fieldname: "unit_price", Fieldtype: "Currency"},
	}})
	registry.Register(&DocType{Name: "Product", Fields: []Field{{Fieldname: "product_name", Fieldtype: "Data"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "POS", Version: "1.0.0", Package: "retail", Status: "draft"},
		Spec: PageManifestSpec{
			Route: "/pos", Runtime: ">=2.0.0 <3.0.0",
			Actions: []PageAction{{ID: "complete", Command: "create_transaction", Input: map[string]any{
				"target_doctype": "Sale", "child_table": "Sale Item", "line_fields": map[string]any{"missing_line_field": "product"},
				"line_defaults": map[string]any{"quantity": "many"}, "bogus": true,
			}}},
			Layout: PageManifestLayout{Type: "single", Children: []PageComponent{{ID: "payment", Component: "payment_panel", Version: 1, Actions: []string{"complete"}, Props: map[string]any{}}}},
		},
	}
	findings := ValidatePageManifestReferences(manifest, registry)
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["reference.field_missing"] {
		t.Errorf("expected missing child field finding, got %#v", findings)
	}
	if !codes["reference.action_input_unsupported"] {
		t.Errorf("expected undeclared action input finding, got %#v", findings)
	}
	if !codes["reference.action_field_value_type"] {
		t.Errorf("expected line default value type finding, got %#v", findings)
	}
}

func TestValidateSnapshotReferencesReportsMissingComponentAndActionReferences(t *testing.T) {
	snapshot := &ConfigSnapshot{
		DocTypes: []*DocType{
			{Name: "Product", Fields: []Field{{Fieldname: "product_name", Fieldtype: "Data"}}},
			{Name: "Sale", Fields: []Field{{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"}}},
			{Name: "Sale Item", IsChildTable: true, Fields: []Field{{Fieldname: "product", Fieldtype: "Link", Options: "Product"}}},
		},
		Views: []*View{{Name: "Point of Sale", Route: "/pos", SourceDocType: "Product", Components: []ViewComponent{{
			ID: "products", Type: "product_grid", Region: "main", SourceDocType: "Product",
			Bindings: map[string]string{"title": "missing_title"},
			Actions: []ViewAction{{ID: "complete", Trigger: "on_click", Type: "create_transaction", Config: map[string]any{
				"target_doctype": "Missing Sale", "child_table": "Missing Line",
			}}},
		}}}},
	}
	findings := ValidateSnapshotReferences(snapshot, nil)
	got := map[string]string{}
	for _, finding := range findings {
		got[finding.Code] = finding.Path
	}
	if got["reference.field_missing"] != "views[Point of Sale].components[products].props.bindings.title" {
		t.Fatalf("field finding = %q; findings: %#v", got["reference.field_missing"], findings)
	}
	if got["reference.doctype_missing"] == "" {
		t.Fatalf("missing DocType finding absent: %#v", findings)
	}
}

func TestValidateSnapshotReferencesRejectsPageAndDocTypeRouteCollision(t *testing.T) {
	snapshot := &ConfigSnapshot{
		DocTypes: []*DocType{{Name: "Point Of Sale"}},
		Views:    []*View{{Name: "POS", Route: "/point-of-sale", SourceDocType: "Point Of Sale"}},
	}
	findings := ValidateSnapshotReferences(snapshot, nil)
	for _, finding := range findings {
		if finding.Code == "reference.route_ambiguous" {
			return
		}
	}
	t.Fatalf("expected route collision finding, got %#v", findings)
}

func TestValidatePageManifestReferencesRequiresDeclaredResource(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Product", Fields: []Field{{Fieldname: "product_name", Fieldtype: "Data"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "POS", Version: "1.0.0", Package: "retail", Status: "draft"},
		Spec:     PageManifestSpec{Route: "/pos", Runtime: ">=2.0.0 <3.0.0", Layout: PageManifestLayout{Type: "two_panel", Children: []PageComponent{{ID: "products", Component: "product_grid", Data: "missing.data", Props: map[string]any{"source_doctype": "Product", "bindings": map[string]any{"title": "product_name"}}}}}},
	}
	findings := ValidatePageManifestReferences(manifest, registry)
	for _, finding := range findings {
		if finding.Code == "reference.resource_missing" && finding.Path == "spec.layout.children[products].data" {
			return
		}
	}
	t.Fatalf("expected missing resource finding, got %#v", findings)
}

func TestPOSPageManifestPassesWhenCategoryComesFromProductResource(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Product", Fields: []Field{
		{Fieldname: "product_name", Fieldtype: "Data"}, {Fieldname: "category", Fieldtype: "Data"},
		{Fieldname: "selling_price", Fieldtype: "Currency"}, {Fieldname: "sku", Fieldtype: "Data"}, {Fieldname: "stock_qty", Fieldtype: "Int"},
	}})
	registry.Register(&DocType{Name: "Sale", Fields: []Field{{Fieldname: "items", Fieldtype: "Table", Options: "Sale Item"}, {Fieldname: "total_amount", Fieldtype: "Currency"}}})
	registry.Register(&DocType{Name: "Sale Item", IsChildTable: true, Fields: []Field{{Fieldname: "product", Fieldtype: "Link", Options: "Product"}, {Fieldname: "quantity", Fieldtype: "Int"}, {Fieldname: "unit_price", Fieldtype: "Currency"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Point of Sale", Version: "1.0.0", Package: "retail", Status: "active"},
		Spec: PageManifestSpec{
			Route: "/pos", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{{ID: "primary", Query: "document.list", Params: map[string]any{"doctype": "Product"}}},
			Actions: []PageAction{
				{ID: "tap", Command: "local_cart_add", Input: map[string]any{}},
				{ID: "complete_sale", Command: "create_transaction", Input: map[string]any{
					"target_doctype": "Sale", "child_table": "Sale Item", "line_defaults": map[string]any{"quantity": 1},
					"line_fields": map[string]any{"product": "product", "quantity": "quantity", "unit_price": "rate"},
					"totals":      map[string]any{"lines": map[string]any{"quantity_field": "quantity", "unit_price_field": "unit_price"}, "total_field": "total_amount"},
				}, Invalidate: []string{"primary"}},
			},
			Layout: PageManifestLayout{Type: "two_panel", Children: []PageComponent{
				{ID: "categories", Component: "category_tabs", Version: 1, Data: "primary.data", Props: map[string]any{"source_doctype": "Product", "bindings": map[string]any{"group_field": "category"}}},
				{ID: "products", Component: "product_grid", Version: 1, Data: "primary.data", Actions: []string{"tap"}, Props: map[string]any{"source_doctype": "Product", "bindings": map[string]any{"title": "product_name", "price": "selling_price", "sku": "sku", "badge": "stock_qty"}}},
				{ID: "payment", Component: "payment_panel", Version: 1, Actions: []string{"complete_sale"}, Props: map[string]any{}},
			}},
		},
	}
	if findings := ValidatePageManifestReferences(manifest, registry); len(findings) != 0 {
		t.Fatalf("valid POS manifest returned findings: %#v", findings)
	}
}

func TestValidateSnapshotReferencesChecksLinkedFieldAgainstLinkTarget(t *testing.T) {
	product := &DocType{Name: "Product", Fields: []Field{{Fieldname: "selling_price", Fieldtype: "Currency"}}}
	line := &DocType{Name: "Sale Item", IsChildTable: true, Fields: []Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "unit_price", Fieldtype: "Currency", LinkedField: "product.selling_price"},
	}}
	snapshot := &ConfigSnapshot{DocTypes: []*DocType{product, line}}
	if findings := ValidateSnapshotReferences(snapshot, nil); len(findings) != 0 {
		t.Fatalf("valid linked_field should pass: %#v", findings)
	}
	line.Fields[1].LinkedField = "product.missing_price"
	findings := ValidateSnapshotReferences(snapshot, nil)
	if len(findings) == 0 || findings[0].Code != "reference.field_missing" {
		t.Fatalf("missing linked target field should be rejected: %#v", findings)
	}
}

func TestCategoryTabsValidatesLinkedCategoryResourceContract(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Product", Fields: []Field{
		{Fieldname: "product_name", Fieldtype: "Data"},
		{Fieldname: "category", Fieldtype: "Link", Options: "Product Category"},
	}})
	registry.Register(&DocType{Name: "Product Category", Fields: []Field{{Fieldname: "category_name", Fieldtype: "Data"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "POS", Version: "1.0.0", Package: "retail", Status: "draft"},
		Spec: PageManifestSpec{
			Route: "/pos", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{
				{ID: "products", Query: "document.list", Params: map[string]any{"doctype": "Product"}},
				{ID: "categories", Query: "document.list", Params: map[string]any{"doctype": "Product Category", "fields": []any{"name", "category_name"}}},
			},
			Layout: PageManifestLayout{Type: "two_panel", Children: []PageComponent{{
				ID: "category-tabs", Component: "category_tabs", Version: 1, Data: "products.data",
				ResourceBindings: map[string]string{"products": "products", "categories": "categories"},
				Props: map[string]any{"source_doctype": "Product", "bindings": map[string]any{
					"group_field": "category", "category_label_field": "category_name",
				}},
			}}},
		},
	}
	if findings := ValidatePageManifestReferences(manifest, registry); len(findings) != 0 {
		t.Fatalf("valid linked-category manifest returned findings: %#v", findings)
	}

	cases := []struct {
		name   string
		mutate func(*PageManifest)
		code   string
	}{
		{
			name: "link target resource required",
			mutate: func(candidate *PageManifest) {
				delete(candidate.Spec.Layout.Children[0].ResourceBindings, "categories")
			},
			code: "reference.component_resource_required",
		},
		{
			name: "label field required with category resource",
			mutate: func(candidate *PageManifest) {
				delete(candidate.Spec.Layout.Children[0].Props["bindings"].(map[string]any), "category_label_field")
			},
			code: "reference.component_binding_required",
		},
		{
			name: "category label field must exist",
			mutate: func(candidate *PageManifest) {
				candidate.Spec.Layout.Children[0].Props["bindings"].(map[string]any)["category_label_field"] = "missing_label"
			},
			code: "reference.field_missing",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var candidate PageManifest
			if err := json.Unmarshal(encoded, &candidate); err != nil {
				t.Fatal(err)
			}
			testCase.mutate(&candidate)
			findings := ValidatePageManifestReferences(&candidate, registry)
			for _, finding := range findings {
				if finding.Code == testCase.code {
					return
				}
			}
			t.Fatalf("expected %s finding, got %#v", testCase.code, findings)
		})
	}
}

func TestPageManifestToViewProjectsTypedActionConfiguration(t *testing.T) {
	manifest := &PageManifest{
		Spec: PageManifestSpec{
			Actions: []PageAction{{ID: "complete", Command: "create_transaction", Input: map[string]any{"target_doctype": "Sale"}}},
			Layout:  PageManifestLayout{Children: []PageComponent{{ID: "payment", Component: "payment_panel", Actions: []string{"complete"}}}},
		},
	}
	view := manifest.ToView()
	if len(view.Components) != 1 || len(view.Components[0].Actions) != 1 {
		t.Fatalf("page action was not projected to the component: %#v", view.Components)
	}
	action := view.Components[0].Actions[0]
	if action.ID != "complete" || action.Type != "create_transaction" || action.Config["target_doctype"] != "Sale" {
		t.Fatalf("projected action = %#v", action)
	}
}

func TestConfigSnapshotRoundTripsPageManifestReferences(t *testing.T) {
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "POS", Version: "1.0.0", Package: "retail", Status: "active"},
		Spec:     PageManifestSpec{Route: "/pos", Runtime: ">=2.0.0 <3.0.0", Resources: []PageResource{{ID: "primary", Query: "document.list", Params: map[string]any{"doctype": "Product"}}}, Layout: PageManifestLayout{Type: "single", Children: []PageComponent{{ID: "products", Component: "product_grid", Version: 1, Data: "primary.data", Props: map[string]any{}}}}},
	}
	snapshot := &ConfigSnapshot{Views: []*View{manifest.ToView()}}
	parsed, err := FromSExpr(ToSExpr(snapshot))
	if err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	if len(parsed.Views) != 1 || parsed.Views[0].PageManifest == nil {
		t.Fatalf("page manifest missing from round trip: %#v", parsed.Views)
	}
	if !reflect.DeepEqual(manifest, parsed.Views[0].PageManifest) {
		t.Fatalf("round-tripped manifest differs:\n got %#v\nwant %#v", parsed.Views[0].PageManifest, manifest)
	}
}

func TestReferenceContractsAreGenericAcrossCustomDomainNames(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Catalog Item", Fields: []Field{
		{Fieldname: "display_label", Fieldtype: "Data"},
		{Fieldname: "unit_cost", Fieldtype: "Currency"},
		{Fieldname: "family", Fieldtype: "Select"},
		{Fieldname: "available_units", Fieldtype: "Int"},
		{Fieldname: "external_code", Fieldtype: "Data"},
	}})
	registry.Register(&DocType{Name: "Invoice", Fields: []Field{
		{Fieldname: "lines", Fieldtype: "Table", Options: "Invoice Line"},
		{Fieldname: "gross_total", Fieldtype: "Currency"},
	}})
	registry.Register(&DocType{Name: "Invoice Line", IsChildTable: true, Fields: []Field{
		{Fieldname: "item", Fieldtype: "Link", Options: "Catalog Item"},
		{Fieldname: "units", Fieldtype: "Int"},
		{Fieldname: "unit_cost", Fieldtype: "Currency"},
	}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Catalog Register", Version: "1.0.0", Package: "wholesale", Status: "draft"},
		Spec: PageManifestSpec{
			Route: "/catalog-register", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{{ID: "catalog", Query: "document.list", Params: map[string]any{"doctype": "Catalog Item"}}},
			Actions: []PageAction{
				{ID: "add", Command: "local_cart_add", Input: map[string]any{}},
				{ID: "invoice", Command: "create_transaction", Input: map[string]any{
					"target_doctype": "Invoice", "child_table": "Invoice Line",
					"line_defaults": map[string]any{"units": 1},
					"line_fields":   map[string]any{"item": "product", "units": "quantity", "unit_cost": "rate"},
					"totals":        map[string]any{"lines": map[string]any{"quantity_field": "units", "unit_price_field": "unit_cost"}, "total_field": "gross_total"},
				}},
			},
			Layout: PageManifestLayout{Type: "two_panel", Children: []PageComponent{
				{ID: "families", Component: "category_tabs", Version: 1, Data: "catalog.data", Props: map[string]any{"bindings": map[string]any{"group_field": "family"}}},
				{ID: "items", Component: "product_grid", Version: 1, Data: "catalog.data", Actions: []string{"add"}, Props: map[string]any{"bindings": map[string]any{
					"title": "display_label", "price": "unit_cost", "badge": "available_units", "sku": "external_code", "subtitle": "family",
				}}},
				{ID: "checkout", Component: "payment_panel", Version: 1, Actions: []string{"invoice"}, Props: map[string]any{}},
			}},
		},
	}
	if findings := ValidatePageManifestReferences(manifest, registry); len(findings) != 0 {
		t.Fatalf("valid non-retail domain manifest returned findings: %#v", findings)
	}
}

func TestSnapshotComponentContractsApplyToLegacyViews(t *testing.T) {
	snapshot := &ConfigSnapshot{
		DocTypes: []*DocType{{Name: "Library Copy", Fields: []Field{
			{Fieldname: "caption", Fieldtype: "Data"},
			{Fieldname: "replacement_cost", Fieldtype: "Currency"},
		}}},
		Views: []*View{{Name: "Collection", Route: "/collection", SourceDocType: "Library Copy", Components: []ViewComponent{{
			ID: "copies", Type: "product_grid", Region: "main", SourceDocType: "Library Copy",
			Bindings: map[string]string{"title": "caption", "price": "replacement_cost", "not_a_contract_binding": "caption"},
		}}}},
	}
	findings := ValidateSnapshotReferences(snapshot, nil)
	codes := map[string]bool{}
	for _, finding := range findings {
		codes[finding.Code] = true
	}
	if !codes["reference.component_binding_unsupported"] {
		t.Fatalf("legacy View did not enforce component registry binding contract: %#v", findings)
	}
	if codes["reference.component_binding_invalid"] || codes["reference.field_missing"] {
		t.Fatalf("valid custom fields were rejected: %#v", findings)
	}
}

func TestSystemFieldBindingsAreRecognizedWithoutDocTypeDeclarations(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Archive Entry", Fields: []Field{{Fieldname: "caption", Fieldtype: "Data"}}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Recent Archive", Version: "1.0.0", Package: "archive", Status: "draft"},
		Spec: PageManifestSpec{Route: "/recent-archive", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{{ID: "entries", Query: "document.list", Params: map[string]any{"doctype": "Archive Entry"}}},
			Layout:    PageManifestLayout{Type: "single", Children: []PageComponent{{ID: "recent", Component: "recent_records", Version: 1, Data: "entries.data", Props: map[string]any{}}}},
		},
	}
	for _, finding := range ValidatePageManifestReferences(manifest, registry) {
		if finding.Code == "reference.field_missing" {
			t.Fatalf("system field binding was treated as missing: %#v", finding)
		}
	}
}

func TestDomainSpecificComponentsRequireExplicitFieldBindings(t *testing.T) {
	registry := NewRegistry()
	registry.Register(&DocType{Name: "Inventory Unit", Fields: []Field{
		{Fieldname: "label", Fieldtype: "Data"},
		{Fieldname: "amount", Fieldtype: "Currency"},
		{Fieldname: "group", Fieldtype: "Data"},
	}})
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1", Kind: "Page",
		Metadata: PageManifestMetadata{Name: "Inventory", Version: "1.0.0", Package: "inventory", Status: "draft"},
		Spec: PageManifestSpec{Route: "/inventory", Runtime: ">=2.0.0 <3.0.0",
			Resources: []PageResource{{ID: "items", Query: "document.list", Params: map[string]any{"doctype": "Inventory Unit"}}},
			Layout: PageManifestLayout{Type: "single", Children: []PageComponent{
				{ID: "groups", Component: "category_tabs", Version: 1, Data: "items.data", Props: map[string]any{}},
				{ID: "items", Component: "product_grid", Version: 1, Data: "items.data", Props: map[string]any{}},
			}},
		},
	}
	missing := map[string]bool{}
	for _, finding := range ValidatePageManifestReferences(manifest, registry) {
		if finding.Code == "reference.component_binding_required" {
			missing[finding.Target] = true
		}
	}
	for _, binding := range []string{"title", "price", "group_field"} {
		if !missing[binding] {
			t.Errorf("missing required field binding %q was not reported", binding)
		}
	}
}
