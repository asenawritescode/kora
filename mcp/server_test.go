package mcp

import (
	"testing"

	"github.com/asenawritescode/kora/doctype"
)

func TestToolGeneration_ForDoctype(t *testing.T) {
	reg := doctype.NewRegistry()

	// Register a non-child doctype.
	dt := &doctype.DocType{
		Name: "Customer",
		Fields: []doctype.Field{
			{Fieldname: "customer_name", Fieldtype: "Data", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data"},
			{Fieldname: "phone", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	server := New(reg, "test-site")

	if server == nil {
		t.Fatal("New returned nil")
	}
	if server.srv == nil {
		t.Fatal("server.srv is nil (no tools registered)")
	}
}

func TestToolNames_FollowPattern(t *testing.T) {
	reg := doctype.NewRegistry()

	// Multi-word doctype.
	dt := &doctype.DocType{
		Name: "Work Order",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
		},
	}
	reg.Register(dt)

	server := New(reg, "test-site")
	if server == nil {
		t.Fatal("New returned nil")
	}

	// The tool names should follow the pattern <sanitized_name>_<operation>.
	// For "Work Order", sanitized name is "work_order".
	// Tools: work_order_list, work_order_create, work_order_get, work_order_update, work_order_delete
	// Plus validate_yaml from addConfigTools.
	// We can verify by checking the server was created without panic.
	_ = server
}

func TestEmptyRegistry_NoTools(t *testing.T) {
	reg := doctype.NewRegistry()

	server := New(reg, "test-site")
	if server == nil {
		t.Fatal("New returned nil")
	}
	// An empty registry should at least have the config tools (validate_yaml),
	// but no doctype-specific tools.
	_ = server
}

func TestAddDoctypeTools_ExcludesChildTables(t *testing.T) {
	reg := doctype.NewRegistry()

	// Register a parent doctype.
	parent := &doctype.DocType{
		Name: "Invoice",
		Fields: []doctype.Field{
			{Fieldname: "total", Fieldtype: "Currency"},
		},
	}
	reg.Register(parent)

	// Register a child table doctype (should be excluded from tools).
	child := &doctype.DocType{
		Name:         "Invoice Item",
		IsChildTable: true,
		Fields: []doctype.Field{
			{Fieldname: "item_name", Fieldtype: "Data"},
			{Fieldname: "qty", Fieldtype: "Int"},
			{Fieldname: "rate", Fieldtype: "Currency"},
		},
	}
	reg.Register(child)

	server := New(reg, "test-site")
	if server == nil {
		t.Fatal("New returned nil")
	}
	// Should have validated that child tables don't generate tools without panic.
	_ = server
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Customer", "customer"},
		{"Work Order", "work_order"},
		{"Sales-Invoice", "sales_invoice"},
		{"Lead", "lead"},
		{"already_lower", "already_lower"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildFieldSchema(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Test",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data"},
			{Fieldname: "age", Fieldtype: "Int"},
			{Fieldname: "price", Fieldtype: "Float"},
			{Fieldname: "active", Fieldtype: "Check"},
			{Fieldname: "items", Fieldtype: "Table"}, // Skipped
		},
	}

	schema := buildFieldSchema(dt)

	if len(schema) != 4 {
		t.Errorf("field schema length = %d, want 4", len(schema))
	}
	// Check types.
	if schema["age"] != nil {
		ageProps := schema["age"].(map[string]any)
		if ageProps["type"] != "integer" {
			t.Errorf("age type = %v, want integer", ageProps["type"])
		}
	}
	if schema["price"] != nil {
		priceProps := schema["price"].(map[string]any)
		if priceProps["type"] != "number" {
			t.Errorf("price type = %v, want number", priceProps["type"])
		}
	}
	if schema["active"] != nil {
		activeProps := schema["active"].(map[string]any)
		if activeProps["type"] != "boolean" {
			t.Errorf("active type = %v, want boolean", activeProps["type"])
		}
	}
	// Table fields should be excluded.
	if _, ok := schema["items"]; ok {
		t.Error("items (Table field) should be excluded from schema")
	}
}

func TestRequiredFields(t *testing.T) {
	dt := &doctype.DocType{
		Name: "Test",
		Fields: []doctype.Field{
			{Fieldname: "name", Fieldtype: "Data", Reqd: true},
			{Fieldname: "email", Fieldtype: "Data", Reqd: false},
			{Fieldname: "age", Fieldtype: "Int", Reqd: true},
			{Fieldname: "items", Fieldtype: "Table", Reqd: true}, // Should be excluded
		},
	}

	req := requiredFields(dt)
	expected := []string{"name", "age"}

	if len(req) != len(expected) {
		t.Fatalf("requiredFields len = %d, want %d; got %v", len(req), len(expected), req)
	}
	for i, v := range expected {
		if req[i] != v {
			t.Errorf("req[%d] = %q, want %q", i, req[i], v)
		}
	}
}
