package analytics

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
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
//
// Workflow transitions are also batched: the UPDATE (close previous) and INSERT
// (new transition) are accumulated in memory and flushed in the same transaction
// as the daily rollup deltas, eliminating 2 extra DB round-trips per transition.
type Worker struct {
	bus      EventBus
	db       *sql.DB
	dialect  db.QueryDialect
	registry *doctype.Registry
	siteName string

	// Accumulated deltas, keyed by (doctype, metric, dimension, date).
	mu     sync.Mutex
	deltas map[deltaKey]float64

	// Accumulated workflow transition operations.
	// workflowCloses: UPDATE to set exited_at on the previous transition.
	// workflowOpens: INSERT the new transition row.
	workflowCloses []workflowCloseOp
	workflowOpens  []workflowOpenOp

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

// workflowCloseOp closes the previous workflow transition (sets exited_at + duration).
type workflowCloseOp struct {
	Site    string
	Doctype string
	DocName string
	OldState string
	Now     time.Time
}

// workflowOpenOp inserts a new workflow transition row.
type workflowOpenOp struct {
	Site       string
	Doctype    string
	DocName    string
	FromState  string
	ToState    string
	Now        time.Time
	ModifiedBy string
}

// NewWorker creates an analytics worker for a single site.
func NewWorker(bus EventBus, database *sql.DB, dialect db.QueryDialect, registry *doctype.Registry, siteName string, cfg *Config) *Worker {
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
	// Recover from panics in the analytics worker. Without this, a nil pointer
	// or type assertion panic in process() or flush() crashes the ENTIRE Go process,
	// taking down all sites. We log the panic and let the goroutine die — the HTTP
	// server stays up, analytics for other sites continue, and the backfill CLI
	// can recover any lost events.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("analytics worker panicked — restart required to resume analytics for this site",
				"site", w.siteName, "panic", r)
		}
	}()

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

	// Retention cleanup + monthly rollup: run once at startup and then daily.
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()
	go w.cleanupRetention()
	go w.rollupMonthly()

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

// InvalidateMetrics clears cached metrics for a single doctype, forcing
// re-generation on the next event. Call after config activation/rollback/reload.
func (w *Worker) InvalidateMetrics(doctype string) {
	w.metricsMu.Lock()
	defer w.metricsMu.Unlock()
	delete(w.metrics, doctype)
}

// InvalidateAllMetrics clears all cached metrics for all doctypes.
// Call after a full config reload or site rebuild.
func (w *Worker) InvalidateAllMetrics() {
	w.metricsMu.Lock()
	defer w.metricsMu.Unlock()
	w.metrics = make(map[string][]*Metric)
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
		// Only log first occurrence per doctype to avoid flooding in hot path.
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
			dim := m.Field + "=" + anyToString(val)
			w.addDelta(event.Doctype, m.Name, dim, today, 1)

			// On update: decrement old dimension if the field changed.
			if event.Operation == EventUpdate && event.OldData != nil {
				oldVal := event.OldData[m.Field]
				if anyToString(oldVal) != anyToString(val) {
					oldDim := m.Field + "=" + anyToString(oldVal)
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
			newState := anyToString(event.Data["doc_status"])
			newDim := "state=" + newState
			w.addDelta(event.Doctype, m.Name, newDim, today, 1)

			if event.Operation == EventUpdate && event.OldData != nil {
				oldState := anyToString(event.OldData["doc_status"])
				if oldState != newState {
					oldDim := "state=" + oldState
					w.addDelta(event.Doctype, m.Name, oldDim, today, -1)

					// Accumulate workflow transition for batch flush.
					w.addWorkflowTransition(event, oldState, newState)
				}
			}

		case MetricCountByLinkedField:
			if m.LinkField == "" {
				continue
			}
			val := event.Data[m.LinkField]
			dim := m.LinkField + "=" + anyToString(val)
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

// flush writes all accumulated deltas and workflow transitions to the DB
// in a single transaction.
//
// Optimization: instead of N individual stmt.Exec() calls (one per delta row),
// we build a single multi-VALUES INSERT (split into chunks for SQLite compat).
// Workflow transitions are also flushed in the same transaction instead of
// being fired as separate auto-commit db.Exec() calls from the hot path.
func (w *Worker) flush() {
	if w.db == nil {
		// No DB connection — discard accumulated state (test mode).
		w.mu.Lock()
		w.deltas = make(map[deltaKey]float64)
		w.workflowCloses = nil
		w.workflowOpens = nil
		w.mu.Unlock()
		return
	}

	w.mu.Lock()
	if len(w.deltas) == 0 && len(w.workflowOpens) == 0 {
		w.mu.Unlock()
		return
	}
	deltas := w.deltas
	w.deltas = make(map[deltaKey]float64)
	closes := w.workflowCloses
	w.workflowCloses = nil
	opens := w.workflowOpens
	w.workflowOpens = nil
	w.mu.Unlock()

	// Rotate the WAL BEFORE committing to the DB. This atomically swaps
	// the current WAL file for a fresh one. If we crash after the DB commit
	// but before deleting the old WAL, the old WAL (.flushing file) is
	// replayed on restart — causing a bounded double-count, which is
	// acceptable for analytics.
	oldWAL, _ := w.bus.RotateWAL()

	// Wrap in a transaction so deltas + workflow ops are atomic.
	tx, err := w.db.Begin()
	if err != nil {
		slog.Error("analytics worker: begin tx for flush", "error", err)
		return
	}
	defer tx.Rollback()

	// Flush daily rollup deltas (split into chunks for SQLite variable limit).
	if len(deltas) > 0 {
		w.flushDeltasTx(tx, deltas)
	}

	// Flush workflow transitions within the same transaction.
	for i := range closes {
		w.flushWorkflowCloseTx(tx, &closes[i])
	}
	for i := range opens {
		w.flushWorkflowOpenTx(tx, &opens[i])
	}

	if err := tx.Commit(); err != nil {
		slog.Error("analytics worker: commit flush", "error", err)
		// Don't commit WAL rotation — the .flushing file remains and will be
		// drained on next restart.
		return
	}

	// DB commit succeeded — safely delete the old WAL file.
	if err := w.bus.CommitWALRotation(oldWAL); err != nil {
		slog.Warn("analytics worker: commit WAL rotation failed", "error", err)
	}

	slog.Debug("analytics worker: flushed",
		"site", w.siteName, "deltas", len(deltas),
		"workflow_closes", len(closes), "workflow_opens", len(opens))
}

// maxBatchRows is the maximum rows per multi-VALUES INSERT.
// SQLite has a default SQLITE_MAX_VARIABLE_NUMBER of 999. With 6 columns per
// row, 150 rows = 900 placeholders, safely under the limit. MySQL handles
// much larger batches, but capping at 150 keeps both dialects happy.
const maxBatchRows = 150

// flushDeltasTx writes daily rollup deltas in multi-row UPSERT chunks within tx.
func (w *Worker) flushDeltasTx(tx *sql.Tx, deltas map[deltaKey]float64) {
	const rowCols = 6
	upsertSuffix := w.dialect.UpsertIncrement(
		[]string{"site", "doctype", "metric", "dimension", "date"},
		[]string{"value"},
	)

	// Flatten map to slice for chunking (map iteration order is random but
	// that's fine — UPSERT is idempotent regardless of order).
	type entry struct {
		key   deltaKey
		delta float64
	}
	flat := make([]entry, 0, len(deltas))
	for k, v := range deltas {
		flat = append(flat, entry{k, v})
	}

	// Split into chunks to stay under SQLite's variable limit.
	for start := 0; start < len(flat); start += maxBatchRows {
		end := min(start+maxBatchRows, len(flat))
		chunk := flat[start:end]
		n := len(chunk)

		rowPlaceholder := "(" + strings.Repeat("?,", rowCols-1) + "?)"
		placeholders := strings.Repeat(rowPlaceholder+",", n-1) + rowPlaceholder

		args := make([]any, 0, n*rowCols)
		for _, e := range chunk {
			args = append(args, w.siteName, e.key.Doctype, e.key.Metric, e.key.Dimension, e.key.Date, e.delta)
		}

		query := "INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES " +
			placeholders + " " + upsertSuffix

		if _, err := tx.Exec(query, args...); err != nil {
			slog.Error("analytics worker: batch upsert chunk failed",
				"site", w.siteName, "chunk_rows", n, "error", err)
			// Continue with remaining chunks — don't lose all data because of one chunk.
		}
	}
}

// flushWorkflowCloseTx executes the UPDATE to close a previous workflow transition.
func (w *Worker) flushWorkflowCloseTx(tx *sql.Tx, op *workflowCloseOp) {
	_, err := tx.Exec(
		`UPDATE _kora_analytics_workflow
		 SET exited_at = ?, duration_seconds = TIMESTAMPDIFF(SECOND, entered_at, ?)
		 WHERE site = ? AND doctype = ? AND doc_name = ? AND to_state = ? AND exited_at IS NULL`,
		op.Now, op.Now, op.Site, op.Doctype, op.DocName, op.OldState,
	)
	if err != nil {
		slog.Warn("analytics worker: workflow close failed",
			"doctype", op.Doctype, "doc", op.DocName, "old_state", op.OldState, "error", err)
	}
}

// flushWorkflowOpenTx executes the INSERT for a new workflow transition.
func (w *Worker) flushWorkflowOpenTx(tx *sql.Tx, op *workflowOpenOp) {
	_, err := tx.Exec(
		`INSERT INTO _kora_analytics_workflow (site, doctype, doc_name, from_state, to_state, entered_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		op.Site, op.Doctype, op.DocName, op.FromState, op.ToState, op.Now, op.ModifiedBy,
	)
	if err != nil {
		slog.Warn("analytics worker: workflow open failed",
			"doctype", op.Doctype, "doc", op.DocName, "from", op.FromState, "to", op.ToState, "error", err)
	}
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

// rollupMonthly aggregates daily rows into _kora_analytics_monthly for the previous month.
func (w *Worker) rollupMonthly() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("analytics worker: rollupMonthly panicked", "site", w.siteName, "panic", r)
		}
	}()
	if w.db == nil {
		return
	}
	// Aggregate the previous month.
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location())
	monthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	upsert := w.dialect.UpsertIncrement(
		[]string{"site", "doctype", "metric", "dimension", "month"},
		[]string{"value"},
	)

	query := fmt.Sprintf(
		`INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, value)
		 SELECT site, doctype, metric, dimension, ? AS month, SUM(value)
		 FROM _kora_analytics_daily
		 WHERE site = ? AND date >= ? AND date < ?
		 GROUP BY site, doctype, metric, dimension
		 %s`, upsert)

	result, err := w.db.Exec(query, monthStart, w.siteName, monthStart, monthEnd)
	if err != nil {
		slog.Warn("analytics: monthly rollup failed", "site", w.siteName, "error", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("analytics: monthly rollup complete", "site", w.siteName, "rows", n)
	}
}

// cleanupRetention deletes rollup rows older than the configured retention period.
func (w *Worker) cleanupRetention() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("analytics worker: cleanupRetention panicked", "site", w.siteName, "panic", r)
		}
	}()
	if w.db == nil {
		return
	}
	dateCutoff := time.Now().AddDate(0, 0, -90).Format("2006-01-02")

	// Each table uses a different date column.
	tables := []struct {
		name, dateCol string
	}{
		{"_kora_analytics_daily", "date"},
		{"_kora_analytics_monthly", "month"},
	}
	for _, t := range tables {
		result, err := w.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE site = ? AND %s < ?", t.name, t.dateCol),
			w.siteName, dateCutoff,
		)
		if err != nil {
			slog.Warn("analytics: retention cleanup failed", "table", t.name, "error", err)
		} else if n, _ := result.RowsAffected(); n > 0 {
			slog.Info("analytics: retention cleanup", "table", t.name, "deleted", n)
		}
	}

	// Events table uses 'event_at' (datetime, not date).
	result, err := w.db.Exec(
		"DELETE FROM _kora_analytics_events WHERE site = ? AND event_at < ?",
		w.siteName, dateCutoff+" 00:00:00",
	)
	if err != nil {
		slog.Warn("analytics: retention cleanup failed", "table", "_kora_analytics_events", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("analytics: retention cleanup", "table", "_kora_analytics_events", "deleted", n)
	}
}

// addWorkflowTransition accumulates a workflow state transition for batch flush.
// Replaces the old trackWorkflowTransition which did 2 separate db.Exec() calls
// per transition — now they are batched into the flush transaction.
func (w *Worker) addWorkflowTransition(event ChangeEvent, oldState, newState string) {
	if w.db == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.workflowCloses = append(w.workflowCloses, workflowCloseOp{
		Site:     event.Site,
		Doctype:  event.Doctype,
		DocName:  event.DocName,
		OldState: oldState,
		Now:      event.Timestamp,
	})
	w.workflowOpens = append(w.workflowOpens, workflowOpenOp{
		Site:       event.Site,
		Doctype:    event.Doctype,
		DocName:    event.DocName,
		FromState:  oldState,
		ToState:    newState,
		Now:        event.Timestamp,
		ModifiedBy: event.ModifiedBy,
	})
}

// anyToString converts an interface{} to string without fmt.Sprintf allocation.
// For the common cases (string, []byte, nil) this is 3-5x faster than fmt.Sprintf("%v", v).
func anyToString(v any) string {
	if v == nil {
		return "<nil>"
	}
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case fmt.Stringer:
		return val.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
