package script

import (
	"testing"
	"time"
)

func TestEventTypes(t *testing.T) {
	tests := []struct {
		event    Event
		isBefore bool
		isAfter  bool
	}{
		{EventBeforeInsert, true, false},
		{EventAfterInsert, false, true},
		{EventBeforeSave, true, false},
		{EventAfterSave, false, true},
		{EventBeforeDelete, true, false},
		{EventAfterDelete, false, true},
		{EventBeforeSubmit, true, false},
		{EventAfterSubmit, false, true},
		{EventValidate, true, false},
		{EventComputed, true, false},
	}

	for _, tt := range tests {
		if got := IsBeforeEvent(tt.event); got != tt.isBefore {
			t.Errorf("IsBeforeEvent(%s) = %v, want %v", tt.event, got, tt.isBefore)
		}
		if got := IsAfterEvent(tt.event); got != tt.isAfter {
			t.Errorf("IsAfterEvent(%s) = %v, want %v", tt.event, got, tt.isAfter)
		}
	}
}

func TestTypeConstants(t *testing.T) {
	types := []Type{TypeDocEvent, TypeAPIMethod, TypeWorkflowAction, TypeScheduled}
	names := []string{"doc_event", "api_method", "workflow_action", "scheduled"}
	for i, typ := range types {
		if string(typ) != names[i] {
			t.Errorf("Type %s != %s", typ, names[i])
		}
	}
}

func TestExecuteRequest(t *testing.T) {
	req := ExecuteRequest{
		Script:     "var x = 1;",
		ScriptType: TypeDocEvent,
		ScriptName: "test",
		DocType:    "Work Order",
		Event:      EventBeforeSave,
		Document:   map[string]any{"total": 100},
		User:       "test@test.local",
		UserRoles:  []string{"Admin"},
		Site:       "test.local",
		Timeout:    5 * time.Second,
	}
	if req.Script == "" {
		t.Error("Script should not be empty")
	}
	if req.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", req.Timeout)
	}
}

func TestNoopProvider(t *testing.T) {
	p := NoopProvider{}
	doc, err := p.GetDoc("X", "Y")
	if err != nil {
		t.Errorf("GetDoc should not error: %v", err)
	}
	if doc != nil {
		t.Error("NoopProvider GetDoc should return nil")
	}

	list, err := p.GetList("X", nil, "", 10, 0)
	if err != nil {
		t.Errorf("GetList should not error: %v", err)
	}
	if list != nil {
		t.Error("NoopProvider GetList should return nil")
	}

	err = p.SaveDoc("X", nil, "")
	if err != nil {
		t.Errorf("SaveDoc should not error: %v", err)
	}

	created, err := p.CreateDoc("X", nil, "", "")
	if err != nil {
		t.Errorf("CreateDoc should not error: %v", err)
	}
	if created != nil {
		t.Error("NoopProvider CreateDoc should return nil")
	}
}

func TestScriptRecord(t *testing.T) {
	r := ScriptRecord{
		Name:       "test",
		Site:       "test.local",
		ScriptType: TypeDocEvent,
		DocType:    "Work Order",
		Event:      EventBeforeSave,
		Priority:   10,
		IsActive:   true,
		TimeoutMs:  5000,
	}
	if r.Name != "test" {
		t.Error("Name mismatch")
	}
	if r.TimeoutMs != 5000 {
		t.Error("TimeoutMs mismatch")
	}
}

func TestScriptUpdateRequest(t *testing.T) {
	active := true
	req := ScriptUpdateRequest{
		IsActive: &active,
	}
	if req.IsActive == nil || !*req.IsActive {
		t.Error("IsActive should be true")
	}
}
