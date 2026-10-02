package doctype

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// View Validation
// ---------------------------------------------------------------------------

func TestViewValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		view    View
		wantErr string
	}{
		{
			name:    "empty view",
			view:    View{},
			wantErr: "view name is required",
		},
		{
			name:    "missing route",
			view:    View{Name: "POS Register"},
			wantErr: `view route is required for "POS Register"`,
		},
		{
			name:    "missing type",
			view:    View{Name: "POS Register", Route: "/pos"},
			wantErr: `view type is required for "POS Register"`,
		},
		{
			name: "valid minimal view",
			view: View{Name: "POS Register", Route: "/pos", Type: "workspace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.view.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if err.Error() != tt.wantErr {
				t.Errorf("expected error %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestViewValidate_DefaultsApplied(t *testing.T) {
	v := View{
		Name:  "My Dashboard",
		Route: "/dashboard",
		Type:  "dashboard",
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Layout != "single" {
		t.Errorf("expected default layout 'single', got %q", v.Layout)
	}
	if v.Module != "Workspace" {
		t.Errorf("expected default module 'Workspace', got %q", v.Module)
	}
}

func TestViewValidate_KeepsExplicitDefaults(t *testing.T) {
	v := View{
		Name:   "My Dashboard",
		Route:  "/dashboard",
		Type:   "dashboard",
		Layout: "three_panel",
		Module: "Sales",
	}
	if err := v.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Layout != "three_panel" {
		t.Errorf("expected layout 'three_panel', got %q", v.Layout)
	}
	if v.Module != "Sales" {
		t.Errorf("expected module 'Sales', got %q", v.Module)
	}
}

func TestPageManifestEnsurePrimaryDataBindings_RepairsMissingFormBinding(t *testing.T) {
	manifest := &PageManifest{
		APIVersion: "ui.kora.dev/v1",
		Kind:       "Page",
		Metadata: PageManifestMetadata{
			Name:    "dummy-form",
			Version: "0.1.0",
			Package: "tenant.workspace",
			Status:  "draft",
		},
		Spec: PageManifestSpec{
			Route:   "/dummy-form",
			Runtime: ">=2.0.0 <3.0.0",
			Layout: PageManifestLayout{
				Type: "single",
				Children: []PageComponent{
					{
						ID:        "record_form_1",
						Component: "record_form",
						Version:   1,
						Region:    "main",
						Position:  0,
						Props: map[string]any{
							"title":          "Dummy",
							"source_doctype": "Dummy",
						},
					},
				},
			},
		},
	}

	manifest.EnsurePrimaryDataBindings()

	if len(manifest.Spec.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(manifest.Spec.Resources))
	}
	if manifest.Spec.Resources[0].ID != "primary" {
		t.Fatalf("expected primary resource id, got %q", manifest.Spec.Resources[0].ID)
	}
	if manifest.Spec.Layout.Children[0].Data != "primary.data" {
		t.Fatalf("expected data binding to be repaired, got %q", manifest.Spec.Layout.Children[0].Data)
	}
}

func TestPageManifestEnsurePrimaryDataBindingsRepairsDataBackedCommerceComponents(t *testing.T) {
	manifest := &PageManifest{
		Spec: PageManifestSpec{
			Resources: []PageResource{{ID: "primary", Query: "document.list", Params: map[string]any{"doctype": "Product"}}, {ID: "products", Query: "document.list", Params: map[string]any{"doctype": "Product"}}},
			Layout: PageManifestLayout{Children: []PageComponent{
				{ID: "categories", Component: "category_tabs", Props: map[string]any{}},
				{ID: "products", Component: "product_grid", Props: map[string]any{}},
				{ID: "cart", Component: "cart_panel", Props: map[string]any{}},
				{ID: "payment", Component: "payment_panel", Props: map[string]any{}},
			}},
		},
	}

	manifest.EnsurePrimaryDataBindings()

	if got := manifest.Spec.Layout.Children[0].Data; got != "primary.data" {
		t.Fatalf("category tabs binding = %q, want primary.data", got)
	}
	if got := manifest.Spec.Layout.Children[1].Data; got != "primary.data" {
		t.Fatalf("product grid binding = %q, want primary.data", got)
	}
	if got := manifest.Spec.Layout.Children[0].Props["source_doctype"]; got != "Product" {
		t.Fatalf("category tabs source doctype = %#v, want Product", got)
	}
	if got := manifest.Spec.Layout.Children[2].Data; got != "" {
		t.Fatalf("cart panel binding = %q, want empty local-state component binding", got)
	}
	if got := manifest.Spec.Layout.Children[3].Data; got != "" {
		t.Fatalf("payment panel binding = %q, want empty action component binding", got)
	}
}

func TestPageManifestFromViewRoundTripPreservesCurrentProjection(t *testing.T) {
	view := &View{
		Name:          "Sales Dashboard",
		Route:         "/sales",
		Type:          "dashboard",
		Layout:        "two_panel",
		Module:        "Sales",
		SourceDocType: "Sales Invoice",
		PublicAccess: &ViewPublicAccess{
			Enabled:        true,
			Components:     []string{"record_form"},
			AllowMutations: true,
		},
		Components: []ViewComponent{
			{
				ID:            "main",
				Type:          "record_form",
				Region:        "main",
				SourceDocType: "Sales Invoice",
			},
		},
	}

	manifest := PageManifestFromView(view)
	if manifest == nil {
		t.Fatal("expected manifest")
	}
	if manifest.APIVersion != "ui.kora.dev/v1" || manifest.Kind != "Page" {
		t.Fatalf("unexpected manifest header: %+v", manifest)
	}
	if manifest.Metadata.Name != view.Name || manifest.Metadata.Package != "tenant.sales" {
		t.Fatalf("unexpected metadata projection: %+v", manifest.Metadata)
	}
	if manifest.Spec.Resources[0].Params["doctype"] != "Sales Invoice" {
		t.Fatalf("expected primary doctype projection, got %+v", manifest.Spec.Resources[0])
	}

	back := manifest.ToView()
	if back.Name != view.Name || back.Route != view.Route {
		t.Fatalf("round-trip name/route mismatch: got %+v want %+v", back, view)
	}
	if back.Type != "collection" {
		t.Fatalf("expected page-manifest projection to map to collection view type, got %q", back.Type)
	}
	if back.Module != "tenant.sales" {
		t.Fatalf("expected module normalization to survive projection, got %q", back.Module)
	}
	if back.PublicAccess == nil || !back.PublicAccess.Enabled || !back.PublicAccess.AllowMutations {
		t.Fatalf("expected public access projection to survive round-trip: %+v", back.PublicAccess)
	}
	if len(back.Components) != 1 || back.Components[0].Type != "record_form" {
		t.Fatalf("expected component projection to survive round-trip: %+v", back.Components)
	}
}

func TestPageManifestFromViewBindsEachComponentToItsDocTypeResource(t *testing.T) {
	view := &View{
		Name:          "Money",
		Route:         "/money",
		Type:          "workspace",
		SourceDocType: "Sale",
		Components: []ViewComponent{
			{ID: "income", Type: "record_table", SourceDocType: "Sale"},
			{ID: "expenses", Type: "record_table", SourceDocType: "Expense"},
		},
	}

	manifest := PageManifestFromView(view)
	if len(manifest.Spec.Resources) != 2 {
		t.Fatalf("resources = %#v, want primary Sale and expense Expense", manifest.Spec.Resources)
	}
	if got := manifest.Spec.Layout.Children[0].Data; got != "primary.data" {
		t.Fatalf("income data binding = %q, want primary.data", got)
	}
	if got := manifest.Spec.Layout.Children[1].Data; got != "expense.data" {
		t.Fatalf("expense data binding = %q, want expense.data", got)
	}
	if got := manifest.Spec.Resources[1].Params["doctype"]; got != "Expense" {
		t.Fatalf("expense resource doctype = %v, want Expense", got)
	}
}

func TestPageManifestFromViewPreservesComponentActionConfiguration(t *testing.T) {
	view := &View{
		Name: "Point of Sale",
		Components: []ViewComponent{{
			ID: "payment",
			Actions: []ViewAction{{
				ID: "complete_sale", Trigger: "on_click", Type: "create_transaction",
				Config: map[string]any{"target_doctype": "Sale"},
			}},
		}},
	}

	manifest := PageManifestFromView(view)
	actions, ok := manifest.Spec.Layout.Children[0].Props["actions"].([]map[string]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("component actions = %#v, want one configured action", manifest.Spec.Layout.Children[0].Props["actions"])
	}
	if actions[0]["id"] != "complete_sale" || actions[0]["type"] != "create_transaction" {
		t.Fatalf("action identity/type = %#v", actions[0])
	}
	config, ok := actions[0]["config"].(map[string]any)
	if !ok || config["target_doctype"] != "Sale" {
		t.Fatalf("action config = %#v, want Sale target", actions[0]["config"])
	}
}

// ---------------------------------------------------------------------------
// ViewComponent Validation
// ---------------------------------------------------------------------------

func TestViewComponentValidate_RequiredFields(t *testing.T) {
	tests := []struct {
		name    string
		comp    ViewComponent
		wantErr string
	}{
		{
			name:    "empty component",
			comp:    ViewComponent{},
			wantErr: "component id is required",
		},
		{
			name:    "missing type",
			comp:    ViewComponent{ID: "products"},
			wantErr: "component type is required",
		},
		{
			name: "valid minimal component",
			comp: ViewComponent{ID: "products", Type: "record_table"},
		},
		{
			name:    "duplicate ID",
			comp:    ViewComponent{ID: "dup", Type: "record_table"},
			wantErr: `duplicate component id "dup"`,
		},
		{
			name:    "invalid span",
			comp:    ViewComponent{ID: "wide", Type: "record_table", Span: 13},
			wantErr: "span must be 0-12, got 13",
		},
	}

	seen := make(map[string]bool)
	seen["dup"] = true // pre-seed for duplicate test

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seenCopy := make(map[string]bool)
			for k, v := range seen {
				seenCopy[k] = v
			}
			err := tt.comp.Validate(seenCopy)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if err.Error() != tt.wantErr {
				t.Errorf("expected error %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestViewComponentValidate_IsContainer(t *testing.T) {
	containers := []string{"dashboard_grid", "split_view", "tabs", "form_section", "field_group"}
	leaves := []string{"record_table", "metric_card", "scanner_input", "product_grid", "cart_panel"}

	for _, ctype := range containers {
		c := ViewComponent{ID: "c", Type: ctype}
		if !c.IsContainer() {
			t.Errorf("expected %q to be a container", ctype)
		}
	}
	for _, ctype := range leaves {
		c := ViewComponent{ID: "c", Type: ctype}
		if c.IsContainer() {
			t.Errorf("expected %q to be a leaf, not a container", ctype)
		}
	}
}

func TestViewComponentValidate_NestedChildren(t *testing.T) {
	v := View{
		Name:  "Dashboard",
		Route: "/dashboard",
		Type:  "dashboard",
		Components: []ViewComponent{
			{
				ID:   "grid",
				Type: "dashboard_grid",
				Components: []ViewComponent{
					{ID: "sales", Type: "metric_card"},
					{ID: "orders", Type: "metric_card"},
					// Duplicate ID inside nested children
					{ID: "sales", Type: "metric_card"},
				},
			},
		},
	}

	err := v.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate nested component id")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ViewAction Validation
// ---------------------------------------------------------------------------

func TestViewActionValidate(t *testing.T) {
	tests := []struct {
		name    string
		action  ViewAction
		wantErr string
	}{
		{
			name:    "empty action",
			action:  ViewAction{},
			wantErr: "action id is required",
		},
		{
			name:    "missing trigger",
			action:  ViewAction{ID: "save", Type: "create_record"},
			wantErr: "action trigger is required",
		},
		{
			name:    "missing type",
			action:  ViewAction{ID: "save", Trigger: "on_click"},
			wantErr: "action type is required",
		},
		{
			name:    "unknown trigger",
			action:  ViewAction{ID: "save", Trigger: "on_hover", Type: "create_record"},
			wantErr: `unknown trigger "on_hover"`,
		},
		{
			name:    "unknown type",
			action:  ViewAction{ID: "save", Trigger: "on_click", Type: "send_email"},
			wantErr: `unknown action type "send_email"`,
		},
		{
			name:   "valid action",
			action: ViewAction{ID: "save", Trigger: "on_click", Type: "create_record"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.action.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestViewAction_IsMutation(t *testing.T) {
	mutations := []string{
		"create_record", "update_record", "delete_record",
		"workflow_transition", "create_transaction",
		"call_script", "call_webhook",
	}
	nonMutations := []string{"navigate", "local_cart_add", "local_cart_remove", "local_state_set"}

	for _, typ := range mutations {
		a := ViewAction{Type: typ}
		if !a.IsMutation() {
			t.Errorf("expected %q to be a mutation", typ)
		}
	}
	for _, typ := range nonMutations {
		a := ViewAction{Type: typ}
		if a.IsMutation() {
			t.Errorf("expected %q to NOT be a mutation", typ)
		}
	}
}

func TestViewAction_IsLocal(t *testing.T) {
	locals := []string{"local_cart_add", "local_cart_remove", "local_state_set", "navigate"}
	remotes := []string{"create_record", "update_record", "workflow_transition", "create_transaction"}

	for _, typ := range locals {
		a := ViewAction{Type: typ}
		if !a.IsLocal() {
			t.Errorf("expected %q to be local", typ)
		}
	}
	for _, typ := range remotes {
		a := ViewAction{Type: typ}
		if a.IsLocal() {
			t.Errorf("expected %q to be NOT local", typ)
		}
	}
}

// ---------------------------------------------------------------------------
// ViewRule Validation
// ---------------------------------------------------------------------------

func TestViewRuleValidate(t *testing.T) {
	tests := []struct {
		name    string
		rule    ViewRule
		wantErr string
	}{
		{
			name:    "empty rule",
			rule:    ViewRule{},
			wantErr: "rule target is required",
		},
		{
			name:    "unknown target",
			rule:    ViewRule{Target: "blink", Condition: ViewCondition{Field: "x", Op: "equals"}},
			wantErr: `unknown rule target "blink"`,
		},
		{
			name:    "missing condition field",
			rule:    ViewRule{Target: "visible", Condition: ViewCondition{Op: "equals"}},
			wantErr: "condition field is required",
		},
		{
			name:    "missing condition op",
			rule:    ViewRule{Target: "visible", Condition: ViewCondition{Field: "status"}},
			wantErr: "condition op is required",
		},
		{
			name:    "unknown condition op",
			rule:    ViewRule{Target: "visible", Condition: ViewCondition{Field: "status", Op: "regex"}},
			wantErr: `unknown condition op "regex"`,
		},
		{
			name: "valid rule",
			rule: ViewRule{Target: "visible", Condition: ViewCondition{Field: "status", Op: "equals", Value: "draft"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rule.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ViewFilter Validation
// ---------------------------------------------------------------------------

func TestViewFilterValidate(t *testing.T) {
	tests := []struct {
		name    string
		filter  ViewFilter
		wantErr string
	}{
		{
			name:    "empty filter",
			filter:  ViewFilter{},
			wantErr: "filter field is required",
		},
		{
			name:    "missing op",
			filter:  ViewFilter{Field: "status"},
			wantErr: "filter op is required",
		},
		{
			name:    "unknown op",
			filter:  ViewFilter{Field: "status", Op: "regex"},
			wantErr: `unknown filter op "regex"`,
		},
		{
			name:   "valid filter",
			filter: ViewFilter{Field: "status", Op: "equals", Value: "Active"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.filter.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ViewPublicAccess Validation
// ---------------------------------------------------------------------------

func TestViewPublicAccessValidate(t *testing.T) {
	tests := []struct {
		name    string
		pa      *ViewPublicAccess
		wantErr string
	}{
		{
			name: "disabled is fine",
			pa:   &ViewPublicAccess{Enabled: false},
		},
		{
			name:    "enabled with no components",
			pa:      &ViewPublicAccess{Enabled: true},
			wantErr: `public_access.components must list at least one component id or "*"`,
		},
		{
			name: "enabled with components",
			pa:   &ViewPublicAccess{Enabled: true, Components: []string{"products", "search"}},
		},
		{
			name: "enabled with star",
			pa:   &ViewPublicAccess{Enabled: true, Components: []string{"*"}},
		},
		{
			name:    "star with other components",
			pa:      &ViewPublicAccess{Enabled: true, Components: []string{"*", "products"}},
			wantErr: `"*" must be the only entry`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pa.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error containing %q, got nil", tt.wantErr)
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestViewPublicAccess_AllowsComponent(t *testing.T) {
	tests := []struct {
		name string
		pa   *ViewPublicAccess
		id   string
		want bool
	}{
		{"nil access", nil, "products", false},
		{"disabled", &ViewPublicAccess{Enabled: false, Components: []string{"products"}}, "products", false},
		{"exact match", &ViewPublicAccess{Enabled: true, Components: []string{"products", "search"}}, "products", true},
		{"no match", &ViewPublicAccess{Enabled: true, Components: []string{"products", "search"}}, "cart", false},
		{"star matches all", &ViewPublicAccess{Enabled: true, Components: []string{"*"}}, "anything", true},
		{"star matches empty", &ViewPublicAccess{Enabled: true, Components: []string{"*"}}, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.pa.AllowsComponent(tt.id)
			if got != tt.want {
				t.Errorf("AllowsComponent(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// JSON Round-Trip
// ---------------------------------------------------------------------------

func TestView_JSONRoundTrip(t *testing.T) {
	v := View{
		Name:   "POS Register",
		Route:  "/pos",
		Type:   "workspace",
		Layout: "two_panel",
		Label:  "POS Register",
		Module: "Sales",
		Components: []ViewComponent{
			{
				ID:   "products",
				Type: "product_grid",
				Data: "primary.data",
				Bindings: map[string]string{
					"title": "product_name",
					"price": "selling_price",
				},
				Actions: []ViewAction{
					{ID: "tap", Trigger: "on_click", Type: "local_cart_add"},
				},
				Rules: []ViewRule{
					{Target: "visible", Condition: ViewCondition{Field: "stock_qty", Op: "gt", Value: float64(0)}},
				},
				Filters: []ViewFilter{
					{Field: "is_active", Op: "equals", Value: true},
				},
				MobileColumns: []string{"product_name", "selling_price"},
			},
		},
	}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored View
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.Name != v.Name {
		t.Errorf("name: got %q, want %q", restored.Name, v.Name)
	}
	if restored.Route != v.Route {
		t.Errorf("route: got %q, want %q", restored.Route, v.Route)
	}
	if len(restored.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(restored.Components))
	}

	comp := restored.Components[0]
	if comp.Bindings["title"] != "product_name" {
		t.Errorf("binding title: got %q, want 'product_name'", comp.Bindings["title"])
	}
	if comp.Data != "primary.data" {
		t.Errorf("data: got %q, want %q", comp.Data, "primary.data")
	}
	if len(comp.MobileColumns) != 2 {
		t.Errorf("expected 2 mobile columns, got %d", len(comp.MobileColumns))
	}
}

const nestedDashboardYAML = `name: Sales Dashboard
route: /dashboard
type: dashboard
layout: grid
label: Sales Dashboard
module: Sales
components:
  - id: grid
    type: dashboard_grid
    region: main
    position: 0
    components:
      - id: total_sales
        type: metric_card
        region: main
        source_doctype: Sale
        label: Sales
        bindings:
          fn: COUNT
          view: Sales Dashboard
        filters:
          - field: payment_status
            op: equals
            value: Paid
        actions:
          - id: open_sales
            trigger: on_click
            type: navigate
            config:
              to: /workspace/Sale
        rules:
          - target: disabled
            condition:
              field: payment_status
              op: equals
              value: Pending
        desktop_columns: [customer_name, total_amount]
        mobile_columns: [customer_name]
        span: 4
`

func parseViewYAMLForTest(t *testing.T, input string) *View {
	t.Helper()

	view, err := ParseViewYAML([]byte(input))
	if err != nil {
		t.Fatalf("ParseViewYAML: %v", err)
	}

	return view
}

func requireComponentCount(t *testing.T, components []ViewComponent, want int) {
	t.Helper()

	if len(components) != want {
		t.Fatalf("component count: got %d, want %d (%#v)", len(components), want, components)
	}
}

func requireString(t *testing.T, label, got, want string) {
	t.Helper()

	if got != want {
		t.Fatalf("%s: got %q, want %q", label, got, want)
	}
}

func requireInt(t *testing.T, label string, got, want int) {
	t.Helper()

	if got != want {
		t.Fatalf("%s: got %d, want %d", label, got, want)
	}
}

func requireMapString(t *testing.T, label string, got map[string]string, want map[string]string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s size: got %d, want %d (%#v)", label, len(got), len(want), got)
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Fatalf("%s[%q]: got %q, want %q", label, key, got[key], wantValue)
		}
	}
}

func requireStringSlice(t *testing.T, label string, got []string, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s size: got %d, want %d (%#v)", label, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d]: got %q, want %q", label, i, got[i], want[i])
		}
	}
}

func TestParseViewYAML_PreservesNestedComponentConfig(t *testing.T) {
	view := parseViewYAMLForTest(t, nestedDashboardYAML)
	requireComponentCount(t, view.Components, 1)
	requireComponentCount(t, view.Components[0].Components, 1)
	metric := view.Components[0].Components[0]

	t.Run("component identity", func(t *testing.T) {
		requireString(t, "id", metric.ID, "total_sales")
		requireString(t, "type", metric.Type, "metric_card")
		requireString(t, "region", metric.Region, "main")
		requireString(t, "label", metric.Label, "Sales")
		requireInt(t, "position", metric.Position, 0)
	})

	t.Run("source doctype", func(t *testing.T) {
		requireString(t, "source_doctype", metric.SourceDocType, "Sale")
	})

	t.Run("bindings", func(t *testing.T) {
		requireMapString(t, "bindings", metric.Bindings, map[string]string{
			"fn":   "COUNT",
			"view": "Sales Dashboard",
		})
	})

	t.Run("filters", func(t *testing.T) {
		if len(metric.Filters) != 1 {
			t.Fatalf("filters size: got %d, want 1 (%#v)", len(metric.Filters), metric.Filters)
		}
		requireString(t, "filters[0].field", metric.Filters[0].Field, "payment_status")
		requireString(t, "filters[0].op", metric.Filters[0].Op, "equals")
		requireString(t, "filters[0].value", metric.Filters[0].Value.(string), "Paid")
	})

	t.Run("actions", func(t *testing.T) {
		if len(metric.Actions) != 1 {
			t.Fatalf("actions size: got %d, want 1 (%#v)", len(metric.Actions), metric.Actions)
		}
		requireString(t, "actions[0].id", metric.Actions[0].ID, "open_sales")
		requireString(t, "actions[0].trigger", metric.Actions[0].Trigger, "on_click")
		requireString(t, "actions[0].type", metric.Actions[0].Type, "navigate")
		requireString(t, "actions[0].config.to", metric.Actions[0].Config["to"].(string), "/workspace/Sale")
	})

	t.Run("rules", func(t *testing.T) {
		if len(metric.Rules) != 1 {
			t.Fatalf("rules size: got %d, want 1 (%#v)", len(metric.Rules), metric.Rules)
		}
		requireString(t, "rules[0].target", metric.Rules[0].Target, "disabled")
		requireString(t, "rules[0].condition.field", metric.Rules[0].Condition.Field, "payment_status")
		requireString(t, "rules[0].condition.op", metric.Rules[0].Condition.Op, "equals")
		requireString(t, "rules[0].condition.value", metric.Rules[0].Condition.Value.(string), "Pending")
	})

	t.Run("layout fields", func(t *testing.T) {
		requireStringSlice(t, "desktop_columns", metric.DesktopColumns, []string{"customer_name", "total_amount"})
		requireStringSlice(t, "mobile_columns", metric.MobileColumns, []string{"customer_name"})
		requireInt(t, "span", metric.Span, 4)
	})
}
