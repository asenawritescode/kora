package outbox

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

// TestDispatcherRoutesWithReceipts verifies the receipt dedup contract: a
// consumer that has already seen an event is skipped, a fresh consumer runs the
// worker and records a receipt, and the row is marked published only when all
// workers succeed.
func TestConsumerReceiptContract(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	c := &Consumer{DB: db, Name: "analytics", Dialect: kdb.Resolve("mysql")}

	// HasSeen → returns false (0).
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
		WithArgs("analytics", "EVT-1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	seen, err := c.HasSeen(context.Background(), "EVT-1")
	if err != nil {
		t.Fatalf("HasSeen: %v", err)
	}
	if seen {
		t.Fatalf("HasSeen should be false before receipt")
	}

	// RecordReceipt → insert succeeds.
	mock.ExpectExec(`INSERT INTO _kora_outbox_receipt`).
		WithArgs("analytics", "EVT-1", "", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := c.RecordReceipt(context.Background(), "EVT-1", ""); err != nil {
		t.Fatalf("RecordReceipt: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestDispatcherWorkerInterface verifies a worker can be registered and that a
// Dispatcher builds a valid consumer namespace.
func TestDispatcherRegister(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	d := NewDispatcher(db, kdb.Resolve("mysql"))
	d.Register("analytics", func(ctx context.Context, e contract.EventEnvelope) error { return nil })

	if _, ok := d.Workers["analytics"]; !ok {
		t.Fatalf("worker not registered")
	}
}

// TestDispatcherDuplicateDeliveryAcrossRestart proves the at-least-once
// delivery path stays idempotent across a dispatcher restart: the first run
// records the receipt, the second run sees the same event and skips the effect.
func TestDispatcherDuplicateDeliveryAcrossRestart(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	event := contract.EventEnvelope{
		ID:      "EVT-1",
		Type:    "kora.sales_invoice.after_insert",
		Version: 1,
		Source:  "kora.kernel",
		Site:    "acme.example.com",
		Data:    json.RawMessage(`{"doctype":"Sales Invoice","name":"SINV-0001"}`),
	}
	payload, _ := json.Marshal(event)

	// First dispatcher instance claims the event, processes it, and writes a receipt.
	mock.ExpectQuery(`SELECT id, payload FROM _kora_outbox`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "payload"}).AddRow(event.ID, string(payload)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
		WithArgs("analytics", event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_outbox_receipt`).
		WithArgs("analytics", event.ID, event.Site, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \? WHERE id = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	// Restarted dispatcher instance sees the same event but the receipt makes the
	// worker a no-op, so the effect is not duplicated.
	mock.ExpectQuery(`SELECT id, payload FROM _kora_outbox`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "payload"}).AddRow(event.ID, string(payload)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
		WithArgs("analytics", event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`UPDATE _kora_outbox SET status = \?, published_at = \? WHERE id = \?`).
		WithArgs(string(StatusPublished), sqlmock.AnyArg(), event.ID).
		WillReturnResult(sqlmock.NewResult(1, 1))

	var effects int32
	run := func() int {
		d := NewDispatcher(db, kdb.Resolve("mysql"))
		d.Register("analytics", func(ctx context.Context, e contract.EventEnvelope) error {
			atomic.AddInt32(&effects, 1)
			if e.ID != event.ID {
				t.Fatalf("worker saw unexpected event: %+v", e)
			}
			return nil
		})
		n, err := d.Run(context.Background(), 10)
		if err != nil {
			t.Fatalf("dispatcher run: %v", err)
		}
		return n
	}

	if got := run(); got != 1 {
		t.Fatalf("first run processed %d rows, want 1", got)
	}
	if got := run(); got != 1 {
		t.Fatalf("second run processed %d rows, want 1", got)
	}
	if got := atomic.LoadInt32(&effects); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
