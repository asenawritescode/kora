package site

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigPackRejectsRetiredLifecycleAndAcceptsTriggerWorkflows(t *testing.T) {
	retired := []PackFile{{Path: "workflows/order.json", ContentType: "workflow_definition", Content: `{"id":"order-flow","version":1,"entry":"finish","steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}],"lifecycle":{"table":"Order","state_field":"status","states":[{"name":"Draft"}],"transitions":[]}}`}}
	if snapshot, _, err := ParseConfigPack("orders", retired); err == nil || snapshot != nil {
		t.Fatalf("retired lifecycle config was accepted: snapshot=%+v err=%v", snapshot, err)
	}

	current := []PackFile{{Path: "workflows/order.json", ContentType: "workflow_definition", Content: `{"id":"order-flow","version":1,"entry":"finish","trigger":{"kind":"record.created","table":"Order"},"steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}`}}
	snapshot, _, err := ParseConfigPack("orders", current)
	if err != nil {
		t.Fatal("trigger workflow was rejected:", err)
	}
	if len(snapshot.WorkflowDefinitions) != 1 || snapshot.WorkflowDefinitions[0].Trigger == nil {
		t.Fatalf("trigger workflow missing from snapshot: %+v", snapshot.WorkflowDefinitions)
	}
	unsupported := []PackFile{{Path: "automations/old.json", ContentType: "unsupported", Content: `{}`}}
	if snapshot, _, err := ParseConfigPack("orders", unsupported); err == nil || snapshot != nil {
		t.Fatalf("unsupported pack input was accepted: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestParseWorkflowDefinitionsDirectoryRejectsLegacyFields(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "order.json")
	if err := os.WriteFile(path, []byte(`{"id":"order-flow","version":1,"entry":"finish","steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}],"document_type":"Order"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if definitions, err := ParseWorkflowDefinitionsDirectory(directory); err == nil || definitions != nil {
		t.Fatalf("legacy workflow fields were accepted: definitions=%+v err=%v", definitions, err)
	}
}
