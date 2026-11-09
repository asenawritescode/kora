package orm

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/eventstream"
)

type capturingOutbox struct{ events []contract.EventEnvelope }

func (c *capturingOutbox) Append(_ context.Context, _ *sql.Tx, event contract.EventEnvelope) error {
	c.events = append(c.events, event)
	return nil
}

func TestWriteOutboxEmitsOneEventWithTriggerFields(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "u1", PrincipalType: contract.PrincipalHuman, Site: "site-a"}
	tests := []struct {
		name     string
		op       eventstream.EventOp
		record   recordChange
		origin   *EventOrigin
		wantType string
		wantRev  string
		wantDiff []string
	}{
		{name: "insert", op: eventstream.EventInsert, record: recordChange{Revision: 1}, origin: &EventOrigin{Actor: actor, CorrelationID: "corr", CausationDepth: 2}, wantType: "kora.task.after_insert", wantRev: "1"},
		{name: "update", op: eventstream.EventUpdate, record: recordChange{Revision: 4, ChangedFields: []string{"status"}}, origin: &EventOrigin{Actor: actor}, wantType: "kora.task.after_save", wantRev: "4", wantDiff: []string{"status"}},
		{name: "delete without origin", op: eventstream.EventDelete, wantType: "kora.task.after_delete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := &capturingOutbox{}
			tx := &TxManager{Outbox: sink, SiteName: "site-a", EventOrigin: tt.origin}
			if err := tx.writeOutbox(nil, tt.op, &doctype.DocType{Name: "Task"}, "TASK-1", "u1", map[string]any{"status": "Open"}, nil, tt.record); err != nil {
				t.Fatal(err)
			}
			if len(sink.events) != 1 {
				t.Fatalf("events = %d, want exactly one", len(sink.events))
			}
			event := sink.events[0]
			if event.Type != tt.wantType {
				t.Fatalf("type = %q, want %q", event.Type, tt.wantType)
			}
			var payload struct {
				Data map[string]any `json:"data"`
				contract.RecordChangeEventData
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Data["status"] != "Open" || payload.Table != "Task" || payload.RecordID != "TASK-1" || payload.Revision != tt.wantRev || !reflect.DeepEqual(payload.ChangedFields, tt.wantDiff) {
				t.Fatalf("payload = %+v", payload)
			}
			if tt.origin != nil && (event.Actor.PrincipalID != "u1" || event.CorrelationID != tt.origin.CorrelationID || event.CausationDepth != tt.origin.CausationDepth) {
				t.Fatalf("origin not stamped: %+v", event)
			}
		})
	}
}

func TestChangedFieldsIgnoresBookkeeping(t *testing.T) {
	before := map[string]any{"status": "Open", "modified": "a", "note": "x", "removed": 1}
	after := map[string]any{"status": "Closed", "modified": "b", "note": "x"}
	if got := ChangedFields(before, after); !reflect.DeepEqual(got, []string{"removed", "status"}) {
		t.Fatalf("ChangedFields = %v", got)
	}
}
