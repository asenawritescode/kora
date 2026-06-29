package doctype

import (
	"testing"
)

func TestMinVersionOK(t *testing.T) {
	tests := []struct {
		running  string
		required string
		expect   bool
	}{
		{"0.8.1", "0.8.0", true},
		{"0.8.0", "0.8.0", true},
		{"0.7.9", "0.8.0", false},
		{"0.8.1", "0.8.1", true},
		{"1.0.0", "0.9.9", true},
		{"0.8.1-alpha", "0.8.0", true},
		{"0.8.0", "0.8.1", false},
		{"0.8", "0.8.0", true},
		{"1", "0.9.9", true},
		{"dev", "0.8.1", true},
		{"", "0.8.1", true},
		{"0.8.1", "", true},
	}
	for _, tt := range tests {
		got := MinVersionOK(tt.running, tt.required)
		if got != tt.expect {
			t.Errorf("MinVersionOK(%q, %q) = %v, want %v", tt.running, tt.required, got, tt.expect)
		}
	}
}

func TestRollbackPlan_FieldAddReverse(t *testing.T) {
	// Forward: a field was added. Reverse: remove the field -> RENAME to quarantine.
	forward := []Change{
		{Type: "add-field", Section: "doctypes", Entity: "Task", Field: "priority"},
	}
	reversed := ReverseDiff(forward)
	if len(reversed) != 1 {
		t.Fatalf("expected 1 reversed change, got %d", len(reversed))
	}
	if reversed[0].Type != "remove-field" {
		t.Errorf("expected remove-field, got %s", reversed[0].Type)
	}
	if reversed[0].Entity != "Task" || reversed[0].Field != "priority" {
		t.Errorf("unexpected entity/field: %s/%s", reversed[0].Entity, reversed[0].Field)
	}

	ddl, quarantine := GenerateRollbackPlan(reversed, "mysql")
	if len(ddl) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if len(quarantine) == 0 {
		t.Fatal("expected quarantine entry")
	}
	if contains(ddl[0], "DROP") {
		t.Errorf("rollback DDL should not contain DROP: %s", ddl[0])
	}
	if !contains(ddl[0], "RENAME COLUMN") {
		t.Errorf("rollback DDL should use RENAME COLUMN: %s", ddl[0])
	}
	if !contains(ddl[0], "_dropquar_") {
		t.Errorf("rollback DDL should rename to quarantine name: %s", ddl[0])
	}
}

func TestRollbackPlan_DoctypeAddReverse(t *testing.T) {
	// Forward: a doctype was added. Reverse: remove the doctype -> RENAME TABLE to quarantine.
	forward := []Change{
		{Type: "add-doctypes", Section: "doctypes", Entity: "Project"},
	}
	reversed := ReverseDiff(forward)
	if len(reversed) != 1 {
		t.Fatalf("expected 1 reversed change, got %d", len(reversed))
	}
	if reversed[0].Type != "remove-doctypes" {
		t.Errorf("expected remove-doctypes, got %s", reversed[0].Type)
	}

	ddl, quarantine := GenerateRollbackPlan(reversed, "mysql")
	if len(ddl) == 0 {
		t.Fatal("expected at least one DDL statement")
	}
	if len(quarantine) == 0 {
		t.Fatal("expected quarantine entry")
	}
	if contains(ddl[0], "DROP") {
		t.Errorf("rollback DDL should not contain DROP: %s", ddl[0])
	}
	if !contains(ddl[0], "RENAME TO") {
		t.Errorf("rollback DDL should use RENAME TO: %s", ddl[0])
	}
	if !contains(ddl[0], "_dropquar_") {
		t.Errorf("rollback DDL should rename to quarantine name: %s", ddl[0])
	}
}

func TestRollbackPlan_FieldRemoveReverse(t *testing.T) {
	// Forward: a field was removed. Reverse: add the field back -> quarantine replay note.
	forward := []Change{
		{Type: "remove-field", Section: "doctypes", Entity: "Task", Field: "old_field"},
	}
	reversed := ReverseDiff(forward)
	if len(reversed) != 1 {
		t.Fatalf("expected 1 reversed change, got %d", len(reversed))
	}
	if reversed[0].Type != "add-field" {
		t.Errorf("expected add-field, got %s", reversed[0].Type)
	}

	ddl, quarantine := GenerateRollbackPlan(reversed, "mysql")
	if len(ddl) != 0 {
		t.Logf("DDL statements were generated (if any): %v", ddl)
	}
	if _, ok := quarantine["Task.old_field_replay_needed"]; !ok {
		t.Errorf("expected quarantine replay note for Task.old_field, got %v", quarantine)
	}
}

func TestRollbackPlan_NoChange(t *testing.T) {
	ddl, quarantine := GenerateRollbackPlan(nil, "mysql")
	if len(ddl) != 0 {
		t.Errorf("expected empty DDL for empty changes, got %d statements", len(ddl))
	}
	if len(quarantine) != 0 {
		t.Errorf("expected empty quarantine for empty changes, got %d entries", len(quarantine))
	}
}

func TestRollbackPlan_RenameReverse(t *testing.T) {
	// NOTE: ReverseDiff currently swaps "add-"/"remove-" prefixes.
	// A plain change like "modify-doctypes" with old/new values is passed
	// through with values swapped.
	forward := []Change{
		{Type: "modify-doctypes", Section: "doctypes", Entity: "Task",
			OldValue: "old_name", NewValue: "new_name"},
	}
	reversed := ReverseDiff(forward)
	if len(reversed) != 1 {
		t.Fatalf("expected 1 reversed change, got %d", len(reversed))
	}
	// Non-add/remove types are unchanged, but old/new values are swapped.
	if reversed[0].Type != "modify-doctypes" {
		t.Errorf("expected modify-doctypes, got %s", reversed[0].Type)
	}
	if reversed[0].OldValue != "new_name" || reversed[0].NewValue != "old_name" {
		t.Errorf("expected swapped values, got old=%v new=%v", reversed[0].OldValue, reversed[0].NewValue)
	}
}

func TestReverseDiffEmpty(t *testing.T) {
	result := ReverseDiff(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 changes for empty input, got %d", len(result))
	}
	result = ReverseDiff([]Change{})
	if len(result) != 0 {
		t.Errorf("expected 0 changes for empty slice, got %d", len(result))
	}
}

func TestReverseDiffOrder(t *testing.T) {
	changes := []Change{
		{Type: "add-doctypes", Section: "doctypes", Entity: "Base"},
		{Type: "add-field", Section: "doctypes", Entity: "Base", Field: "name"},
	}
	reversed := ReverseDiff(changes)
	if len(reversed) != 2 {
		t.Fatalf("expected 2 reversed changes, got %d", len(reversed))
	}
	// Order should be reverse: first remove the field, then remove the doctype.
	if reversed[0].Type != "remove-field" {
		t.Errorf("first reversed change should be remove-field, got %s", reversed[0].Type)
	}
	if reversed[1].Type != "remove-doctypes" {
		t.Errorf("second reversed change should be remove-doctypes, got %s", reversed[1].Type)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
