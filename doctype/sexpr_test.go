package doctype

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestToSExpr_RoundTrip(t *testing.T) {
	snapshot := buildTestSnapshot()
	sexpr := ToSExpr(snapshot)
	t.Logf("Generated s-expr:\n%s", sexpr)

	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}

	// Verify by re-serializing and comparing strings (canonical comparison).
	// Since both ToSExpr outputs are canonical, if the round-trip preserves
	// all data, the strings will match.
	sexpr2 := ToSExpr(parsed)
	if sexpr != sexpr2 {
		t.Errorf("round-trip mismatch:\noriginal:\n%s\n\nre-serialized:\n%s", sexpr, sexpr2)
	}
}

func TestFromSExprRejectsUnsupportedConfigSections(t *testing.T) {
	if _, err := FromSExpr("(config (doctypes) (unsupported-config))"); err == nil {
		t.Fatal("unsupported config section was silently ignored")
	}
	if _, err := FromSExpr("(config (doctypes) (workflows))"); err == nil {
		t.Fatal("retired lifecycle workflow section was accepted")
	}
}

func TestToSExpr_Canonical(t *testing.T) {
	snapshot := buildTestSnapshot()

	// Mutate field order within a doctype — canonical output should be identical.
	for _, dt := range snapshot.DocTypes {
		if len(dt.Fields) >= 2 {
			dt.Fields[0], dt.Fields[1] = dt.Fields[1], dt.Fields[0]
		}
	}

	sexpr1 := ToSExpr(snapshot)

	// Re-sort fields back
	for _, dt := range snapshot.DocTypes {
		if len(dt.Fields) >= 2 {
			dt.Fields[0], dt.Fields[1] = dt.Fields[1], dt.Fields[0]
		}
	}

	sexpr2 := ToSExpr(snapshot)

	if sexpr1 != sexpr2 {
		t.Error("canonical form is not deterministic across field order changes")
	}

	// Also test: re-ordering doctypes shouldn't matter.
	dts := snapshot.DocTypes
	if len(dts) >= 2 {
		dts[0], dts[1] = dts[1], dts[0]
	}
	sexpr3 := ToSExpr(snapshot)
	if sexpr2 != sexpr3 {
		t.Error("canonical form is not deterministic across doctype order changes")
	}
}

func TestToSExpr_AllSections(t *testing.T) {
	snapshot := buildFullTestSnapshot()
	sexpr := ToSExpr(snapshot)
	t.Logf("Full snapshot s-expr:\n%s", sexpr)

	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}

	// Verify all sections
	if len(parsed.DocTypes) != len(snapshot.DocTypes) {
		t.Errorf("doctypes: got %d, want %d", len(parsed.DocTypes), len(snapshot.DocTypes))
	}
	if len(parsed.Roles) != len(snapshot.Roles) {
		t.Errorf("roles: got %d, want %d", len(parsed.Roles), len(snapshot.Roles))
	}
	if len(parsed.Permissions) != len(snapshot.Permissions) {
		t.Errorf("permissions: got %d, want %d", len(parsed.Permissions), len(snapshot.Permissions))
	}
	if len(parsed.AnalyticsMetrics) != len(snapshot.AnalyticsMetrics) {
		t.Errorf("analytics_metrics: got %d, want %d", len(parsed.AnalyticsMetrics), len(snapshot.AnalyticsMetrics))
	}
	if len(parsed.Scripts) != len(snapshot.Scripts) {
		t.Errorf("scripts: got %d, want %d", len(parsed.Scripts), len(snapshot.Scripts))
	}
	if parsed.MinKoraVersion != snapshot.MinKoraVersion {
		t.Errorf("min_kora_version: got %q, want %q", parsed.MinKoraVersion, snapshot.MinKoraVersion)
	}

	if len(parsed.DocTypes) > 0 {
		dt := parsed.DocTypes[0]
		if !dt.IsSubmittable {
			t.Error("doctype should be submittable")
		}
		if dt.TitleField != "title" {
			t.Errorf("title_field = %q, want %q", dt.TitleField, "title")
		}
	}

	// Canonical comparison
	sexpr2 := ToSExpr(parsed)
	if sexpr != sexpr2 {
		t.Errorf("round-trip mismatch:\n%s\n\n%s", sexpr, sexpr2)
	}
}

func TestToSExpr_RoundTripReports(t *testing.T) {
	snapshot := &ConfigSnapshot{Reports: []json.RawMessage{json.RawMessage(`{"name":"cake_profit","label":"Cake profit","queries":[]}`)}}
	sexpr := ToSExpr(snapshot)
	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}
	if len(parsed.Reports) != 1 || string(parsed.Reports[0]) != string(snapshot.Reports[0]) {
		t.Fatalf("reports were not preserved: %#v", parsed.Reports)
	}
}

func TestToSExpr_RoundTripsScheduledJobDefinitions(t *testing.T) {
	snapshot := &ConfigSnapshot{ScheduledJobs: []contract.ScheduledJobDefinition{{
		Name: "invoice-generation", Type: "subscription_invoice", Schedule: "0 0 * * *",
		Config: json.RawMessage(`{"due_days":14,"include_paused":false}`),
	}}}
	sexpr := ToSExpr(snapshot)
	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr: %v", err)
	}
	if len(parsed.ScheduledJobs) != 1 || parsed.ScheduledJobs[0].Name != snapshot.ScheduledJobs[0].Name || string(parsed.ScheduledJobs[0].Config) != string(snapshot.ScheduledJobs[0].Config) {
		t.Fatalf("scheduled jobs were not preserved: %#v", parsed.ScheduledJobs)
	}
	if got := ToSExpr(parsed); got != sexpr {
		t.Fatalf("scheduled jobs did not serialize canonically:\nfirst: %s\nsecond: %s", sexpr, got)
	}
}

func TestToSExpr_RoundTripsModuleDescriptors(t *testing.T) {
	snapshot := &ConfigSnapshot{Modules: []contract.ModuleDescriptor{{
		ID: "finance.ar", Version: "1.2.3",
		Resources: []contract.OwnedResource{{
			Ref:    contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1},
			Access: contract.ModulePrivate, MutationMode: contract.MutationCommands,
			ScopeKind: contract.ScopeEntityOwned, ScopeField: "legal_entity",
		}},
	}}}
	encoded := ToSExpr(snapshot)
	parsed, err := FromSExpr(encoded)
	if err != nil {
		t.Fatalf("parse module descriptor snapshot: %v", err)
	}
	if len(parsed.Modules) != 1 || parsed.Modules[0].ID != "finance.ar" || parsed.Modules[0].Resources[0].Ref.Name != "Invoice" || parsed.Modules[0].Resources[0].ScopeField != "legal_entity" {
		t.Fatalf("module ownership metadata was not preserved: %#v", parsed.Modules)
	}
	if got := ToSExpr(parsed); got != encoded {
		t.Fatalf("module snapshot did not serialize canonically:\nfirst: %s\nsecond: %s", encoded, got)
	}
}

func TestToSExpr_Empty(t *testing.T) {
	snapshot := &ConfigSnapshot{}
	sexpr := ToSExpr(snapshot)
	t.Logf("Empty snapshot s-expr: %s", sexpr)

	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}

	if len(parsed.DocTypes) != 0 {
		t.Error("expected empty doctypes")
	}
	if len(parsed.Roles) != 0 {
		t.Error("expected empty roles")
	}
}

func TestToSExpr_Nil(t *testing.T) {
	sexpr := ToSExpr(nil)
	if sexpr != "(config)" {
		t.Errorf("nil snapshot: got %q, want %q", sexpr, "(config)")
	}
}

func TestFromSExpr_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"just parens", "()"},
		{"wrong head", "(wrong)"},
		{"garbage", "(((()))"},
		{"unterminated string", `(config "unterminated)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FromSExpr(tt.input)
			if err == nil {
				t.Errorf("expected error for input %q", tt.input)
			}
		})
	}
}

func TestCanonicalizeSExpr(t *testing.T) {
	// Test that canonicalization handles non-canonical input
	// by re-parsing and re-serializing.
	snapshot := buildTestSnapshot()
	canonical := ToSExpr(snapshot)

	result := CanonicalizeSExpr(canonical)
	if result != canonical {
		t.Errorf("canonicalization changed already-canonical s-expr:\n%s\n\n%s", canonical, result)
	}
}

func TestFromSExpr_FieldsWithConstraints(t *testing.T) {
	snapshot := &ConfigSnapshot{
		DocTypes: []*DocType{
			{
				Name: "Task",
				Fields: []Field{
					{
						Fieldname: "email",
						Fieldtype: "Data",
						Reqd:      true,
						Unique:    true,
						Constraints: []Constraint{
							{Type: "min_length", Value: int64(5), Message: "Too short"},
							{Type: "pattern", Pattern: `^[a-z]+$`, Message: "Invalid"},
						},
					},
					{
						Fieldname: "age",
						Fieldtype: "Int",
						Default:   "0",
						Constraints: []Constraint{
							{Type: "min", Value: int64(0), Message: "Must be >= 0"},
							{Type: "max", Value: int64(150), Message: "Must be <= 150"},
						},
					},
				},
			},
		},
	}

	sexpr := ToSExpr(snapshot)
	t.Logf("Fields with constraints:\n%s", sexpr)

	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}

	if len(parsed.DocTypes) != 1 {
		t.Fatalf("expected 1 doctype, got %d", len(parsed.DocTypes))
	}
	dt := parsed.DocTypes[0]
	if len(dt.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(dt.Fields))
	}

	// Find fields by name (since ToSExpr sorts by fieldname)
	var emailField, ageField *Field
	for i := range dt.Fields {
		switch dt.Fields[i].Fieldname {
		case "email":
			emailField = &dt.Fields[i]
		case "age":
			ageField = &dt.Fields[i]
		}
	}
	if emailField == nil {
		t.Fatal("email field not found")
	}
	if ageField == nil {
		t.Fatal("age field not found")
	}

	// Check email field constraints
	if !emailField.Reqd {
		t.Error("email field should be required")
	}
	if !emailField.Unique {
		t.Error("email field should be unique")
	}
	if len(emailField.Constraints) != 2 {
		t.Fatalf("email constraints: got %d, want 2", len(emailField.Constraints))
	}
	// Constraints are sorted by type alphabetically: min_length < pattern
	if emailField.Constraints[0].Type != "min_length" {
		t.Errorf("constraint[0].Type = %q, want %q", emailField.Constraints[0].Type, "min_length")
	}
	if emailField.Constraints[1].Type != "pattern" {
		t.Errorf("constraint[1].Type = %q, want %q", emailField.Constraints[1].Type, "pattern")
	}

	// Check age field
	if ageField.Default != "0" {
		t.Errorf("age default = %q, want %q", ageField.Default, "0")
	}
	if len(ageField.Constraints) != 2 {
		t.Fatalf("age constraints: got %d, want 2", len(ageField.Constraints))
	}
	// Constraints sorted alphabetically: max < min
	if ageField.Constraints[0].Type != "max" {
		t.Errorf("age constraint[0].Type = %q, want %q", ageField.Constraints[0].Type, "max")
	}

	// Canonical comparison
	sexpr2 := ToSExpr(parsed)
	if sexpr != sexpr2 {
		t.Errorf("round-trip mismatch:\n%s\n\n%s", sexpr, sexpr2)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func buildTestSnapshot() *ConfigSnapshot {
	return &ConfigSnapshot{
		DocTypes: []*DocType{
			{
				Name:          "Task",
				Module:        "Core",
				IsSubmittable: true,
				TitleField:    "title",
				SearchFields:  "title,status",
				SortField:     "modified",
				SortOrder:     "DESC",
				Description:   "A task item",
				Fields: []Field{
					{
						Fieldname: "title",
						Fieldtype: "Data",
						Reqd:      true,
					},
					{
						Fieldname: "status",
						Fieldtype: "Select",
						Options:   "Open,In Progress,Closed",
						Default:   "Open",
					},
					{
						Fieldname: "description",
						Fieldtype: "Text",
					},
				},
			},
			{
				Name:   "User",
				Module: "Core",
				Fields: []Field{
					{
						Fieldname: "email",
						Fieldtype: "Data",
						Reqd:      true,
						Unique:    true,
					},
					{
						Fieldname: "name",
						Fieldtype: "Data",
						Reqd:      true,
					},
				},
			},
		},
	}
}

func buildFullTestSnapshot() *ConfigSnapshot {
	return &ConfigSnapshot{
		MinKoraVersion: "0.10.0",
		DocTypes: []*DocType{
			{
				Name:          "Invoice",
				Module:        "Sales",
				IsSubmittable: true,
				TitleField:    "title",
				Fields: []Field{
					{
						Fieldname: "customer",
						Fieldtype: "Link",
						Options:   "Customer",
						Reqd:      true,
					},
					{
						Fieldname: "amount",
						Fieldtype: "Currency",
						Reqd:      true,
						Default:   "0",
					},
				},
			},
		},
		Roles: []*Role{
			{Name: "Administrator", WorkspaceAccess: true, Description: "System admin"},
			{Name: "Guest", WorkspaceAccess: false},
		},
		Permissions: []*Permission{
			{Doctype: "Invoice", Role: "Administrator", Read: true, Write: true, Create: true, Delete: true, Submit: true},
			{Doctype: "Invoice", Role: "Guest", Read: true, IfAssigned: []string{"assignee_user"}},
		},
		AnalyticsMetrics: []*AnalyticsMetricConfig{
			{
				Name:      "total_revenue",
				Label:     "Total Revenue",
				Type:      "sum",
				DocType:   "Invoice",
				FieldName: "amount",
			},
		},
		Scripts: []*ScriptSnapshot{
			{
				Name:       "validate_invoice",
				ScriptType: "doc_event",
				DocType:    "Invoice",
				Event:      "before_save",
				IsActive:   true,
				ScriptHash: "abc123def456",
				Source:     "return { success: true };\n",
			},
		},
	}
}
