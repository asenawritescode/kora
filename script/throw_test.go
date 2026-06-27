package script

import (
	"context"
	"testing"
)

func TestThrowPropagation(t *testing.T) {
	runner := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1, Timeout: 5000})
	defer runner.Close()

	req := ExecuteRequest{
		Script:    "throw 'test rejection'",
		ScriptType: TypeDocEvent,
		ScriptName: "test_throw",
		DocType:    "Work Order",
		Event:      EventValidate,
		Document:   map[string]any{"description": "Abc"},
		User:       "test@test.local",
		UserRoles:  []string{"Admin"},
		Site:       "test.local",
	}

	_, err := runner.Execute(context.Background(), req)
	if err == nil {
		t.Error("Expected error from throw, got nil")
	} else {
		t.Logf("Throw produced error: %v", err)
	}
}

func TestThrowInIIFE(t *testing.T) {
	runner := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1, Timeout: 5000})
	defer runner.Close()

	req := ExecuteRequest{
		Script:    "var d = __kora_event__.doc; if (d.description) throw 'too short';",
		ScriptType: TypeDocEvent,
		ScriptName: "test_throw_iife",
		DocType:    "Work Order",
		Event:      EventValidate,
		Document:   map[string]any{"description": "Abc"},
		User:       "test@test.local",
		UserRoles:  []string{"Admin"},
		Site:       "test.local",
	}

	_, err := runner.Execute(context.Background(), req)
	if err == nil {
		t.Error("Expected error from throw in IIFE, got nil")
	} else {
		t.Logf("IIFE throw produced error: %v", err)
	}
}
