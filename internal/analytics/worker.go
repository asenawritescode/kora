package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
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
	bus           EventBus
	db            *sql.DB
	dialect       db.QueryDialect
	registry      *doctype.Registry
	siteName      string
	workAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	flushMu sync.Mutex // serializes database commits and WAL acknowledgments

	// Accumulated deltas, keyed by (doctype, metric, dimension, date).
	processMu    sync.Mutex // prevents flush from snapshotting a partially processed event
	mu           sync.Mutex
	deltas       map[deltaKey]float64
	manualDeltas map[deltaKey]float64 // additions without an event receipt identity
	// Contributions retain the association between each durable event ID and
	// the rollup mutations it produced. Flush uses this to apply only the events
	// whose SQL receipt was newly inserted in the same transaction.
	contributions        map[string]eventContribution
	unkeyedContributions []eventContribution

	// Accumulated workflow transition operations.
	// workflowCloses: UPDATE to set exited_at on the previous transition.
	// workflowOpens: INSERT the new transition row.
	workflowCloses    []workflowCloseOp
	workflowOpens     []workflowOpenOp
	pendingEventIDs   []string
	pendingEventIDSet map[string]struct{}
	pendingSequences  map[string]pendingEventSequence

	batchSize     int
	flushEvery    time.Duration
	retentionDays int
	walPending    bool
	stopCh        chan struct{}
	stopped       bool
	started       bool
	done          chan struct{}
	ctx           context.Context
	cancel        context.CancelFunc

	// Metrics cache: auto-generated metrics per doctype, resolved once.
	metricsMu sync.RWMutex
	metrics   map[string][]*Metric // doctype → metrics
}

// SetWorkAdmission attaches the endpoint-shared active-work limiter. It must
// be called before the worker starts; each flush or maintenance operation
// acquires exactly one slot for its complete unit of database work.
func (w *Worker) SetWorkAdmission(admission interface {
	Acquire(context.Context) (func(), error)
}) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		w.workAdmission = admission
	}
}

func (w *Worker) acquireMaintenance(ctx context.Context) (func(), error) {
	if w == nil || w.workAdmission == nil {
		return func() {}, nil
	}
	acquire := w.workAdmission.Acquire
	if maintenance, ok := w.workAdmission.(interface {
		AcquireMaintenance(context.Context) (func(), error)
	}); ok {
		acquire = maintenance.AcquireMaintenance
	}
	return acquire(ctx)
}

type deltaKey struct {
	BusinessScope contract.BusinessScope
	Doctype       string
	Metric        string
	Dimension     string
	Date          string // "2006-01-02" format
}

type eventContribution struct {
	sequence string
	deltas   map[deltaKey]float64
	closes   []workflowCloseOp
	opens    []workflowOpenOp
}

type pendingEventSequence struct {
	value    string
	conflict bool
}

func (c *eventContribution) addDelta(scope contract.BusinessScope, doctype, metric, dimension, date string, delta float64) {
	key := deltaKey{BusinessScope: scope, Doctype: doctype, Metric: metric, Dimension: dimension, Date: date}
	if c.deltas == nil {
		c.deltas = make(map[deltaKey]float64)
	}
	c.deltas[key] += delta
}

// workflowCloseOp closes the previous workflow transition (sets exited_at + duration).
type workflowCloseOp struct {
	Site          string
	BusinessScope contract.BusinessScope
	Doctype       string
	DocName       string
	OldState      string
	Now           time.Time
}

// workflowOpenOp inserts a new workflow transition row.
type workflowOpenOp struct {
	Site          string
	BusinessScope contract.BusinessScope
	Doctype       string
	DocName       string
	FromState     string
	ToState       string
	Now           time.Time
	ModifiedBy    string
}

// NewWorker creates an analytics worker for a single site.
func NewWorker(bus EventBus, database *sql.DB, dialect db.QueryDialect, registry *doctype.Registry, siteName string, cfg *Config) *Worker {
	if cfg == nil {
		cfg = LoadConfig()
	}
	d, _ := time.ParseDuration(cfg.FlushInterval)
	if d <= 0 {
		d = 1 * time.Second
	}
	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultAnalyticsBatchSize
	}
	if batchSize > MaxAnalyticsBatchSize {
		batchSize = MaxAnalyticsBatchSize
	}
	return &Worker{
		bus:               bus,
		db:                database,
		dialect:           dialect,
		registry:          registry,
		siteName:          siteName,
		deltas:            make(map[deltaKey]float64),
		manualDeltas:      make(map[deltaKey]float64),
		batchSize:         batchSize,
		flushEvery:        d,
		retentionDays:     cfg.RetentionDays,
		stopCh:            make(chan struct{}),
		done:              make(chan struct{}),
		metrics:           make(map[string][]*Metric),
		pendingEventIDSet: make(map[string]struct{}),
		pendingSequences:  make(map[string]pendingEventSequence),
		contributions:     make(map[string]eventContribution),
	}
}

// Start consumes events until stopped. Use StartAsync for goroutine startup.
func (w *Worker) Start() {
	ctx, ok := w.markStarted(context.Background())
	if !ok {
		return
	}
	w.run(ctx)
}

// StartAsync marks the worker started before launching its goroutine so Stop
// can wait for its final flush before closing the database and event bus.
func (w *Worker) StartAsync() {
	w.StartAsyncContext(context.Background())
}

// StartAsyncContext runs the worker under ctx. Canceling ctx stops event
// consumption and runs a bounded final flush before the worker exits.
func (w *Worker) StartAsyncContext(parent context.Context) {
	ctx, ok := w.markStarted(parent)
	if ok {
		go w.run(ctx)
	}
}

func (w *Worker) markStarted(parent context.Context) (context.Context, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.stopped {
		return nil, false
	}
	if parent == nil {
		parent = context.Background()
	}
	w.ctx, w.cancel = context.WithCancel(parent)
	w.started = true
	return w.ctx, true
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	if w.cancel != nil {
		defer w.cancel()
	}
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
	w.drainWAL(ctx)

	flushTicker := time.NewTicker(w.flushEvery)
	defer flushTicker.Stop()

	// Retention cleanup + monthly rollup: run once at startup and then daily.
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()
	w.cleanupRetention(ctx)
	w.rollupMonthly(ctx)

	count := 0
	_, walDriven := w.bus.(WALBatchDrainer)
	for {
		select {
		case event, ok := <-ch:
			if !ok {
				w.flushOnShutdown()
				return
			}
			if ctx.Err() != nil {
				w.flushOnShutdown()
				return
			}
			if walDriven {
				// The local channel is a wake-up hint. WAL is the authoritative
				// queue, so overflow and fanout competition recover on the next
				// bounded scan without replaying this channel copy twice.
				w.drainWAL(ctx)
				count = 0
			} else {
				w.process(event)
				count++
				if count >= w.batchSize {
					w.flushAndLog(ctx)
					count = 0
				}
			}

		case <-flushTicker.C:
			if walDriven {
				w.drainWAL(ctx)
			} else {
				w.flushAndLog(ctx)
			}
			count = 0

		case <-retentionTicker.C:
			w.cleanupRetention(ctx)

		case <-w.stopCh:
			w.flushOnShutdown()
			return

		case <-ctx.Done():
			w.flushOnShutdown()
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

// Flush forces any buffered analytics deltas to persist immediately.
// It is safe to call even if no events are pending.
func (w *Worker) Flush() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return w.flush(ctx)
}

func (w *Worker) flushAndLog(ctx context.Context) {
	if err := w.flush(ctx); err != nil {
		slog.Error("analytics worker flush failed; batch retained for retry", "site", w.siteName, "error", err)
	}
}

func (w *Worker) flushOnShutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w.flushAndLog(ctx)
}

// Stop signals the worker to flush and exit.
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
		if w.cancel != nil {
			w.cancel()
		}
	}
	started := w.started
	w.mu.Unlock()
	if started {
		<-w.done
	}
}

// process handles a single ChangeEvent, resolving metrics and accumulating deltas.
func (w *Worker) process(event ChangeEvent) {
	w.processMu.Lock()
	defer w.processMu.Unlock()
	if !w.trackEventID(event.ID, event.Sequence) {
		return
	}
	if event.Operation != EventInsert && event.Operation != EventUpdate && event.Operation != EventDelete && event.Operation != EventSubmit && event.Operation != EventCancel {
		return
	}
	if event.BusinessScope.TenantID == "" {
		tenant := contract.TenantID(event.Site)
		if w.siteName != "" {
			tenant = contract.TenantID(w.siteName)
		}
		event.BusinessScope = contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
	}
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
	contribution := eventContribution{sequence: event.Sequence}

	for _, m := range metrics {
		date := eventDate(event, m.TimeField)
		switch m.Type {
		case MetricCount:
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, "", date, 1)

		case MetricCountByField:
			if m.Field == "" {
				continue
			}
			val := event.Data[m.Field]
			dim := m.Field + "=" + anyToString(val)
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, dim, date, 1)

			// On update: decrement old dimension if the field changed.
			if event.Operation == EventUpdate && event.OldData != nil {
				oldVal := event.OldData[m.Field]
				if anyToString(oldVal) != anyToString(val) {
					oldDim := m.Field + "=" + anyToString(oldVal)
					contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, oldDim, eventOldDate(event, m.TimeField), -1)
				}
			}

		case MetricCountByTime:
			if event.Operation == EventInsert {
				contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, "", event.Timestamp.Format("2006-01-02"), 1)
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
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, "", date, netDelta)

		case MetricSumByField:
			if m.Field == "" || m.GroupByField == "" {
				continue
			}
			newVal := toFloat(event.Data[m.Field])
			oldVal := 0.0
			if event.Operation == EventUpdate && event.OldData != nil {
				oldVal = toFloat(event.OldData[m.Field])
			}
			netDelta := newVal - oldVal
			if event.Operation == EventDelete {
				netDelta = -newVal
			}
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, m.GroupByField+"="+anyToString(event.Data[m.GroupByField]), date, netDelta)
			if event.Operation == EventUpdate && event.OldData != nil && anyToString(event.OldData[m.GroupByField]) != anyToString(event.Data[m.GroupByField]) {
				contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, m.GroupByField+"="+anyToString(event.OldData[m.GroupByField]), eventOldDate(event, m.TimeField), -oldVal)
			}

		case MetricStateDistribution:
			// Track document counts by workflow state.
			newState := anyToString(event.Data["doc_status"])
			newDim := "state=" + newState
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, newDim, date, 1)

			if event.Operation == EventUpdate && event.OldData != nil {
				oldState := anyToString(event.OldData["doc_status"])
				if oldState != newState {
					oldDim := "state=" + oldState
					contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, oldDim, eventOldDate(event, m.TimeField), -1)

					// Accumulate workflow transition for batch flush.
					contribution.addWorkflowTransition(event, oldState, newState)
				}
			}

		case MetricCountByLinkedField:
			if m.LinkField == "" {
				continue
			}
			val := event.Data[m.LinkField]
			dim := m.LinkField + "=" + anyToString(val)
			contribution.addDelta(event.BusinessScope, event.Doctype, m.Name, dim, date, 1)
		}
	}
	w.recordContribution(event.ID, contribution)
}

func (c *eventContribution) addWorkflowTransition(event ChangeEvent, oldState, newState string) {
	c.closes = append(c.closes, workflowCloseOp{Site: event.Site, BusinessScope: event.BusinessScope, Doctype: event.Doctype, DocName: event.DocName, OldState: oldState, Now: event.Timestamp})
	c.opens = append(c.opens, workflowOpenOp{Site: event.Site, BusinessScope: event.BusinessScope, Doctype: event.Doctype, DocName: event.DocName, FromState: oldState, ToState: newState, Now: event.Timestamp, ModifiedBy: event.ModifiedBy})
}

func (w *Worker) recordContribution(id string, contribution eventContribution) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, value := range contribution.deltas {
		w.deltas[key] += value
	}
	w.workflowCloses = append(w.workflowCloses, contribution.closes...)
	w.workflowOpens = append(w.workflowOpens, contribution.opens...)
	if id == "" {
		w.unkeyedContributions = append(w.unkeyedContributions, contribution)
		return
	}
	if w.contributions == nil {
		w.contributions = make(map[string]eventContribution)
	}
	w.contributions[id] = contribution
}

// getMetrics returns all metrics for a doctype, resolving auto-generated ones on first call.
func (w *Worker) getMetrics(dt *doctype.DocType) []*Metric {
	if dt == nil || dt.ModuleOwnerID != "" {
		return nil
	}
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

	w.metrics[dt.Name] = metrics
	return metrics
}

// addDelta accumulates a value change for a rollup row.
func (w *Worker) addDelta(doctype, metric, dimension, date string, delta float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	tenant := contract.TenantID(w.siteName)
	if tenant == "" {
		tenant = "tenant"
	}
	key := deltaKey{BusinessScope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}, Doctype: doctype, Metric: metric, Dimension: dimension, Date: date}
	w.deltas[key] += delta
	if w.manualDeltas == nil {
		w.manualDeltas = make(map[deltaKey]float64)
	}
	w.manualDeltas[key] += delta
}

func (w *Worker) trackEventID(id, sequence string) bool {
	if id == "" {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pendingEventIDSet == nil {
		w.pendingEventIDSet = make(map[string]struct{})
	}
	if w.pendingSequences == nil {
		w.pendingSequences = make(map[string]pendingEventSequence)
	}
	if _, exists := w.pendingEventIDSet[id]; exists {
		pending := w.pendingSequences[id]
		if !pending.conflict && sequence != "" {
			if pending.value == "" {
				pending.value = sequence
			} else if pending.value != sequence {
				pending.value, pending.conflict = "", true
			}
		}
		w.pendingSequences[id] = pending
		return false
	}
	w.pendingEventIDSet[id] = struct{}{}
	w.pendingEventIDs = append(w.pendingEventIDs, id)
	if sequence != "" {
		w.pendingSequences[id] = pendingEventSequence{value: sequence}
	}
	return true
}

// flush writes all accumulated deltas and workflow transitions to the DB
// in a single transaction.
//
// Optimization: instead of N individual stmt.Exec() calls (one per delta row),
// we build a single multi-VALUES INSERT (split into chunks for SQLite compat).
// Workflow transitions are also flushed in the same transaction instead of
// being fired as separate auto-commit db.Exec() calls from the hot path.
func (w *Worker) flush(ctx context.Context) error {
	// A second flush must not acknowledge WAL or publish workflow deltas while
	// the first transaction's commit/acknowledgment boundary is unresolved.
	w.flushMu.Lock()
	defer w.flushMu.Unlock()
	if w.db != nil {
		w.mu.Lock()
		hasWork := len(w.deltas) > 0 || len(w.manualDeltas) > 0 || len(w.workflowCloses) > 0 || len(w.workflowOpens) > 0 || len(w.pendingEventIDs) > 0 || len(w.unkeyedContributions) > 0 || w.walPending
		w.mu.Unlock()
		if !hasWork {
			return nil
		}
	}
	if w.db != nil && w.workAdmission != nil {
		release, err := w.workAdmission.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("analytics flush work admission: %w", err)
		}
		defer release()
	}
	w.processMu.Lock()

	if w.db == nil {
		// No DB connection — discard accumulated state (test mode).
		w.mu.Lock()
		w.deltas = make(map[deltaKey]float64)
		w.manualDeltas = make(map[deltaKey]float64)
		w.workflowCloses = nil
		w.workflowOpens = nil
		w.pendingEventIDs = nil
		w.pendingEventIDSet = make(map[string]struct{})
		w.contributions = make(map[string]eventContribution)
		w.unkeyedContributions = nil
		w.walPending = false
		w.mu.Unlock()
		w.processMu.Unlock()
		return nil
	}

	w.mu.Lock()
	if len(w.deltas) == 0 && len(w.workflowCloses) == 0 && len(w.workflowOpens) == 0 && len(w.pendingEventIDs) == 0 && !w.walPending {
		w.mu.Unlock()
		w.processMu.Unlock()
		return nil
	}
	deltas := w.deltas
	w.deltas = make(map[deltaKey]float64)
	manualDeltas := w.manualDeltas
	w.manualDeltas = make(map[deltaKey]float64)
	closes := w.workflowCloses
	w.workflowCloses = nil
	opens := w.workflowOpens
	w.workflowOpens = nil
	eventIDs := w.pendingEventIDs
	w.pendingEventIDs = nil
	eventSequences := w.pendingSequences
	w.pendingSequences = make(map[string]pendingEventSequence)
	contributions := w.contributions
	w.contributions = make(map[string]eventContribution)
	unkeyed := w.unkeyedContributions
	w.unkeyedContributions = nil
	w.mu.Unlock()
	w.processMu.Unlock()

	txDeltas := make(map[deltaKey]float64, len(manualDeltas))
	for key, value := range manualDeltas {
		txDeltas[key] = value
	}
	txCloses := make([]workflowCloseOp, 0, len(closes))
	txOpens := make([]workflowOpenOp, 0, len(opens))
	for _, contribution := range unkeyed {
		txCloses = append(txCloses, contribution.closes...)
		txOpens = append(txOpens, contribution.opens...)
	}

	// Legacy buses rotate their WAL before the transaction. The local WAL bus
	// uses per-event acknowledgment after commit so queued events remain intact.
	var oldWAL string
	var err error
	_, perEventAck := w.bus.(EventWALAcknowledger)
	if !perEventAck && w.bus != nil {
		oldWAL, err = w.bus.RotateWAL()
		if err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return fmt.Errorf("rotate analytics WAL: %w", err)
		}
	}

	// Wrap in a transaction so deltas + workflow ops are atomic.
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
		return fmt.Errorf("begin analytics flush: %w", err)
	}
	defer tx.Rollback()
	checkpoints, err := lockRebuildStateTx(ctx, tx, w.dialect, w.siteName)
	if err != nil {
		w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
		return err
	}
	for _, contribution := range unkeyed {
		accepted, err := contributionOutsideRebuiltRange(contribution, contribution.sequence, checkpoints)
		if err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return err
		}
		for key, value := range accepted {
			txDeltas[key] += value
		}
	}

	for _, id := range eventIDs {
		inserted, err := insertEventReceiptTx(ctx, tx, w.dialect, w.siteName, id, eventSequences[id].value)
		if err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return fmt.Errorf("record analytics event receipt %q: %w", id, err)
		}
		if !inserted {
			continue
		}
		if contribution, ok := contributions[id]; ok {
			accepted, err := contributionOutsideRebuiltRange(contribution, contribution.sequence, checkpoints)
			if err != nil {
				w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
				return fmt.Errorf("filter analytics event %q by rebuild cutoff: %w", id, err)
			}
			for key, value := range accepted {
				txDeltas[key] += value
			}
			txCloses = append(txCloses, contribution.closes...)
			txOpens = append(txOpens, contribution.opens...)
		}
	}

	// Flush daily rollup deltas (split into chunks for SQLite variable limit).
	if len(txDeltas) > 0 {
		if err := w.flushDeltasTx(tx, txDeltas); err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return err
		}
	}

	// Flush workflow transitions within the same transaction.
	for i := range txCloses {
		if err := w.flushWorkflowCloseTx(tx, &txCloses[i]); err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return err
		}
	}
	for i := range txOpens {
		if err := w.flushWorkflowOpenTx(tx, &txOpens[i]); err != nil {
			w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		w.restoreBatch(deltas, manualDeltas, closes, opens, eventIDs, eventSequences, contributions, unkeyed)
		// Don't commit WAL rotation — the .flushing file remains and will be
		// drained on next restart.
		return fmt.Errorf("commit analytics flush: %w", err)
	}

	// Acknowledge only event IDs included in this committed batch. This avoids
	// deleting later events that have reached the WAL but are still queued.
	var ackErr error
	if ack, ok := w.bus.(EventWALAcknowledger); ok {
		ackErr = ack.AcknowledgeWAL(eventIDs)
	} else if w.bus != nil {
		ackErr = w.bus.CommitWALRotation(oldWAL)
	}
	if ackErr != nil {
		slog.Warn("analytics worker: WAL acknowledgment failed after database commit", "error", ackErr)
		w.mu.Lock()
		w.pendingEventIDs = append(eventIDs, w.pendingEventIDs...)
		w.walPending = true
		w.mu.Unlock()
		return fmt.Errorf("acknowledge analytics WAL: %w", ackErr)
	}
	w.mu.Lock()
	w.walPending = false
	for _, id := range eventIDs {
		delete(w.pendingEventIDSet, id)
		delete(w.pendingSequences, id)
	}
	w.mu.Unlock()

	slog.Debug("analytics worker: flushed",
		"site", w.siteName, "deltas", len(txDeltas),
		"workflow_closes", len(txCloses), "workflow_opens", len(txOpens))
	return nil
}

func (w *Worker) restoreBatch(deltas, manualDeltas map[deltaKey]float64, closes []workflowCloseOp, opens []workflowOpenOp, eventIDs []string, eventSequences map[string]pendingEventSequence, contributions map[string]eventContribution, unkeyed []eventContribution) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, v := range deltas {
		w.deltas[k] += v
	}
	if w.manualDeltas == nil {
		w.manualDeltas = make(map[deltaKey]float64)
	}
	for k, v := range manualDeltas {
		w.manualDeltas[k] += v
	}
	w.workflowCloses = append(closes, w.workflowCloses...)
	w.workflowOpens = append(opens, w.workflowOpens...)
	w.pendingEventIDs = append(eventIDs, w.pendingEventIDs...)
	if w.pendingSequences == nil {
		w.pendingSequences = make(map[string]pendingEventSequence)
	}
	for id, sequence := range eventSequences {
		if current, exists := w.pendingSequences[id]; !exists {
			w.pendingSequences[id] = sequence
		} else if current.conflict || sequence.conflict || (current.value != "" && sequence.value != "" && current.value != sequence.value) {
			w.pendingSequences[id] = pendingEventSequence{conflict: true}
		} else if current.value == "" {
			w.pendingSequences[id] = sequence
		}
	}
	if w.contributions == nil {
		w.contributions = make(map[string]eventContribution)
	}
	for id, contribution := range contributions {
		w.contributions[id] = contribution
	}
	w.unkeyedContributions = append(unkeyed, w.unkeyedContributions...)
}

func insertEventReceiptTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, site, eventID, sourceSequence string) (bool, error) {
	var sequence any
	if sourceSequence != "" {
		parsed, err := strconv.ParseInt(sourceSequence, 10, 64)
		if err != nil || parsed < 0 || strconv.FormatInt(parsed, 10) != sourceSequence {
			return false, fmt.Errorf("invalid analytics event source sequence %q", sourceSequence)
		}
		sequence = parsed
	}
	query := `INSERT INTO _kora_analytics_event_receipt (site, event_id, source_sequence, processed_at) VALUES (?, ?, ?, ?)`
	driver := ""
	if named, ok := dialect.(interface{ DriverName() string }); ok {
		driver = named.DriverName()
	}
	if driver == "mysql" {
		query = `INSERT INTO _kora_analytics_event_receipt (site, event_id, source_sequence, processed_at) VALUES (?, ?, ?, ?) ON DUPLICATE KEY UPDATE event_id = event_id`
	} else {
		query += ` ON CONFLICT (site, event_id) DO NOTHING`
	}
	result, err := tx.ExecContext(ctx, db.Rebind(dialect, query), site, eventID, sequence, time.Now().UTC())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// maxBatchRows is the maximum rows per multi-VALUES INSERT.
// SQLite has a default SQLITE_MAX_VARIABLE_NUMBER of 999. With 6 columns per
// row, 150 rows = 900 placeholders, safely under the limit. MySQL handles
// much larger batches, but capping at 150 keeps both dialects happy.
const maxBatchRows = 100

func incrementUpsert(dialect db.QueryDialect, tableName string, conflictCols, incrementCols []string) string {
	if qualified, ok := dialect.(db.TableQualifiedIncrementDialect); ok {
		return qualified.UpsertIncrementForTable(tableName, conflictCols, incrementCols)
	}
	return dialect.UpsertIncrement(conflictCols, incrementCols)
}

// flushDeltasTx writes daily rollup deltas in multi-row UPSERT chunks within tx.
func (w *Worker) flushDeltasTx(tx *sql.Tx, deltas map[deltaKey]float64) error {
	const rowCols = 8
	upsertSuffix := incrementUpsert(w.dialect, "_kora_analytics_daily",
		[]string{"site", "doctype", "metric", "dimension", "date", "scope_kind", "legal_entity_id"},
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

		rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", rowCols), ",") + ")"
		placeholders := strings.TrimSuffix(strings.Repeat(rowPlaceholder+",", n), ",")

		args := make([]any, 0, n*rowCols)
		for _, e := range chunk {
			args = append(args, w.siteName, e.key.Doctype, e.key.Metric, e.key.Dimension, e.key.Date, string(e.key.BusinessScope.Kind), e.key.BusinessScope.LegalEntityID, e.delta)
		}

		query := "INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, scope_kind, legal_entity_id, value) VALUES " +
			placeholders + " " + upsertSuffix

		if _, err := tx.Exec(db.Rebind(w.dialect, query), args...); err != nil {
			return fmt.Errorf("upsert analytics chunk (%d rows): %w", n, err)
		}
	}
	return nil
}

// flushWorkflowCloseTx executes the UPDATE to close a previous workflow transition.
func (w *Worker) flushWorkflowCloseTx(tx *sql.Tx, op *workflowCloseOp) error {
	duration := "TIMESTAMPDIFF(SECOND, entered_at, ?)"
	if d, ok := w.dialect.(interface{ DriverName() string }); ok && d.DriverName() == "postgres" {
		duration = "EXTRACT(EPOCH FROM (? - entered_at))"
	}
	if d, ok := w.dialect.(interface{ DriverName() string }); ok && d.DriverName() == "libsql" {
		duration = "CAST((julianday(?) - julianday(entered_at))*86400 AS INTEGER)"
	}
	_, err := tx.Exec(
		db.Rebind(w.dialect, fmt.Sprintf(`UPDATE _kora_analytics_workflow
		 SET exited_at = ?, duration_seconds = %s
		 WHERE site = ? AND doctype = ? AND doc_name = ? AND to_state = ? AND exited_at IS NULL`,
			duration)),
		op.Now, op.Now, op.Site, op.Doctype, op.DocName, op.OldState,
	)
	if err != nil {
		return fmt.Errorf("close analytics workflow %s/%s: %w", op.Doctype, op.DocName, err)
	}
	return nil
}

// flushWorkflowOpenTx executes the INSERT for a new workflow transition.
func (w *Worker) flushWorkflowOpenTx(tx *sql.Tx, op *workflowOpenOp) error {
	_, err := tx.Exec(
		db.Rebind(w.dialect, `INSERT INTO _kora_analytics_workflow (site, doctype, doc_name, from_state, to_state, entered_at, actor)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`),
		op.Site, op.Doctype, op.DocName, op.FromState, op.ToState, op.Now, op.ModifiedBy,
	)
	if err != nil {
		return fmt.Errorf("open analytics workflow %s/%s: %w", op.Doctype, op.DocName, err)
	}
	return nil
}

// toFloat safely converts an interface{} to float64.
func toFloat(v any) float64 {
	switch val := v.(type) {
	case json.Number:
		f, _ := val.Float64()
		return f
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

// drainWAL processes one bounded durable batch. The WAL, rather than the
// in-memory channel, is authoritative for local analytics delivery.
func (w *Worker) drainWAL(ctx context.Context) {
	handler := func(event ChangeEvent) {
		w.process(event)
	}
	var count int
	var err error
	if drainer, ok := w.bus.(WALBatchDrainer); ok {
		count, err = drainer.DrainWALBatch(w.batchSize, handler)
	} else {
		count, err = w.bus.DrainWAL(handler)
	}
	if err != nil {
		slog.Error("analytics worker: WAL drain failed", "error", err)
	}
	if count > 0 {
		w.mu.Lock()
		w.walPending = true
		w.mu.Unlock()
		w.flushAndLog(ctx)
	}
}

// rollupMonthly aggregates daily rows into _kora_analytics_monthly for the previous month.
func (w *Worker) rollupMonthly(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("analytics worker: rollupMonthly panicked", "site", w.siteName, "panic", r)
		}
	}()
	if w.db == nil {
		return
	}
	release, err := w.acquireMaintenance(ctx)
	if err != nil {
		slog.Warn("analytics: monthly rollup work admission failed", "site", w.siteName, "error", err)
		return
	}
	defer release()
	// Aggregate the previous month.
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, now.Location())
	monthEnd := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	monthStartDate := monthStart.Format("2006-01-02")
	monthEndDate := monthEnd.Format("2006-01-02")

	upsert := incrementUpsert(w.dialect, "_kora_analytics_monthly",
		[]string{"site", "doctype", "metric", "dimension", "month", "scope_kind", "legal_entity_id"},
		[]string{"value"},
	)

	query := db.Rebind(w.dialect, fmt.Sprintf(
		`INSERT INTO _kora_analytics_monthly (site, doctype, metric, dimension, month, scope_kind, legal_entity_id, value)
		 SELECT site, doctype, metric, dimension, ? AS month, scope_kind, legal_entity_id, SUM(value)
		 FROM _kora_analytics_daily
		 WHERE site = ? AND date >= ? AND date < ?
		 GROUP BY site, doctype, metric, dimension, scope_kind, legal_entity_id
		 %s`, upsert))

	result, err := w.db.ExecContext(ctx, query, monthStartDate, w.siteName, monthStartDate, monthEndDate)
	if err != nil {
		slog.Warn("analytics: monthly rollup failed", "site", w.siteName, "error", err)
		return
	}
	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("analytics: monthly rollup complete", "site", w.siteName, "rows", n)
	}
}

// cleanupRetention deletes rollup rows older than the configured retention period.
func (w *Worker) cleanupRetention(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("analytics worker: cleanupRetention panicked", "site", w.siteName, "panic", r)
		}
	}()
	if w.db == nil {
		return
	}
	release, err := w.acquireMaintenance(ctx)
	if err != nil {
		slog.Warn("analytics: retention cleanup work admission failed", "site", w.siteName, "error", err)
		return
	}
	defer release()
	dateCutoff, retain := retentionCutoff(time.Now(), w.retentionDays)
	if !retain {
		return
	}

	// Each table uses a different date column.
	tables := []struct {
		name, dateCol string
	}{
		{"_kora_analytics_daily", "date"},
		{"_kora_analytics_monthly", "month"},
	}
	for _, t := range tables {
		result, err := w.db.ExecContext(ctx,
			db.Rebind(w.dialect, fmt.Sprintf("DELETE FROM %s WHERE site = ? AND %s < ?", t.name, t.dateCol)),
			w.siteName, dateCutoff,
		)
		if err != nil {
			slog.Warn("analytics: retention cleanup failed", "table", t.name, "error", err)
		} else if n, _ := result.RowsAffected(); n > 0 {
			slog.Info("analytics: retention cleanup", "table", t.name, "deleted", n)
		}
	}

	// Events table uses 'event_at' (datetime, not date).
	result, err := w.db.ExecContext(ctx,
		db.Rebind(w.dialect, "DELETE FROM _kora_analytics_events WHERE site = ? AND event_at < ?"),
		w.siteName, dateCutoff+" 00:00:00",
	)
	if err != nil {
		slog.Warn("analytics: retention cleanup failed", "table", "_kora_analytics_events", "error", err)
	} else if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("analytics: retention cleanup", "table", "_kora_analytics_events", "deleted", n)
	}
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

func eventDate(event ChangeEvent, field string) string {
	if field == "" || field == "creation" {
		return event.Timestamp.Format("2006-01-02")
	}
	return normalizeEventDate(event.Data[field], event.Timestamp)
}

func eventOldDate(event ChangeEvent, field string) string {
	if field == "" || field == "creation" || event.OldData == nil {
		return event.Timestamp.Format("2006-01-02")
	}
	return normalizeEventDate(event.OldData[field], event.Timestamp)
}

func normalizeEventDate(value any, fallback time.Time) string {
	switch v := value.(type) {
	case time.Time:
		return v.Format("2006-01-02")
	case *time.Time:
		if v != nil {
			return v.Format("2006-01-02")
		}
	case string:
		for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, v); err == nil {
				return parsed.Format("2006-01-02")
			}
		}
	}
	return fallback.Format("2006-01-02")
}
