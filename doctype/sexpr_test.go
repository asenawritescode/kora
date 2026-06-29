package doctype

import (
	"testing"
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
	if len(parsed.Workflows) != len(snapshot.Workflows) {
		t.Errorf("workflows: got %d, want %d", len(parsed.Workflows), len(snapshot.Workflows))
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

	// Check specific data points
	if len(parsed.Workflows) > 0 {
		wf := parsed.Workflows[0]
		if len(wf.States) != 2 {
			t.Errorf("workflow states: got %d, want 2", len(wf.States))
		}
		if len(wf.Transitions) != 1 {
			t.Errorf("workflow transitions: got %d, want 1", len(wf.Transitions))
		}
		if !wf.IsActive {
			t.Error("workflow should be active")
		}
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

func TestFromSExpr_WorkflowWithActionsAndNotifications(t *testing.T) {
	snapshot := &ConfigSnapshot{
		Workflows: []*Workflow{
			{
				Name:               "Order Workflow",
				DocumentType:       "Order",
				IsActive:           true,
				WorkflowStateField: "status",
				States: []WorkflowState{
					{State: "Draft", DocStatus: 0, AllowEdit: "Administrator", Style: "default"},
					{State: "Approved", DocStatus: 1, Style: "success"},
				},
				Transitions: []WorkflowTransition{
					{
						Action:  "Approve",
						From:    "Draft",
						To:      "Approved",
						Allowed: "Manager",
						OnTransition: []WorkflowAction{
							{Type: "script", Script: "validate_order"},
						},
						OnSuccess: []WorkflowAction{
							{Type: "webhook", WebhookURL: "https://hooks.example.com/order-approved", Async: true},
						},
					},
				},
				Notifications: []WorkflowNotification{
					{
						Event:   "on_submit",
						ToState: "Approved",
						Recipients: []map[string]string{
							{"role": "Manager"},
							{"email": "admin@example.com"},
						},
						Subject: "Order Approved",
						Message: "Order {{name}} has been approved.",
					},
				},
			},
		},
	}

	sexpr := ToSExpr(snapshot)
	t.Logf("Workflow with actions/notifications:\n%s", sexpr)

	parsed, err := FromSExpr(sexpr)
	if err != nil {
		t.Fatalf("FromSExpr error = %v", err)
	}

	if len(parsed.Workflows) != 1 {
		t.Fatalf("expected 1 workflow, got %d", len(parsed.Workflows))
	}
	wf := parsed.Workflows[0]
	if wf.Name != "Order Workflow" {
		t.Errorf("workflow name = %q, want %q", wf.Name, "Order Workflow")
	}
	if wf.DocumentType != "Order" {
		t.Errorf("document_type = %q, want %q", wf.DocumentType, "Order")
	}
	if !wf.IsActive {
		t.Error("workflow should be active")
	}

	if len(wf.States) != 2 {
		t.Fatalf("states: got %d, want 2", len(wf.States))
	}
	if wf.States[0].State != "Draft" || wf.States[0].DocStatus != 0 {
		t.Errorf("first state: got %s/%d, want Draft/0", wf.States[0].State, wf.States[0].DocStatus)
	}

	if len(wf.Transitions) != 1 {
		t.Fatalf("transitions: got %d, want 1", len(wf.Transitions))
	}
	tr := wf.Transitions[0]
	if tr.Action != "Approve" {
		t.Errorf("transition action = %q, want %q", tr.Action, "Approve")
	}
	if tr.From != "Draft" || tr.To != "Approved" {
		t.Errorf("transition: %s -> %s, want Draft -> Approved", tr.From, tr.To)
	}
	if len(tr.OnTransition) != 1 {
		t.Errorf("on_transition: got %d, want 1", len(tr.OnTransition))
	}
	if len(tr.OnSuccess) != 1 {
		t.Errorf("on_success: got %d, want 1", len(tr.OnSuccess))
	}

	if len(wf.Notifications) != 1 {
		t.Fatalf("notifications: got %d, want 1", len(wf.Notifications))
	}
	notif := wf.Notifications[0]
	if notif.Event != "on_submit" {
		t.Errorf("notification event = %q, want %q", notif.Event, "on_submit")
	}
	if len(notif.Recipients) != 2 {
		t.Errorf("recipients: got %d, want 2", len(notif.Recipients))
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
			{Doctype: "Invoice", Role: "Guest", Read: true},
		},
		Workflows: []*Workflow{
			{
				Name:               "InvoiceWorkflow",
				DocumentType:       "Invoice",
				IsActive:           true,
				WorkflowStateField: "status",
				States: []WorkflowState{
					{State: "Draft", DocStatus: 0},
					{State: "Submitted", DocStatus: 1},
				},
				Transitions: []WorkflowTransition{
					{
						Action:  "Submit",
						From:    "Draft",
						To:      "Submitted",
						Allowed: "Administrator",
					},
				},
			},
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
			},
		},
	}
}
