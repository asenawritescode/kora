package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestPublisherReleasesDatabaseAdmissionBeforeDestinationPublish(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)

	now := time.Now().UTC()
	event := contract.EventEnvelope{
		ID: "event-publish-admission", Type: "kora.invoice.after_insert", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice",
		AggregateID: "INV-3", OccurredAt: now,
		Data: contract.MustEncodeData(map[string]any{"doctype": "Invoice"}),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT id FROM _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(event.ID))
	mock.ExpectExec(`UPDATE _kora_outbox\s+SET status = \?, lease_owner = \?, lease_until = \?, attempts = attempts \+ 1`).
		WithArgs(string(StatusPublishing), sqlmock.AnyArg(), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT attempts FROM _kora_outbox WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(event.ID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(1))
	mock.ExpectQuery(`SELECT payload, source_sequence FROM _kora_outbox WHERE id = \?`).
		WithArgs(event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"payload", "source_sequence"}).AddRow(string(payload), 0))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \?, lease_owner = '', lease_until = NULL WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	admission := &dispatcherAdmissionProbe{}
	publisher := NewPublisher(database, eventPublisherFunc(func(context.Context, contract.EventEnvelope) error {
		if active := admission.active.Load(); active != 0 {
			t.Errorf("destination ran with %d DB permits held", active)
		}
		if inUse := database.Stats().InUse; inUse != 0 {
			t.Errorf("destination ran with %d database connections in use", inUse)
		}
		return nil
	}), db.Resolve("mysql"))
	publisher.WorkAdmission = admission
	publisher.LeaseOwner = "publisher"
	if published, err := publisher.PublishDue(context.Background(), 1); err != nil || published != 1 {
		t.Fatalf("PublishDue = (%d, %v), want one published event", published, err)
	}
	if active := admission.active.Load(); active != 0 {
		t.Fatalf("active permits after publish = %d, want zero", active)
	}
	if calls := admission.calls.Load(); calls != 5 {
		t.Fatalf("admission calls = %d, want selection, claim, attempts, payload and acknowledgment", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestPublishDueReturnsPublishedAcknowledgmentError(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	now := time.Now().UTC()
	event := contract.EventEnvelope{
		ID: "event-ack-failure", Type: "kora.invoice.after_insert", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice",
		AggregateID: "INV-1", OccurredAt: now,
		Data: contract.MustEncodeData(map[string]any{"doctype": "Invoice"}),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT id FROM _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(event.ID))
	mock.ExpectExec(`UPDATE _kora_outbox\s+SET status = \?, lease_owner = \?, lease_until = \?, attempts = attempts \+ 1`).
		WithArgs(string(StatusPublishing), sqlmock.AnyArg(), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT attempts FROM _kora_outbox WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(event.ID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(1))
	mock.ExpectQuery(`SELECT payload, source_sequence FROM _kora_outbox WHERE id = \?`).
		WithArgs(event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"payload", "source_sequence"}).AddRow(string(payload), 0))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \?, lease_owner = '', lease_until = NULL WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg()).
		WillReturnError(errors.New("database acknowledgment unavailable"))

	publisher := NewPublisher(database, eventPublisherFunc(func(context.Context, contract.EventEnvelope) error { return nil }), db.Resolve("mysql"))
	publisher.LeaseOwner = "publisher"
	published, err := publisher.PublishDue(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "acknowledge published event") {
		t.Fatalf("PublishDue error = %v, want surfaced acknowledgment failure", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0 when durable acknowledgment failed", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishDueDoesNotAcknowledgeAClaimTakenOverByAnotherWorker(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()

	now := time.Now().UTC()
	event := contract.EventEnvelope{
		ID: "event-stale-claim", Type: "kora.invoice.after_insert", Version: 1,
		Source: "kora.kernel", Site: "tenant-a", AggregateType: "Invoice",
		AggregateID: "INV-2", OccurredAt: now,
		Data: contract.MustEncodeData(map[string]any{"doctype": "Invoice"}),
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT id FROM _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(event.ID))
	mock.ExpectExec(`UPDATE _kora_outbox\s+SET status = \?, lease_owner = \?, lease_until = \?, attempts = attempts \+ 1`).
		WithArgs(string(StatusPublishing), sqlmock.AnyArg(), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT attempts FROM _kora_outbox WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(event.ID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(1))
	mock.ExpectQuery(`SELECT payload, source_sequence FROM _kora_outbox WHERE id = \?`).
		WithArgs(event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"payload", "source_sequence"}).AddRow(string(payload), 0))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \?, lease_owner = '', lease_until = NULL WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	publisher := NewPublisher(database, eventPublisherFunc(func(context.Context, contract.EventEnvelope) error { return nil }), db.Resolve("mysql"))
	published, err := publisher.PublishDue(context.Background(), 1)
	if !errors.Is(err, ErrLeaseLost) || !strings.Contains(err.Error(), "claim lost") {
		t.Fatalf("PublishDue error = %v, want stale claim error", err)
	}
	if published != 0 {
		t.Fatalf("published = %d, want 0 for a stale claim", published)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
