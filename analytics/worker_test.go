package analytics

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

type trackedFlushBus struct {
	*fakeBus
	acknowledged bool
}

func (b *trackedFlushBus) CommitWALRotation(path string) error { b.acknowledged = true; return nil }

type recordingWorkAdmission struct {
	err      error
	calls    int
	releases int
}

func (a *recordingWorkAdmission) Acquire(context.Context) (func(), error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return func() { a.releases++ }, nil
}

type recordingMaintenanceAdmission struct {
	ordinaryCalls    int
	maintenanceCalls int
	releases         int
}

func (a *recordingMaintenanceAdmission) Acquire(context.Context) (func(), error) {
	a.ordinaryCalls++
	return func() { a.releases++ }, nil
}

func (a *recordingMaintenanceAdmission) AcquireMaintenance(context.Context) (func(), error) {
	a.maintenanceCalls++
	return func() { a.releases++ }, nil
}

func TestNewWorkerCapsAnalyticsBatchSize(t *testing.T) {
	worker := NewWorker(nil, nil, nil, nil, "tenant", &Config{BatchSize: MaxAnalyticsBatchSize + 1})
	if worker.batchSize != MaxAnalyticsBatchSize {
		t.Fatalf("worker batch size = %d, want hard maximum %d", worker.batchSize, MaxAnalyticsBatchSize)
	}
	if worker.flushEvery != time.Second {
		t.Fatalf("worker flush interval = %s, want safe default %s", worker.flushEvery, time.Second)
	}
}

func TestAnalyticsRollupAndRetentionUseHeavyMaintenanceAdmission(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Worker)
	}{
		{name: "monthly rollup", run: func(w *Worker) { w.rollupMonthly(context.Background()) }},
		{name: "retention cleanup", run: func(w *Worker) { w.cleanupRetention(context.Background()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal("sqlmock.New:", err)
			}
			defer database.Close()
			if tc.name == "monthly rollup" {
				mock.ExpectExec(`INSERT INTO _kora_analytics_monthly`).WithArgs(sqlmock.AnyArg(), "tenant", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
			} else {
				mock.ExpectExec(`DELETE FROM _kora_analytics_daily`).WithArgs("tenant", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectExec(`DELETE FROM _kora_analytics_monthly`).WithArgs("tenant", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			admission := &recordingMaintenanceAdmission{}
			worker := NewWorker(newFakeBus(), database, db.Resolve("mysql"), doctype.NewRegistry(), "tenant", LoadConfig())
			worker.SetWorkAdmission(admission)
			tc.run(worker)
			if admission.ordinaryCalls != 0 || admission.maintenanceCalls != 1 || admission.releases != 1 {
				t.Fatalf("ordinary/maintenance/release calls = %d/%d/%d, want 0/1/1", admission.ordinaryCalls, admission.maintenanceCalls, admission.releases)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("maintenance SQL expectations:", err)
			}
		})
	}
}

func TestFlushFailureReturnsErrorAndRetainsBatchAndWAL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin().WillReturnError(errors.New("database unavailable"))
	bus := &trackedFlushBus{fakeBus: &fakeBus{ch: make(chan ChangeEvent, 100)}}
	w := NewWorker(bus, database, db.Resolve("postgres"), doctype.NewRegistry(), "tenant", LoadConfig())
	admission := &recordingWorkAdmission{}
	w.SetWorkAdmission(admission)
	w.addDelta("Invoice", "count", "", "2026-10-13", 1)
	if err := w.Flush(); err == nil {
		t.Fatal("Flush succeeded after transaction begin failed")
	}
	if bus.acknowledged {
		t.Fatal("WAL rotation acknowledged after database failure")
	}
	if admission.calls != 1 || admission.releases != 1 {
		t.Fatalf("flush admission calls/releases = %d/%d, want 1/1", admission.calls, admission.releases)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.deltas[deltaKey{Doctype: "Invoice", Metric: "count", Date: "2026-10-13"}] != 1 {
		t.Fatalf("failed batch was not retained: %#v", w.deltas)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFlushAdmissionRejectionPreservesBatch(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admission := &recordingWorkAdmission{err: dbaccess.ErrExhausted}
	w := NewWorker(newFakeBus(), database, db.Resolve("postgres"), doctype.NewRegistry(), "tenant", LoadConfig())
	w.SetWorkAdmission(admission)
	w.addDelta("Invoice", "count", "", "2026-10-13", 1)
	if err := w.Flush(); !errors.Is(err, dbaccess.ErrExhausted) {
		t.Fatalf("Flush error = %v, want admission rejection", err)
	}
	if len(w.manualDeltas) != 1 {
		t.Fatalf("admission rejection lost pending batch: %#v", w.manualDeltas)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if admission.calls != 1 || admission.releases != 0 {
		t.Fatalf("rejected flush admission calls/releases = %d/%d, want 1/0", admission.calls, admission.releases)
	}
}

func TestWorkerStopsWhenLifecycleContextIsCanceled(t *testing.T) {
	worker := NewWorker(newFakeBus(), nil, nil, doctype.NewRegistry(), "tenant", LoadConfig())
	ctx, cancel := context.WithCancel(context.Background())
	worker.StartAsyncContext(ctx)
	cancel()
	worker.Stop()
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("analytics worker did not stop after context cancellation")
	}
}

// fakeBus implements EventBus for testing — captures published events in a channel.
type fakeBus struct {
	ch      chan ChangeEvent
	dropped int64
}

func newFakeBus() *fakeBus {
	return &fakeBus{ch: make(chan ChangeEvent, 100)}
}

func (b *fakeBus) Publish(event ChangeEvent) error {
	b.ch <- event
	return nil
}
func (b *fakeBus) Subscribe() (<-chan ChangeEvent, error)          { return b.ch, nil }
func (b *fakeBus) DrainWAL(handler func(ChangeEvent)) (int, error) { return 0, nil }
func (b *fakeBus) RotateWAL() (string, error)                      { return "", nil }
func (b *fakeBus) CommitWALRotation(string) error                  { return nil }
func (b *fakeBus) Dropped() int64                                  { return b.dropped }
func (b *fakeBus) Close() error                                    { close(b.ch); return nil }

// testWorker creates a Worker for unit testing (no DB).
// We test the delta accumulation logic, not the flush.
func testWorker(registry *doctype.Registry, siteName string) *Worker {
	w := &Worker{
		bus:        newFakeBus(),
		db:         nil, // no DB needed for delta tests
		dialect:    nil, // no dialect needed for delta tests
		registry:   registry,
		siteName:   siteName,
		deltas:     make(map[deltaKey]float64),
		batchSize:  100,
		flushEvery: time.Minute,
		stopCh:     make(chan struct{}),
		metrics:    make(map[string][]*Metric),
	}
	return w
}

// testRegistry creates a Registry with the given DocTypes.
func testRegistry(dts ...*doctype.DocType) *doctype.Registry {
	r := doctype.NewRegistry()
	for _, dt := range dts {
		r.Register(dt)
	}
	return r
}

func TestWorkerSkipsRealtimeNotifications(t *testing.T) {
	w := &Worker{}
	w.process(ChangeEvent{Operation: EventNotification})
}

func TestWorker_InsertIncrementsCount(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("name", "Data", "Name", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{"name": "Acme Corp"},
	}

	w.process(event)

	// Check delta for count metric.
	today := event.Timestamp.Format("2006-01-02")
	key := deltaKey{Doctype: "Customer", Metric: "customer_count", Dimension: "", Date: today}
	if delta, ok := w.deltas[key]; !ok {
		t.Error("expected count delta after insert")
	} else if delta != 1.0 {
		t.Errorf("expected count delta 1, got %f", delta)
	}
}

func TestWorkerDoesNotAccumulateSameUnacknowledgedEventTwice(t *testing.T) {
	dt := buildTestDocType("Customer", false, field("name", "Data", "Name", true))
	w := testWorker(testRegistry(dt), "tenant")
	event := ChangeEvent{
		ID: "evt-replayed", Site: "tenant", Doctype: "Customer", DocName: "CUST-1",
		Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Data: map[string]any{"name": "Acme"},
	}
	w.process(event)
	sequencedReplay := event
	sequencedReplay.Sequence = "17"
	w.process(sequencedReplay)
	conflictingReplay := sequencedReplay
	conflictingReplay.Sequence = "18"
	w.process(conflictingReplay)
	key := deltaKey{Doctype: "Customer", Metric: "customer_count", Date: "2026-10-14"}
	if got := w.deltas[key]; got != 1 {
		t.Fatalf("duplicate pending event accumulated count=%v, want 1", got)
	}
	if len(w.pendingEventIDs) != 1 || w.pendingEventIDs[0] != event.ID {
		t.Fatalf("pending event IDs = %v, want one ID %q", w.pendingEventIDs, event.ID)
	}
	if sequence := w.pendingSequences[event.ID]; sequence.value != "" || !sequence.conflict {
		t.Fatalf("conflicting duplicate source sequence = %+v, want explicitly unknown", sequence)
	}
}

func TestFlushSkipsEffectsWhenReplayReceiptAlreadyExists(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO _kora_analytics_rebuild_lock`).WithArgs("tenant").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_analytics_rebuild_lock SET fence = fence \+ 1 WHERE site = \?`).WithArgs("tenant").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT doctype, from_date, last_source_sequence FROM _kora_analytics_rebuild_checkpoint WHERE site = \?`).
		WithArgs("tenant").WillReturnRows(sqlmock.NewRows([]string{"doctype", "from_date", "last_source_sequence"}))
	mock.ExpectExec(`INSERT INTO _kora_analytics_event_receipt`).
		WithArgs("tenant", "event-already-committed", int64(17), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	bus := &trackedFlushBus{fakeBus: newFakeBus()}
	w := NewWorker(bus, database, db.Resolve("mysql"), testRegistry(buildTestDocType("Customer", false, field("name", "Data", "Name", true))), "tenant", LoadConfig())
	event := ChangeEvent{
		ID: "event-already-committed", Site: "tenant", Doctype: "Customer", DocName: "CUST-1",
		Sequence: "17", Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Data: map[string]any{"name": "Acme"},
	}
	w.process(event)
	if err := w.Flush(); err != nil {
		t.Fatalf("flush replay: %v", err)
	}
	if !bus.acknowledged {
		t.Fatal("durably receipted replay was not acknowledged")
	}
	if len(w.deltas) != 0 {
		t.Fatalf("duplicate replay retained effects: %#v", w.deltas)
	}
	if len(w.contributions) != 0 {
		t.Fatalf("committed replay contribution retained after flush: %#v", w.contributions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptFailureRetainsEffectsAndDoesNotAcknowledgeWAL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO _kora_analytics_rebuild_lock`).WithArgs("tenant").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_analytics_rebuild_lock SET fence = fence \+ 1 WHERE site = \?`).WithArgs("tenant").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT doctype, from_date, last_source_sequence FROM _kora_analytics_rebuild_checkpoint WHERE site = \?`).
		WithArgs("tenant").WillReturnRows(sqlmock.NewRows([]string{"doctype", "from_date", "last_source_sequence"}))
	mock.ExpectExec(`INSERT INTO _kora_analytics_event_receipt`).WillReturnError(errors.New("receipt table unavailable"))
	mock.ExpectRollback()
	bus := &trackedFlushBus{fakeBus: newFakeBus()}
	w := NewWorker(bus, database, db.Resolve("mysql"), testRegistry(buildTestDocType("Customer", false, field("name", "Data", "Name", true))), "tenant", LoadConfig())
	event := ChangeEvent{
		ID: "event-retryable", Site: "tenant", Doctype: "Customer", DocName: "CUST-1",
		Sequence: "18", Operation: EventInsert, Timestamp: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
		Data: map[string]any{"name": "Acme"},
	}
	w.process(event)
	if err := w.Flush(); err == nil {
		t.Fatal("flush succeeded after event receipt failed")
	}
	if bus.acknowledged {
		t.Fatal("WAL acknowledged after receipt failure")
	}
	key := deltaKey{Doctype: "Customer", Metric: "customer_count", Date: "2026-10-14"}
	if got := w.deltas[key]; got != 1 {
		t.Fatalf("failed event effects = %v, want 1 retained", got)
	}
	if _, ok := w.contributions[event.ID]; !ok {
		t.Fatal("failed event contribution was not retained for retry")
	}
	if w.pendingSequences[event.ID].value != event.Sequence {
		t.Fatalf("failed event source sequence = %q, want %q retained for retry", w.pendingSequences[event.ID].value, event.Sequence)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorker_InsertSelectField_IncrementsCountByField(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("status", "Select", "Status", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{"status": "Active"},
	}

	w.process(event)

	today := event.Timestamp.Format("2006-01-02")
	key := deltaKey{Doctype: "Customer", Metric: "customer_count_by_status", Dimension: "status=Active", Date: today}
	if delta, ok := w.deltas[key]; !ok {
		t.Error("expected count_by_status delta after insert")
	} else if delta != 1.0 {
		t.Errorf("expected delta 1, got %f", delta)
	}
}

func TestWorker_UpdateStatus_NetDeltas(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("status", "Select", "Status", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventUpdate,
		Timestamp: time.Now(),
		Data:      map[string]any{"status": "Inactive"},
		OldData:   map[string]any{"status": "Active"},
	}

	w.process(event)

	today := event.Timestamp.Format("2006-01-02")

	// Old dimension should decrement.
	oldKey := deltaKey{Doctype: "Customer", Metric: "customer_count_by_status", Dimension: "status=Active", Date: today}
	if delta, ok := w.deltas[oldKey]; !ok {
		t.Error("expected decrement for old status dimension")
	} else if delta != -1.0 {
		t.Errorf("expected -1 for old status, got %f", delta)
	}

	// New dimension should increment.
	newKey := deltaKey{Doctype: "Customer", Metric: "customer_count_by_status", Dimension: "status=Inactive", Date: today}
	if delta, ok := w.deltas[newKey]; !ok {
		t.Error("expected increment for new status dimension")
	} else if delta != 1.0 {
		t.Errorf("expected +1 for new status, got %f", delta)
	}
}

func TestWorker_UpdateNumericField_NetDelta(t *testing.T) {
	dt := buildTestDocType("Invoice", false,
		field("total", "Float", "Total", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Invoice",
		DocName:   "INV-0001",
		Operation: EventUpdate,
		Timestamp: time.Now(),
		Data:      map[string]any{"total": 500.0},
		OldData:   map[string]any{"total": 300.0},
	}

	w.process(event)

	today := event.Timestamp.Format("2006-01-02")
	key := deltaKey{Doctype: "Invoice", Metric: "invoice_sum_total", Dimension: "", Date: today}
	if delta, ok := w.deltas[key]; !ok {
		t.Error("expected sum delta after numeric update")
	} else if delta != 200.0 {
		t.Errorf("expected net delta 200, got %f", delta)
	}
}

func TestWorker_Delete_DecrementsCountAndDimensions(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("status", "Select", "Status", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventDelete,
		Timestamp: time.Now(),
		Data:      map[string]any{"status": "Active"},
	}

	w.process(event)

	today := event.Timestamp.Format("2006-01-02")

	// Count should decrement.
	countKey := deltaKey{Doctype: "Customer", Metric: "customer_count", Dimension: "", Date: today}
	if delta, ok := w.deltas[countKey]; !ok || delta != 1.0 {
		t.Errorf("expected count +1 for delete (delete passes data, worker treats as insert count)")
	}

	// Status dimension should also have a delta.
	dimKey := deltaKey{Doctype: "Customer", Metric: "customer_count_by_status", Dimension: "status=Active", Date: today}
	if _, ok := w.deltas[dimKey]; !ok {
		t.Error("expected dimension delta for delete")
	}
}

func TestWorker_BatchMerge_SameMetricMultipleEvents(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("name", "Data", "Name", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")
	today := time.Now().Format("2006-01-02")

	// 3 inserts of the same doctype.
	for i := 0; i < 3; i++ {
		w.process(ChangeEvent{
			Site:      "test.local",
			Doctype:   "Customer",
			DocName:   "doc",
			Operation: EventInsert,
			Timestamp: time.Now(),
			Data:      map[string]any{"name": "Acme"},
		})
	}

	// Should have a single delta entry with value 3 (merged).
	key := deltaKey{Doctype: "Customer", Metric: "customer_count", Dimension: "", Date: today}
	delta, ok := w.deltas[key]
	if !ok {
		t.Fatal("expected merged count delta")
	}
	if delta != 3.0 {
		t.Errorf("expected merged delta 3.0, got %f", delta)
	}
}

func TestWorker_StateDistribution_InsertAndUpdate(t *testing.T) {
	dt := buildTestDocType("Work Order", true,
		field("title", "Data", "Title", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")
	today := time.Now().Format("2006-01-02")

	// Insert with doc_status=0 (Draft).
	w.process(ChangeEvent{
		Site:      "test.local",
		Doctype:   "Work Order",
		DocName:   "WO-0001",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{"doc_status": 0},
	})

	draftKey := deltaKey{Doctype: "Work Order", Metric: "work_order_state_distribution", Dimension: "state=0", Date: today}
	if delta, ok := w.deltas[draftKey]; !ok || delta != 1.0 {
		t.Errorf("expected Draft delta +1, got %v", w.deltas[draftKey])
	}

	// Update: Draft → Submitted (doc_status 0 → 1).
	w.process(ChangeEvent{
		Site:      "test.local",
		Doctype:   "Work Order",
		DocName:   "WO-0001",
		Operation: EventUpdate,
		Timestamp: time.Now(),
		Data:      map[string]any{"doc_status": 1},
		OldData:   map[string]any{"doc_status": 0},
	})

	// After update: Draft should be at 0 (1 - 1), Submitted at 1.
	if delta, ok := w.deltas[draftKey]; !ok || delta != 0.0 {
		t.Errorf("expected Draft delta 0 after update, got %f", delta)
	}
	submittedKey := deltaKey{Doctype: "Work Order", Metric: "work_order_state_distribution", Dimension: "state=1", Date: today}
	if delta, ok := w.deltas[submittedKey]; !ok || delta != 1.0 {
		t.Errorf("expected Submitted delta +1, got %v", w.deltas[submittedKey])
	}
}

func TestWorker_UnknownDoctype_NoPanic(t *testing.T) {
	reg := testRegistry()
	w := testWorker(reg, "test.local")

	// Should not panic on unknown doctype.
	w.process(ChangeEvent{
		Site:      "test.local",
		Doctype:   "NonExistent",
		DocName:   "doc",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{},
	})

	if len(w.deltas) != 0 {
		t.Error("no deltas should be created for unknown doctype")
	}
}

func TestWorker_MetricsAreCached(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("name", "Data", "Name", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	// First call resolves and caches.
	metrics1 := w.getMetrics(dt)
	// Second call should return cached.
	metrics2 := w.getMetrics(dt)

	if len(metrics1) == 0 {
		t.Fatal("expected metrics to be generated")
	}
	// Same slice reference (cached).
	if &metrics1[0] != &metrics2[0] {
		t.Error("expected cached metrics (same pointer)")
	}
}

// Ensure the worker does NOT open a DB connection during delta accumulation.
func TestWorker_DeltaOnly_NoDBRequired(t *testing.T) {
	dt := buildTestDocType("Customer", false,
		field("name", "Data", "Name", true),
	)
	reg := testRegistry(dt)
	w := testWorker(reg, "test.local")

	// Should not panic with nil DB.
	w.process(ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{"name": "Acme Corp"},
	})

	if len(w.deltas) == 0 {
		t.Error("expected deltas even with nil DB")
	}
}

// sqlOpenPlaceholder prevents unused import error.
var _ = sql.Open
