package outbox

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
)

func TestPublishDueReclaimsExpiredPublishingLeaseAfterRestart(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	event := contract.EventEnvelope{
		ID:            "EVT-1",
		Type:          "kora.sales_invoice.after_insert",
		Version:       1,
		Source:        "kora.kernel",
		Site:          "test.local",
		AggregateType: "Sales Invoice",
		AggregateID:   "SINV-0001",
		OccurredAt:    now.Add(-time.Hour),
		Data:          contract.MustEncodeData(map[string]any{"doctype": "Sales Invoice"}),
	}

	mock.ExpectQuery(`SELECT id FROM _kora_outbox`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(event.ID))
	mock.ExpectExec(`UPDATE _kora_outbox
			 SET status = \?, lease_owner = \?, lease_until = \?, attempts = attempts \+ 1
			 WHERE id = \?
			   AND \(
			       \(status = 'pending' AND \(next_attempt_at IS NULL OR next_attempt_at <= \?\)\)
			       OR \(status = 'publishing' AND lease_until IS NOT NULL AND lease_until < \?\)
			   \)`).
		WithArgs(string(StatusPublishing), sqlmock.AnyArg(), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT attempts FROM _kora_outbox WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(event.ID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"attempts"}).AddRow(1))
	mock.ExpectQuery(`SELECT payload, source_sequence FROM _kora_outbox WHERE id = \?`).
		WithArgs(event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"payload", "source_sequence"}).AddRow(`{"id":"EVT-1","type":"kora.sales_invoice.after_insert","version":1,"source":"kora.kernel","site":"test.local","aggregate_type":"Sales Invoice","aggregate_id":"SINV-0001","occurred_at":"`+event.OccurredAt.Format(time.RFC3339Nano)+`","data":{"doctype":"Sales Invoice"}}`, 17))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \?, lease_owner = '', lease_until = NULL WHERE id = \? AND status = 'publishing' AND lease_owner = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	var delivered contract.EventEnvelope
	p := &Publisher{
		DB:          db,
		Destination: eventPublisherFunc(func(ctx context.Context, got contract.EventEnvelope) error { delivered = got; return nil }),
		LeaseOwner:  "lease-owner",
		LeaseTTL:    time.Minute,
		MaxAttempts: 8,
	}

	n, err := p.PublishDue(context.Background(), 1)
	if err != nil {
		t.Fatalf("PublishDue: %v", err)
	}
	if n != 1 {
		t.Fatalf("PublishDue = %d, want 1 published row", n)
	}
	if delivered.Sequence != "17" {
		t.Fatalf("legacy envelope sequence = %q, want source column sequence 17", delivered.Sequence)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

type eventPublisherFunc func(context.Context, contract.EventEnvelope) error

func (f eventPublisherFunc) Publish(ctx context.Context, event contract.EventEnvelope) error {
	return f(ctx, event)
}
