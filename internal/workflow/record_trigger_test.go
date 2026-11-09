package workflow

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestRecordTriggerEventProjection(t *testing.T) {
	data := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	identity := map[string]any{"data": map[string]any{"status": "Open"}, "table": "Task", "record_id": "TASK-1", "revision": "2"}
	withChanges := map[string]any{"table": "Task", "record_id": "TASK-1", "revision": "3", "changed_fields": []string{"status"}}
	tests := []struct {
		name     string
		event    contract.EventEnvelope
		wantOK   bool
		wantType string
	}{
		{name: "insert starts created triggers", event: contract.EventEnvelope{Type: "kora.task.after_insert", Data: data(identity)}, wantOK: true, wantType: "record.created"},
		{name: "update with changes starts updated triggers", event: contract.EventEnvelope{Type: "kora.task.after_save", Data: data(withChanges)}, wantOK: true, wantType: "record.updated"},
		{name: "update without changes is ignored", event: contract.EventEnvelope{Type: "kora.task.after_save", Data: data(identity)}},
		{name: "delete is ignored", event: contract.EventEnvelope{Type: "kora.task.after_delete", Data: data(identity)}},
		{name: "event without record identity is ignored", event: contract.EventEnvelope{Type: "kora.task.after_insert", Data: data(map[string]any{"data": map[string]any{}})}},
		{name: "legacy record event passes through", event: contract.EventEnvelope{Type: "record.updated", Data: data(withChanges)}, wantOK: true, wantType: "record.updated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RecordTriggerFromChangeEvent(tt.event)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Type != tt.wantType {
				t.Fatalf("type = %q, want %q", got.Type, tt.wantType)
			}
			var payload contract.RecordChangeEventData
			if err := json.Unmarshal(got.Data, &payload); err != nil || payload.Table != "Task" || payload.RecordID != "TASK-1" || payload.Revision == "" {
				t.Fatalf("payload = %+v err=%v", payload, err)
			}
		})
	}
}
