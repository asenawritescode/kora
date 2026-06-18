package analytics

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// Worker consumes ChangeEvents from the EventBus and pre-computes rollups
// into _kora_analytics_daily and _kora_analytics_workflow tables.
//
// Design: batch-and-merge. Instead of UPSERTing on every event, the worker
// accumulates deltas in memory and flushes periodically. Multiple increments
// to the same (metric, dimension, date) row are merged into a single UPSERT.
type Worker struct {
	bus      EventBus
	db       *sql.DB
	dialect  db.Dialect
	registry *doctype.Registry
	siteName string

	// Accumulated deltas, keyed by (doctype, metric, dimension, date).
	mu     sync.Mutex
	deltas map[deltaKey]float64

	batchSize   int
	flushEvery  time.Duration
	stopCh      chan struct{}
	stopped     bool

	// Metrics cache: auto-generated metrics per doctype, resolved once.
	metricsMu sync.RWMutex
	metrics   map[string][]*Metric // doctype → metrics
}

type deltaKey struct {
	Doctype   string
	Metric    string
	Dimension string
	Date      string // "2006-01-02" format
}

// NewWorker creates an analytics worker for a single site.
func NewWorker(bus EventBus, database *sql.DB, dialect db.Dialect, registry *doctype.Registry, siteName string, cfg *Config) *Worker {
	d, _ := time.ParseDuration(cfg.FlushInterval)
	if d <= 0 {
		d = 1 * time.Second
	}
	return &Worker{
		bus:        bus,
		db:         database,
		dialect:    dialect,
		registry:   registry,
		siteName:   siteName,
		deltas:     make(map[deltaKey]float64),
		batchSize:  cfg.BatchSize,
		flushEvery: d,
		stopCh:     make(chan struct{}),
		metrics:    make(map[string][]*Metric),
	}
}

// Start begins consuming events. Runs in a background goroutine.
// On startup, drains any WAL backlog from a previous unclean shutdown.
func (w *Worker) Start() {
	ch, err := w.bus.Subscribe()
	if err != nil {
		slog.Error("analytics worker: failed to subscribe to event bus", "error", err)
		return
	}

	slog.Info("analytics worker started", "site", w.siteName,
		"batch_size", w.batchSize, "flush_interval", w.flushEvery)

	// Drain any WAL backlog before consuming live events.
	w.drainWAL()

	flushTicker := time.NewTicker(w.flushEvery)
	defer flushTicker.Stop()

	// Retention cleanup: run once at startup and then daily.
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()
	go w.cleanupRetention()

	count := 0
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				w.flush()
				return
			}
			w.process(event)
			count++
			if count >= w.batchSize {
				w.flush()
				count = 0
			}

		case <-flushTicker.C:
			if count > 0 {
				w.flush()
				count = 0
			}

		case <-retentionTicker.C:
			go w.cleanupRetention()

		case <-w.stopCh:
			w.flush()
			return
		}
	}
}

// Stop signals the worker to flush and exit.
func (w *Worker) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
	}
}

// process handles a single ChangeEvent, resolving metrics and accumulating deltas.
func (w *Worker) process(event ChangeEvent) {
	dt := w.registry.Get(event.Doctype)
	if dt == nil {
		slog.Warn("analytics worker: unknown doctype", "doctype", event.Doctype)
		return
	}

	metrics := w.getMetrics(dt)
	if len(metrics) == 0 {
		return
	}

	today := event.Timestamp.Format("2006-01-02")

	for _, m := range metrics {
		switch m.Type {
		case MetricCount:
			w.addDelta(event.Doctype, m.Name, "", today, 1)

		case MetricCountByField:
			if m.Field == "" {
				continue
			}
			val := event.Data[m.Field]
			dim := fmt.Sprintf("%s=%v", m.Field, val)
			w.addDelta(event.Doctype, m.Name, dim, today, 1)

			// On update: decrement old dimension if the field changed.
			if event.Operation == EventUpdate && event.OldData != nil {
				oldVal := event.OldData[m.Field]
				if fmt.Sprintf("%v", oldVal) != fmt.Sprintf("%v", val) {
					oldDim := fmt.Sprintf("%s=%v", m.Field, oldVal)
					w.addDelta(event.Doctype, m.Name, oldDim, today, -1)
				}
			}

		case MetricCountByTime:
			if event.Operation == EventInsert {
				w.addDelta(event.Doctype, m.Name, "", today, 1)
			}

		case MetricSum:
			if m.Field == "" {
				continue
			}
			newVal := toFloat(event.Data[m.Field])
			var oldVal float64
			if event.Operation == EventUpdate && event.OldData != nil {
				oldVal = toFloat(event.OldData[m.Field])
			}
			// Net delta: new value minus old value (old value is 0 for inserts).
			netDelta := newVal - oldVal
			if event.Operation == EventDelete {
				netDelta = -newVal
			}
			w.addDelta(event.Doctype, m.Name, "", today, netDelta)

		case MetricStateDistribution:
			// Track document counts by workflow state.
			newState := fmt.Sprintf("%v", event.Data["doc_status"])
			newDim := "state=" + newState
			w.addDelta(event.Doctype, m.Name, newDim, today, 1)

			if event.Operation == EventUpdate && event.OldData != nil {
				oldState := fmt.Sprintf("%v", event.OldData["doc_status"])
				if oldState != newState {
					oldDim := "state=" + oldState
					w.addDelta(event.Doctype, m.Name, oldDim, today, -1)

					// Track the state transition for funnel/duration metrics.
					w.trackWorkflowTransition(event, oldState, newState)
				}
			}

		case MetricCountByLinkedField:
			// Cross-doctype: resolve Link field → linked document → extract group_by_field.
			// For now, linked field value is stored as the linked document name.
			// Full cross-doctype resolution (following the link) is a future enhancement.
			if m.LinkField == "" {
				continue
			}
			val := event.Data[m.LinkField]
			dim := fmt.Sprintf("%s=%v", m.LinkField, val)
			w.addDelta(event.Doctype, m.Name, dim, today, 1)
		}
	}
}

// getMetrics returns all metrics for a doctype, resolving auto-generated ones on first call.
func (w *Worker) getMetrics(dt *doctype.DocType) []*Metric {
	w.metricsMu.RLock()
	cached, ok := w.metrics[dt.Name]
	w.metricsMu.RUnlock()
	if ok {
		return cached
	}

	w.metricsMu.Lock()
	defer w.metricsMu.Unlock()
	// Double-check after acquiring write lock.
	if cached, ok := w.metrics[dt.Name]; ok {
		return cached
	}

	metrics := GenerateMetrics(dt)
	if dt.IsSubmittable {
		if wf := w.registry.Workflows.Get(dt.Name); wf != nil {
			metrics = append(metrics, GenerateWorkflowMetrics(dt, wf)...)
		}
	}
	w.metrics[dt.Name] = metrics
	return metrics
}

// addDelta accumulates a value change for a rollup row.
func (w *Worker) addDelta(doctype, metric, dimension, date string, delta float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := deltaKey{Doctype: doctype, Metric: metric, Dimension: dimension, Date: date}
	w.deltas[key] += delta
}

// flush writes all accumulated deltas to _kora_analytics_daily via batched UPSERTs.
func (w *Worker) flush() {
	if w.db == nil {
		// No DB connection — discard deltas (test mode).
		w.mu.Lock()
		w.deltas = make(map[deltaKey]float64)
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	if len(w.deltas) == 0 {
		w.mu.Unlock()
		return
	}
	deltas := w.deltas
	w.deltas = make(map[deltaKey]float64)
	w.mu.Unlock()

	tx, err := w.db.Begin()
	if err != nil {
		slog.Error("analytics worker: begin tx for flush", "error", err)
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value)
		 VALUES (?, ?, ?, ?, ?, ?)
		 %s`,
		w.dialect.UpsertIncrement(
			[]string{"site", "doctype", "metric", "dimension", "date"},
			[]string{"value"},
		)))
	if err != nil {
		slog.Error("analytics worker: prepare upsert", "error", err)
		return
	}
	defer stmt.Close()

	for key, delta := range deltas {
		if _, err := stmt.Exec(w.siteName, key.Doctype, key.Metric, key.Dimension, key.Date, delta); err != nil {
			slog.Error("analytics worker: upsert delta",
				"doctype", key.Doctype, "metric", key.Metric,
				"dimension", key.Dimension, "date", key.Date, "delta", delta, "error", err)
		}
	}

	if err := tx.Commit(); err != nil {
		slog.Error("analytics worker: commit flush", "error", err)
		return
	}

	slog.Debug("analytics worker: flushed deltas", "site", w.siteName, "rows", len(deltas))
}

// toFloat safely converts an interface{} to float64.
func toFloat(v any) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case int32:
		return float64(val)
	default:
		return 0
	}
}

// drainWAL replays any events spilled to disk from a previous unclean shutdown.
func (w *Worker) drainWAL() {
	count, err := w.bus.DrainWAL(func(event ChangeEvent) {
		w.process(event)
	})
	if err != nil {
		slog.Error("analytics worker: WAL drain failed", "error", err)
	}
	if count > 0 {
		w.flush()
	}
}

// cleanupRetention deletes rollup rows older than the configured retention period.
func (w *Worker) cleanupRetention() {
	if w.db == nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -90).Format("2006-01-02")
	for _, table := range []string{"_kora_analytics_daily", "_kora_analytics_events"} {
		result, err := w.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE site = ? AND date < ?", table),
			w.siteName, cutoff,
		)
		if err != nil {
			slog.Warn("analytics: retention cleanup failed", "table", table, "error", err)
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			slog.Info("analytics: retention cleanup", "table", table, "deleted", n)
		}
	}
}

// trackWorkflowTransition records a state transition in _kora_analytics_workflow.
// Called when a document's workflow state changes.
// No-op if the worker has no DB connection (e.g., in tests).
func (w *Worker) trackWorkflowTransition(event ChangeEvent, oldState, newState string) {
	if w.db == nil {
		return
	}
	now := event.Timestamp

	// Close the previous transition for this document (set exited_at + duration).
	w.db.Exec(
		`UPDATE _kora_analytics_workflow
		 SET exited_at = ?, duration_seconds = TIMESTAMPDIFF(SECOND, entered_at, ?)
		 WHERE site = ? AND doctype = ? AND doc_name = ? AND to_state = ? AND exited_at IS NULL`,
		now, now, event.Site, event.Doctype, event.DocName, oldState,
	)

	// Insert the new state transition.
	w.db.Exec(
		`INSERT INTO _kora_analytics_workflow (site, doctype, doc_name, from_state, to_state, entered_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.Site, event.Doctype, event.DocName, oldState, newState, now, event.ModifiedBy,
	)
}
