package doctype

import (
	"testing"
)

func TestViewSExpr_RoundTrip(t *testing.T) {
	original := &View{
		Name:   "POS Register",
		Route:  "/pos",
		Type:   "workspace",
		Layout: "two_panel",
		Label:  "POS Register",
		Module: "Sales",
		Components: []ViewComponent{
			{
				ID:            "products",
				Type:          "product_grid",
				Region:        "main",
				SourceDocType: "Product",
				Bindings: map[string]string{
					"title": "product_name",
					"price": "selling_price",
					"badge": "stock_qty",
				},
				Filters: []ViewFilter{
					{Field: "is_active", Op: "equals", Value: true},
				},
				Actions: []ViewAction{
					{ID: "tap", Trigger: "on_click", Type: "local_cart_add"},
				},
				Rules: []ViewRule{
					{Target: "visible", Condition: ViewCondition{
						Field: "stock_qty", Op: "gt", Value: float64(0),
					}},
				},
				MobileColumns: []string{"product_name", "selling_price"},
			},
		},
		PublicAccess: &ViewPublicAccess{
			Enabled:    false,
			Components: nil,
		},
	}

	// Build a snapshot with the view and serialize.
	snapshot := &ConfigSnapshot{Views: []*View{original}}
	sexpr := ToSExpr(snapshot)

	// Parse back.
	restored, err := ParseConfig(sexpr)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	if len(restored.Views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(restored.Views))
	}

	r := restored.Views[0]
	if r.Name != original.Name {
		t.Errorf("name: got %q, want %q", r.Name, original.Name)
	}
	if r.Route != original.Route {
		t.Errorf("route: got %q, want %q", r.Route, original.Route)
	}
	if r.Type != original.Type {
		t.Errorf("type: got %q, want %q", r.Type, original.Type)
	}
	if r.Layout != original.Layout {
		t.Errorf("layout: got %q, want %q", r.Layout, original.Layout)
	}
	if r.Module != original.Module {
		t.Errorf("module: got %q, want %q", r.Module, original.Module)
	}

	if len(r.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(r.Components))
	}

	comp := r.Components[0]
	if comp.ID != "products" {
		t.Errorf("component id: got %q, want 'products'", comp.ID)
	}
	if comp.Type != "product_grid" {
		t.Errorf("component type: got %q, want 'product_grid'", comp.Type)
	}
	if comp.SourceDocType != "Product" {
		t.Errorf("source doctype: got %q, want 'Product'", comp.SourceDocType)
	}
	if len(comp.Bindings) != 3 {
		t.Errorf("expected 3 bindings, got %d", len(comp.Bindings))
	}
	if comp.Bindings["title"] != "product_name" {
		t.Errorf("binding title: got %q, want 'product_name'", comp.Bindings["title"])
	}
	if comp.Bindings["badge"] != "stock_qty" {
		t.Errorf("binding badge: got %q, want 'stock_qty'", comp.Bindings["badge"])
	}
	if len(comp.Filters) != 1 {
		t.Errorf("expected 1 filter, got %d", len(comp.Filters))
	}
	if comp.Filters[0].Field != "is_active" {
		t.Errorf("filter field: got %q, want 'is_active'", comp.Filters[0].Field)
	}
	if len(comp.Actions) != 1 {
		t.Errorf("expected 1 action, got %d", len(comp.Actions))
	}
	if comp.Actions[0].Type != "local_cart_add" {
		t.Errorf("action type: got %q, want 'local_cart_add'", comp.Actions[0].Type)
	}
	if len(comp.Rules) != 1 {
		t.Errorf("expected 1 rule, got %d", len(comp.Rules))
	}
	if comp.Rules[0].Condition.Field != "stock_qty" {
		t.Errorf("rule field: got %q, want 'stock_qty'", comp.Rules[0].Condition.Field)
	}
	if len(comp.MobileColumns) != 2 {
		t.Errorf("expected 2 mobile columns, got %d", len(comp.MobileColumns))
	}
}

func TestViewSExpr_EmptyViews(t *testing.T) {
	snapshot := &ConfigSnapshot{Views: nil}
	sexpr := ToSExpr(snapshot)

	// Should contain empty views section.
	if sexpr == "" {
		t.Fatal("expected non-empty s-expr")
	}

	restored, err := ParseConfig(sexpr)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(restored.Views) != 0 {
		t.Errorf("expected 0 views for nil, got %d", len(restored.Views))
	}

	// Empty slice should also work.
	snapshot2 := &ConfigSnapshot{Views: []*View{}}
	sexpr2 := ToSExpr(snapshot2)
	restored2, err := ParseConfig(sexpr2)
	if err != nil {
		t.Fatalf("ParseConfig empty: %v", err)
	}
	if len(restored2.Views) != 0 {
		t.Errorf("expected 0 views for empty slice, got %d", len(restored2.Views))
	}
}

func TestViewSExpr_DeterministicOutput(t *testing.T) {
	v1 := &View{Name: "B", Route: "/b", Type: "workspace"}
	v2 := &View{Name: "A", Route: "/a", Type: "dashboard"}

	snapshot := &ConfigSnapshot{Views: []*View{v1, v2}}
	first := ToSExpr(snapshot)
	second := ToSExpr(snapshot)

	if first != second {
		t.Errorf("expected deterministic output, got different s-exprs")
	}

	// "A" should appear before "B" (sorted by name).
	// Parse back to verify order.
	restored, err := ParseConfig(first)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(restored.Views) != 2 {
		t.Fatalf("expected 2 views, got %d", len(restored.Views))
	}
	if restored.Views[0].Name != "A" || restored.Views[1].Name != "B" {
		t.Errorf("expected sorted A then B, got %q then %q",
			restored.Views[0].Name, restored.Views[1].Name)
	}
}

func TestViewSExpr_DiffAfterViewChange(t *testing.T) {
	old := &ConfigSnapshot{
		Views: []*View{
			{Name: "Test", Route: "/old", Type: "workspace"},
		},
	}
	new := &ConfigSnapshot{
		Views: []*View{
			{Name: "Test", Route: "/new", Type: "dashboard", Layout: "grid"},
		},
	}

	oldSExpr := ToSExpr(old)
	newSExpr := ToSExpr(new)

	changes, err := DiffSExpr(oldSExpr, newSExpr)
	if err != nil {
		t.Fatalf("DiffSExpr: %v", err)
	}

	// Should detect a modification in the views section.
	foundViewChange := false
	for _, c := range changes {
		if c.Section == "views" {
			foundViewChange = true
			break
		}
	}
	if !foundViewChange {
		t.Errorf("expected diff to include a views section change, got %d changes", len(changes))
	}
}

func TestViewSExpr_BackwardCompatible(t *testing.T) {
	// Old format: doctypes-only JSON array should parse fine even
	// when ConfigSnapshot now has a Views field.
	oldJSON := `{"doctypes":[],"roles":null,"permissions":null,"views":null}`
	snapshot, err := ParseSnapshot(oldJSON)
	if err != nil {
		t.Fatalf("ParseSnapshot old format: %v", err)
	}
	if snapshot.Views != nil {
		t.Errorf("expected nil views from old format, got %v", snapshot.Views)
	}

	// New format with views.
	newJSON := `{"doctypes":[],"views":[{"name":"Test","route":"/test","type":"workspace"}]}`
	snapshot2, err := ParseSnapshot(newJSON)
	if err != nil {
		t.Fatalf("ParseSnapshot new format: %v", err)
	}
	if len(snapshot2.Views) != 1 {
		t.Fatalf("expected 1 view, got %d", len(snapshot2.Views))
	}
	if snapshot2.Views[0].Name != "Test" {
		t.Errorf("expected view name 'Test', got %q", snapshot2.Views[0].Name)
	}
}

func TestParseSnapshotRejectsUnsupportedConfigSections(t *testing.T) {
	if _, err := ParseSnapshot(`{"doctypes":[],"unsupported_config":[]}`); err == nil {
		t.Fatal("unsupported snapshot section was silently ignored")
	}
	if _, err := ParseSnapshot(`{"doctypes":[],"workflows":[]}`); err == nil {
		t.Fatal("retired lifecycle workflow section was accepted")
	}
}

func TestParseStoredConfigSnapshotRejectsRetiredWorkflowField(t *testing.T) {
	if _, err := ParseStoredConfigSnapshot(`{"doctypes":[],"workflows":[]}`); err == nil {
		t.Fatal("retired workflow field was accepted from stored config")
	}
}
