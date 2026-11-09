package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
)

type dispatcherAdmissionProbe struct {
	active atomic.Int32
	calls  atomic.Int32
}

func (a *dispatcherAdmissionProbe) Acquire(context.Context) (func(), error) {
	a.calls.Add(1)
	a.active.Add(1)
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			a.active.Add(-1)
		}
	}, nil
}

func TestInspectOpenWorkReportsOnlyOwnedUnpublishedEvents(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	owned, _ := json.Marshal(contract.EventEnvelope{ID: "evt-owned", Type: "invoice.issued", Version: 1, Source: "billing.ar", Site: "tenant-a"})
	other, _ := json.Marshal(contract.EventEnvelope{ID: "evt-other", Type: "order.created", Version: 1, Source: "sales.orders", Site: "tenant-a"})
	mock.ExpectQuery(`SELECT id,payload FROM _kora_outbox WHERE site=\? AND status IN \('pending','publishing','failed'\) ORDER BY created_at,id LIMIT 257`).
		WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"id", "payload"}).AddRow("evt-owned", string(owned)).AddRow("evt-other", string(other)))
	mock.ExpectQuery(`SELECT f.event_id,o.payload FROM _kora_outbox_delivery_failure f JOIN _kora_outbox o ON o.id=f.event_id AND o.site=f.site WHERE f.site=\? AND f.quarantined=\? ORDER BY f.updated_at,f.event_id LIMIT 257`).
		WithArgs("tenant-a", true).WillReturnRows(sqlmock.NewRows([]string{"event_id", "payload"}).AddRow("evt-quarantined", string(owned)).AddRow("evt-other-quarantined", string(other)))
	blockers, err := InspectOpenWork(context.Background(), database, kdb.Resolve("mysql"), "tenant-a", "billing.ar", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 2 || blockers[0].Code != "event_delivery_open" || blockers[0].Count != 1 || !reflect.DeepEqual(blockers[0].Examples, []string{"evt-owned"}) || blockers[1].Code != "event_delivery_quarantined" || blockers[1].Count != 1 || !reflect.DeepEqual(blockers[1].Examples, []string{"evt-quarantined"}) {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherReleasesDBCursorAndAdmissionBeforeExternalHandler(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	event := contract.EventEnvelope{ID: "EVT-1", Sequence: "99", Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a", Data: json.RawMessage(`{}`)}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	admission := &dispatcherAdmissionProbe{}
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	dispatcher.WorkAdmission = admission
	dispatcher.Register("external", func(_ context.Context, got contract.EventEnvelope) error {
		if got.Sequence != "17" {
			t.Errorf("delivered sequence = %q, want authoritative outbox sequence 17", got.Sequence)
		}
		if active := admission.active.Load(); active != 0 {
			t.Errorf("external handler ran with %d DB permits held", active)
		}
		if inUse := database.Stats().InUse; inUse != 0 {
			t.Errorf("external handler ran with %d database connections in use", inUse)
		}
		return nil
	})
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).
		WithArgs("external", "external", 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}).AddRow(event.ID, string(payload), int64(17)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
		WithArgs("external", event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_outbox_receipt`).
		WithArgs("external", event.ID, event.Site, int64(17), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("dispatcher Run = (%d, %v), want one processed event", processed, err)
	}
	if active := admission.active.Load(); active != 0 {
		t.Fatalf("active permits after dispatch = %d, want zero", active)
	}
	if calls := admission.calls.Load(); calls != 3 {
		t.Fatalf("admission calls = %d, want select, receipt lookup and receipt write", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}

func TestDispatcherCapsAggregateBufferedPayloadBytes(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	events := make([]contract.EventEnvelope, 4)
	rows := sqlmock.NewRows([]string{"id", "payload", "source_sequence"})
	for i := range events {
		events[i] = contract.EventEnvelope{ID: fmt.Sprintf("EVT-%d", i), Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a", Data: json.RawMessage(`{}`)}
		encoded, err := json.Marshal(events[i])
		if err != nil {
			t.Fatal(err)
		}
		payload := string(encoded) + strings.Repeat(" ", MaxEventPayloadBytes-len(encoded))
		rows.AddRow(events[i].ID, payload, int64(0))
	}
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	var delivered atomic.Int32
	dispatcher.Register("external", func(context.Context, contract.EventEnvelope) error {
		delivered.Add(1)
		return nil
	})
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).
		WithArgs("external", "external", 10).WillReturnRows(rows)
	for i := 0; i < 3; i++ {
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
			WithArgs("external", events[i].ID).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
		mock.ExpectExec(`INSERT INTO _kora_outbox_receipt`).
			WithArgs("external", events[i].ID, events[i].Site, nil, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	processed, err := dispatcher.Run(context.Background(), 10)
	if err != nil {
		t.Fatal("dispatch bounded batch:", err)
	}
	if processed != 3 || delivered.Load() != 3 {
		t.Fatalf("processed/delivered = %d/%d, want three events at the aggregate payload ceiling", processed, delivered.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet expectations:", err)
	}
}

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
		WithArgs("analytics", "EVT-1", "", nil, sqlmock.AnyArg()).
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

func TestDispatcherPatternConsumerFiltersAtQueryAndDispatch(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock:", err)
	}
	defer database.Close()
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	if err := dispatcher.RegisterForPatterns("webhooks", []string{"kora.%.after!_insert", "kora.%.after!_save"}, func(context.Context, contract.EventEnvelope) error { return nil }); err != nil {
		t.Fatal("register pattern consumer:", err)
	}
	query, args, ok := dispatcher.consumerQuery(25)
	if !ok || !strings.Contains(query, "o.event_type LIKE ? ESCAPE '!'") || !reflect.DeepEqual(args, []any{"kora.%.after!_insert", "kora.%.after!_save", "webhooks", "webhooks", 25}) {
		t.Fatalf("consumer query = (%q, %#v, %t)", query, args, ok)
	}
	if !dispatcher.matchesWorkerEvent("webhooks", "kora.invoice.after_insert") || dispatcher.matchesWorkerEvent("webhooks", "command.completed") {
		t.Fatal("pattern consumer matched the wrong event family")
	}
}

func TestApplyStoredSourceSequence(t *testing.T) {
	tests := []struct {
		name     string
		payload  contract.Revision
		stored   int64
		want     contract.Revision
		wantFail bool
	}{
		{name: "stored column overrides stale envelope", payload: "99", stored: 17, want: "17"},
		{name: "legacy column preserves payload sequence", payload: "17", want: "17"},
		{name: "unknown sequence remains unknown"},
		{name: "negative stored sequence rejected", stored: -1, wantFail: true},
		{name: "malformed legacy payload sequence rejected", payload: "01", wantFail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := contract.EventEnvelope{Sequence: tt.payload}
			err := applyStoredSourceSequence(&event, tt.stored)
			if (err != nil) != tt.wantFail {
				t.Fatalf("applyStoredSourceSequence() error = %v, wantFail %t", err, tt.wantFail)
			}
			if !tt.wantFail && event.Sequence != tt.want {
				t.Fatalf("sequence = %q, want %q", event.Sequence, tt.want)
			}
		})
	}
}

func TestDispatcherTransactionalWorkerCommitsReceiptWithEffect(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer database.Close()
	event := contract.EventEnvelope{ID: "EVT-1", Sequence: "99", Type: "request.created", Version: 1, Source: "engine", Site: "tenant-a", Data: json.RawMessage(`{}`)}
	payload, _ := json.Marshal(event)
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	var effects atomic.Int32
	if err := dispatcher.RegisterTransactional("notifications", func(ctx context.Context, tx *sql.Tx, got contract.EventEnvelope) error {
		if got.ID != event.ID {
			t.Fatalf("worker event = %q, want %q", got.ID, event.ID)
		}
		if got.Sequence != "17" {
			t.Fatalf("transactional worker sequence = %q, want authoritative outbox sequence 17", got.Sequence)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO local_effect (event_id) VALUES (?)", got.ID); err != nil {
			return err
		}
		effects.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	dispatcher.Register("notifications", func(context.Context, contract.EventEnvelope) error { return nil })
	if _, err := dispatcher.Run(context.Background(), 1); err == nil {
		t.Fatal("dispatcher did not report duplicate consumer registration")
	}
	// Use a clean dispatcher for effect/replay assertions below.
	dispatcher = NewDispatcher(database, kdb.Resolve("mysql"))
	if err := dispatcher.RegisterTransactional("notifications", func(ctx context.Context, tx *sql.Tx, got contract.EventEnvelope) error {
		if got.ID != event.ID {
			t.Fatalf("worker event = %q, want %q", got.ID, event.ID)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO local_effect (event_id) VALUES (?)", got.ID); err != nil {
			return err
		}
		effects.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// First delivery reserves the receipt before running the local effect; both
	// writes share one transaction and are committed together. Local completion
	// leaves publication pending for the separate Publisher.
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).WithArgs("notifications", "notifications", 1).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}).AddRow(event.ID, string(payload), int64(17)))
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT IGNORE INTO _kora_outbox_receipt`).WithArgs("notifications", event.ID, event.Site, int64(17), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO local_effect`).WithArgs(event.ID).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 1 {
		t.Fatalf("first Run = (%d, %v)", processed, err)
	}

	// The committed receipt removes the event from local dispatch selection;
	// the publication status is still owned by Publisher.
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).WithArgs("notifications", "notifications", 1).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}))
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("duplicate Run = (%d, %v)", processed, err)
	}
	if got := effects.Load(); got != 1 {
		t.Fatalf("local effects = %d, want exactly one", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
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

	// First dispatcher instance processes the event and writes a receipt.
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).
		WithArgs("analytics", "analytics", 10).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}).AddRow(event.ID, string(payload), int64(0)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_outbox_receipt`).
		WithArgs("analytics", event.ID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`INSERT INTO _kora_outbox_receipt`).
		WithArgs("analytics", event.ID, event.Site, nil, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// Restarted dispatcher excludes the event by its durable receipt.
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).
		WithArgs("analytics", "analytics", 10).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}))

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
	if got := run(); got != 0 {
		t.Fatalf("second run processed %d rows, want 0", got)
	}
	if got := atomic.LoadInt32(&effects); got != 1 {
		t.Fatalf("effects = %d, want 1", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestDispatcherFilteredTransactionalWorkerSkipsIrrelevantReceipt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dispatcher := NewDispatcher(database, kdb.Resolve("mysql"))
	var calls atomic.Int32
	if err := dispatcher.RegisterTransactionalForTypes("workflow-signals", []string{"invoice.paid"}, func(context.Context, *sql.Tx, contract.EventEnvelope) error {
		calls.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT o.id,o.payload,o.source_sequence FROM _kora_outbox o WHERE .*ORDER BY o.created_at,o.id LIMIT \?`).WithArgs("invoice.paid", "workflow-signals", "workflow-signals", 1).WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "source_sequence"}))
	if processed, err := dispatcher.Run(context.Background(), 1); err != nil || processed != 0 {
		t.Fatalf("Run = (%d, %v), want no event selected for irrelevant consumer", processed, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("filtered consumer calls = %d, want 0", calls.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
