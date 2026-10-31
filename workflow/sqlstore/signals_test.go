package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

func eventWaitFixture(t *testing.T, correlation string) (contract.WorkflowDefinition, contract.EventEnvelope, string) {
	t.Helper()
	definition := contract.WorkflowDefinition{ID: "invoice-flow", Version: 2, Entry: "wait", Steps: []contract.StepDefinition{
		{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{EventType: "invoice.paid", CorrelationField: "invoice_id", TimeoutSeconds: 30, OnEvent: "paid", OnTimeout: "expired"}},
		{ID: "paid", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "paid"}},
		{ID: "expired", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "expired"}},
	}}
	body, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	event := contract.EventEnvelope{ID: "EVT-1", Type: "invoice.paid", Version: 1, Source: "engine", Site: "tenant-route", OccurredAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), Data: json.RawMessage(`{"invoice_id":"` + correlation + `"}`)}
	return definition, event, hex.EncodeToString(digest[:])
}

func TestAcceptSignalAdvancesMatchingSubjectWaitAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewExecutionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	definition, event, digest := eventWaitFixture(t, "INV-1")
	body, _ := json.Marshal(definition)
	eventPayload, _ := json.Marshal(event)
	databaseNow := event.OccurredAt
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_signal_capacity SET lock_id=lock_id WHERE lock_id=1")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT lock_id FROM _kora_workflow_signal_capacity WHERE lock_id=1 FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"lock_id"}).AddRow(1))
	mock.ExpectQuery("SELECT event_id FROM _kora_workflow_signal WHERE tenant_id=\\?.*ORDER BY received_at,event_id LIMIT \\? FOR UPDATE SKIP LOCKED").WithArgs("tenant-a", int64(maxWorkflowSignalBatch)).WillReturnRows(sqlmock.NewRows([]string{"event_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload_json FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?")).WithArgs("tenant-a", event.ID).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM _kora_workflow_signal WHERE tenant_id=\\?.*").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT IGNORE INTO _kora_workflow_signal .*").WithArgs("tenant-a", event.ID, event.Type, "", string(eventPayload)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT s.event_id,s.event_type,s.payload_json,i.id,i.current_step,sub.correlation_field,sub.correlation_value,sub.on_event,i.revision,i.definition_id,i.definition_version .*FOR UPDATE SKIP LOCKED").
		WithArgs("tenant-a", event.ID, int64(maxWorkflowSignalBatch)).WillReturnRows(sqlmock.NewRows([]string{"event_id", "event_type", "payload_json", "instance_id", "step_id", "field", "expected", "on_event", "revision", "definition_id", "definition_version"}).AddRow(event.ID, event.Type, string(eventPayload), "instance-1", "wait", "invoice_id", "INV-1", "paid", int64(4), "invoice-flow", int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT digest,definition_json FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?")).WithArgs("tenant-a", "invoice-flow", int64(2)).WillReturnRows(sqlmock.NewRows([]string{"digest", "definition_json"}).AddRow(digest, string(body)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?")).WithArgs("tenant-a", "instance-1").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(5)))
	mock.ExpectExec("UPDATE _kora_workflow_instance SET status='ready'.*").WithArgs("paid", "tenant-a", "instance-1", "wait", int64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=?")).WithArgs("tenant-a", "instance-1", "wait").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_subscription WHERE tenant_id=? AND instance_id=? AND step_id=?")).WithArgs("tenant-a", "instance-1", "wait").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_workflow_history .*").WithArgs("tenant-a", "instance-1", uint64(5), "wait", event.ID).WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowSourceSequence(mock, event.Site, 1)
	mock.ExpectExec("INSERT INTO _kora_outbox .*").WithArgs(sqlmock.AnyArg(), "tenant-route", int64(1), "kora.workflow.advanced", contract.CurrentVersion, "workflow_instance", "instance-1", sqlmock.AnyArg(), "pending", 0, databaseNow).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT IGNORE INTO _kora_workflow_signal_receipt .*").WithArgs("tenant-a", event.ID, "instance-1", true).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	advanced, err := store.AcceptSignal(context.Background(), workflow.Signal{Tenant: "tenant-a", Event: event})
	if err != nil || advanced != 1 {
		t.Fatalf("AcceptSignal = (%d, %v), want one advanced wait", advanced, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("signal transition transaction:", err)
	}
}

func expectWorkflowSourceSequence(mock sqlmock.Sqlmock, tenant string, sequence int64) {
	mock.ExpectExec("INSERT INTO _kora_tenant_event_sequence").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(sequence - 1))
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(last_sequence\\), 0\\) FROM _kora_event_sequence").
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(0))
	mock.ExpectExec("UPDATE _kora_tenant_event_sequence SET last_sequence = \\? WHERE id = 1").WithArgs(sequence).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestSignalCorrelationRequiresExactTopLevelString(t *testing.T) {
	cases := []struct {
		name  string
		data  string
		found bool
	}{
		{"exact", `{"invoice_id":"INV-1"}`, true},
		{"nested", `{"invoice":{"invoice_id":"INV-1"}}`, false},
		{"number", `{"invoice_id":12}`, false},
		{"missing", `{"other":"INV-1"}`, false},
		{"non-object", `[]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, found, err := eventCorrelationValue(json.RawMessage(tc.data), "invoice_id")
			if err != nil || found != tc.found || (found && value != "INV-1") {
				t.Fatalf("eventCorrelationValue = (%q, %v, %v)", value, found, err)
			}
		})
	}
}

func TestAcceptSignalRejectsEventIDContentReuse(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	_, event, _ := eventWaitFixture(t, "INV-2")
	previous := event
	previous.Data = json.RawMessage(`{"invoice_id":"INV-1"}`)
	previousPayload, err := canonicalSignalEnvelope(previous)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_signal_capacity SET lock_id=lock_id WHERE lock_id=1")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT lock_id FROM _kora_workflow_signal_capacity WHERE lock_id=1 FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"lock_id"}).AddRow(1))
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT event_id FROM _kora_workflow_signal WHERE tenant_id=\\?.*ORDER BY received_at,event_id LIMIT \\? FOR UPDATE SKIP LOCKED").WithArgs("tenant-a", int64(maxWorkflowSignalBatch)).WillReturnRows(sqlmock.NewRows([]string{"event_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload_json FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?")).WithArgs("tenant-a", event.ID).WillReturnRows(sqlmock.NewRows([]string{"payload_json"}).AddRow(string(previousPayload)))
	mock.ExpectRollback()
	_, err = store.AcceptSignalInTx(context.Background(), tx, workflow.Signal{Tenant: "tenant-a", Event: event})
	var conflict *contract.Error
	if !errors.As(err, &conflict) || conflict.Type != contract.CodeConflict {
		t.Fatalf("AcceptSignalInTx error = %v, want conflict", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback caller transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("event identity transaction:", err)
	}
}

func TestAcceptSignalCapacityRejectsAtLimitBeforeInsert(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := NewExecutionStore(database, &db.MySQLDialect{})
	if err != nil {
		t.Fatal(err)
	}
	_, event, _ := eventWaitFixture(t, "INV-1")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_signal_capacity SET lock_id=lock_id WHERE lock_id=1")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT lock_id FROM _kora_workflow_signal_capacity WHERE lock_id=1 FOR UPDATE")).WillReturnRows(sqlmock.NewRows([]string{"lock_id"}).AddRow(1))
	mock.ExpectQuery("SELECT event_id FROM _kora_workflow_signal WHERE tenant_id=\\?.*ORDER BY received_at,event_id LIMIT \\? FOR UPDATE SKIP LOCKED").WithArgs("tenant-a", int64(maxWorkflowSignalBatch)).WillReturnRows(sqlmock.NewRows([]string{"event_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload_json FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?")).WithArgs("tenant-a", event.ID).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM _kora_workflow_signal WHERE tenant_id=\\?.*").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(maxWorkflowSignalInboxRows))
	mock.ExpectRollback()

	_, err = store.AcceptSignal(context.Background(), workflow.Signal{Tenant: "tenant-a", Event: event})
	var apiErr *contract.Error
	if !errors.As(err, &apiErr) || apiErr.Type != contract.CodeResourceExhausted {
		t.Fatalf("AcceptSignal at capacity error = %v, want RESOURCE_EXHAUSTED", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("capacity backpressure transaction:", err)
	}
}
