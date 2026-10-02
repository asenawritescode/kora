package doctype

import (
	"strings"
	"testing"
)

func TestParseFieldworkConfigs(t *testing.T) {
	// Parse all Fieldwork doctypes.
	doctypes, err := ParseDirectory("../config/v0/fieldwork/doctypes")
	if err != nil {
		t.Fatalf("parsing Fieldwork configs: %v", err)
	}

	expected := map[string]int{
		"Customer":         9,
		"Equipment":        7,
		"Technician":       7,
		"Work Order":       11,
		"Work Order Item":  5,
		"Service Report":   10,
		"Service Request":  5,
		"Visit":            5,
		"Used Part":        5,
		"Customer Signoff": 5,
	}

	for _, dt := range doctypes {
		expFields, ok := expected[dt.Name]
		if !ok {
			t.Errorf("unexpected doctype: %s", dt.Name)
			continue
		}
		if len(dt.Fields) != expFields {
			// Count includes layout fields like Section Break.
			t.Logf("%s has %d fields (expected includes layout fields)", dt.Name, len(dt.Fields))
		}

		// Verify key properties.
		switch dt.Name {
		case "Work Order":
			if !dt.IsSubmittable {
				t.Error("Work Order should be submittable")
			}
			if !dt.TrackChanges {
				t.Error("Work Order should track changes")
			}
			if dt.TitleField != "title" {
				t.Errorf("Work Order title_field = %q, want %q", dt.TitleField, "title")
			}

		case "Work Order Item":
			if !dt.IsChildTable {
				t.Error("Work Order Item should be a child table")
			}

		case "Service Report":
			if !dt.IsSubmittable {
				t.Error("Service Report should be submittable")
			}
			if len(dt.DocConstraints) < 1 {
				t.Error("Service Report should have doc constraints")
			}

		case "Customer":
			if dt.TitleField != "company_name" {
				t.Errorf("Customer title_field = %q, want %q", dt.TitleField, "company_name")
			}

		case "Equipment":
			// Verify Link field has exists constraint.
			f := dt.GetField("customer")
			if f == nil {
				t.Error("Equipment.customer field not found")
			} else {
				if f.Fieldtype != "Link" {
					t.Errorf("Equipment.customer type = %q, want Link", f.Fieldtype)
				}
				hasExists := false
				for _, c := range f.Constraints {
					if c.Type == "exists" {
						hasExists = true
					}
				}
				if !hasExists {
					t.Error("Equipment.customer should have 'exists' constraint")
				}
			}

		case "Technician":
			f := dt.GetField("employee_id")
			if f == nil {
				t.Error("Technician.employee_id field not found")
			} else if !f.Unique {
				t.Error("Technician.employee_id should be unique")
			}
		}

		t.Logf("✓ %s (%d fields)", dt.Name, len(dt.Fields))
	}

	if len(doctypes) != 10 {
		t.Errorf("expected 10 doctypes, got %d", len(doctypes))
	}
}

func TestParseContentConfigsWithPublicAccess(t *testing.T) {
	doctypes, err := ParseDirectory("../config/v0/content/doctypes")
	if err != nil {
		t.Fatalf("parsing Content configs: %v", err)
	}

	if len(doctypes) == 0 {
		t.Fatal("expected content doctypes")
	}

	// Doctypes that are intentionally private (internal CMS schema, not website-facing).
	privateDoctypes := map[string]bool{
		"Template Pack":      true,
		"Template Pack File": true,
	}

	seenPublic := false
	for _, dt := range doctypes {
		if privateDoctypes[dt.Name] {
			continue
		}
		if dt.PublicAccess == nil {
			t.Fatalf("%s should define public_access", dt.Name)
		}
		if !dt.PublicAccess.Enabled {
			t.Fatalf("%s public_access should be enabled", dt.Name)
		}
		if len(dt.PublicAccess.Fields) == 0 {
			t.Fatalf("%s public_access should expose at least one field", dt.Name)
		}
		seenPublic = true
	}

	if !seenPublic {
		t.Fatal("expected at least one public content doctype")
	}
}

func TestParseSingleFile(t *testing.T) {
	dt, err := ParseFile("../config/v0/fieldwork/doctypes/customer.yaml")
	if err != nil {
		t.Fatalf("parsing customer.yaml: %v", err)
	}

	if dt.Name != "Customer" {
		t.Errorf("name = %q, want %q", dt.Name, "Customer")
	}
	if dt.Module != "Fieldwork" {
		t.Errorf("module = %q, want %q", dt.Module, "Fieldwork")
	}
	if dt.TitleField != "company_name" {
		t.Errorf("title_field = %q, want %q", dt.TitleField, "company_name")
	}

	// Verify company_name field.
	f := dt.GetField("company_name")
	if f == nil {
		t.Fatal("company_name field not found")
	}
	if f.Fieldtype != "Data" {
		t.Errorf("company_name type = %q, want Data", f.Fieldtype)
	}
	if !f.Reqd {
		t.Error("company_name should be required")
	}
	if !f.Bold {
		t.Error("company_name should be bold")
	}
	if !f.InListView {
		t.Error("company_name should be in_list_view")
	}
}

func TestFieldValidation(t *testing.T) {
	// Test that validation catches missing required properties.
	dt := &DocType{
		Name:   "TestType",
		Module: "Test",
		Fields: []Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name"},
			{Fieldname: "items", Fieldtype: "Table", Label: "Items"}, // Missing Options
		},
	}

	err := dt.Validate()
	if err == nil {
		t.Error("expected validation error for Table field without options")
	}
	t.Logf("validation error (expected): %v", err)
}

func TestFieldValidationRejectsInvalidTemporalDefaults(t *testing.T) {
	tests := []struct {
		name      string
		fieldtype string
		def       string
	}{
		{name: "today on data", fieldtype: "Data", def: "Today"},
		{name: "now on date", fieldtype: "Date", def: "Now"},
		{name: "now on int", fieldtype: "Int", def: "Now"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dt := &DocType{
				Name:   "TemporalTest",
				Module: "Test",
				Fields: []Field{
					{Fieldname: "value", Fieldtype: tt.fieldtype, Default: tt.def},
				},
			}
			err := dt.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s default on %s", tt.def, tt.fieldtype)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "default") {
				t.Fatalf("expected default validation error, got: %v", err)
			}
		})
	}
}
