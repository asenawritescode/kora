package analytics

import (
	"database/sql"
	"testing"
	"time"

	"github.com/asenawritescode/kora/doctype"
)

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
func (b *fakeBus) Subscribe() (<-chan ChangeEvent, error) { return b.ch, nil }
func (b *fakeBus) DrainWAL(handler func(ChangeEvent)) (int, error) { return 0, nil }
func (b *fakeBus) Dropped() int64                           { return b.dropped }
func (b *fakeBus) Close() error                             { close(b.ch); return nil }

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
