package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/doctype"
)

func TestResourceMutationPayload(t *testing.T) {
	tests := []struct {
		name string
		in   resourceMutation
		want string
	}{
		{"zero optional fields are omitted", resourceMutation{DocType: "Note"}, `{"doctype":"Note"}`},
		{"name and data are included when set", resourceMutation{DocType: "Note", Name: "N-1", Data: json.RawMessage(`{"title":"a"}`)}, `{"data":{"title":"a"},"doctype":"Note","name":"N-1"}`},
		{"delete carries no data", resourceMutation{Command: "record.delete", DocType: "Note", Name: "N-1"}, `{"doctype":"Note","name":"N-1"}`},
		{"key and version stay out of the payload", resourceMutation{DocType: "Note", IdempotencyKey: "k", ExpectedVersion: "v"}, `{"doctype":"Note"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.payload()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("payload = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestExternalOperationEventData(t *testing.T) {
	dt := &doctype.DocType{Name: "External Operation Event", Fields: []doctype.Field{
		{Fieldname: "operation", Fieldtype: "Data"}, {Fieldname: "direction", Fieldtype: "Data"}, {Fieldname: "event_type", Fieldtype: "Data"},
		{Fieldname: "previous_status", Fieldtype: "Data"}, {Fieldname: "new_status", Fieldtype: "Data"}, {Fieldname: "processing_status", Fieldtype: "Data"},
		{Fieldname: "error_message", Fieldtype: "Data"}, {Fieldname: "idempotency_key", Fieldtype: "Data"},
		{Fieldname: "received_at", Fieldtype: "Datetime"}, {Fieldname: "processed_at", Fieldtype: "Datetime"},
	}}
	operation := doctype.NewDocument("External Operation")
	operation.Name = "OP-1"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name       string
		event      externalOperationEvent
		wantKeys   []string
		wantAbsent []string
	}{
		{
			name:       "zero occurred-at leaves the timestamps unset",
			event:      externalOperationEvent{Direction: "Outbound", Type: "Initiate", Key: "k1"},
			wantKeys:   []string{"operation", "direction", "event_type", "idempotency_key"},
			wantAbsent: []string{"received_at", "processed_at"},
		},
		{
			name:     "occurred-at stamps both timestamps",
			event:    externalOperationEvent{Direction: "Inbound", Type: "Callback", Key: "k2", OccurredAt: at},
			wantKeys: []string{"received_at", "processed_at"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := externalOperationEventData(dt, operation, tt.event)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range tt.wantKeys {
				if _, ok := got[key]; !ok {
					t.Errorf("missing %q in %s", key, raw)
				}
			}
			for _, key := range tt.wantAbsent {
				if _, ok := got[key]; ok {
					t.Errorf("unexpected %q in %s", key, raw)
				}
			}
			if got["operation"] != "OP-1" || got["direction"] != tt.event.Direction || got["event_type"] != tt.event.Type || got["idempotency_key"] != tt.event.Key {
				t.Errorf("fields not mapped: %s", raw)
			}
		})
	}
}
