package engine

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/natsprovider"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/script"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// ResourceMetrics registers low-cardinality gauges for process and tenant
// resource budgets. Tenant/user IDs and endpoint names are never metric labels.
type ResourceMetrics struct {
	registration metric.Registration
	once         sync.Once
	err          error
}

type ResourceMetricSources struct {
	PlatformDB              *sql.DB
	RealtimeStreams         *RealtimeStreamAdmission
	SessionCache            *auth.SessionCacheBudget
	Uploads                 *UploadAdmission
	AnalyticsQueueAdmission *analytics.AnalyticsQueueAdmission
	TenantActivation        interface {
		AdmissionSnapshot() ActivationAdmissionSnapshot
	}
	DurableWorkScan interface {
		Snapshot() DurableWorkScannerSnapshot
	}
	NATSRealtime        *natsprovider.Provider
	DeletionWorker      *PendingDeletionWorker
	AnalyticsQueryCache interface {
		AnalyticsQueryCacheSnapshot() api.AnalyticsQueryCacheSnapshot
	}
}

func RegisterResourceMetrics(tenants *knet.SiteRouter, hooks *AsyncHookWorker, scripts *script.BudgetedRunner, sources ...ResourceMetricSources) (*ResourceMetrics, error) {
	return registerResourceMetrics(otel.Meter("kora/engine/resources"), tenants, hooks, scripts, sources...)
}

func registerResourceMetrics(meter metric.Meter, tenants *knet.SiteRouter, hooks *AsyncHookWorker, scripts *script.BudgetedRunner, sources ...ResourceMetricSources) (*ResourceMetrics, error) {
	var source ResourceMetricSources
	if len(sources) > 0 {
		source = sources[0]
	}
	names := []string{
		"kora.db.connections.capacity", "kora.db.connections.in_use", "kora.db.connections.waiters", "kora.db.connections.wait_total", "kora.db.connections.wait_duration_ns", "kora.db.connections.rejected",
		"kora.db.pools.open", "kora.db.pools.in_use", "kora.db.pools.idle", "kora.db.pools.max_open", "kora.db.pools.wait_total", "kora.db.pools.wait_duration_ns",
		"kora.db.operations.total", "kora.db.operations.duration_ns", "kora.db.lock_contention.total",
		"kora.db.work.capacity", "kora.db.work.active", "kora.db.work.waiters", "kora.db.work.wait_total", "kora.db.work.wait_duration_ns", "kora.db.work.rejected", "kora.db.work.maintenance_active", "kora.db.work.maintenance_capacity",
		"kora.tenant.runtimes.active", "kora.tenant.runtimes.retiring",
		"kora.async_hooks.queue.items", "kora.async_hooks.queue.bytes", "kora.async_hooks.queue.max_items", "kora.async_hooks.queue.max_bytes", "kora.async_hooks.queue.rejected_items",
		"kora.scripts.active", "kora.scripts.waiters", "kora.scripts.active_tenants", "kora.scripts.max_active_tenants",
		"kora.realtime.streams.active", "kora.realtime.streams.capacity", "kora.realtime.streams.rejected",
		"kora.realtime.nats.subscriptions.active", "kora.realtime.nats.client_pending_messages", "kora.realtime.nats.client_pending_bytes", "kora.realtime.nats.client_pending_nonempty_observed_span_ns", "kora.realtime.nats.application_pending_items", "kora.realtime.nats.application_pending_bytes", "kora.realtime.nats.application_capacity_items", "kora.realtime.nats.application_capacity_bytes", "kora.realtime.nats.application_oldest_age_upper_bound_ns", "kora.realtime.nats.client_dropped_messages",
		"kora.process.goroutines", "kora.process.heap_alloc_bytes", "kora.process.heap_sys_bytes", "kora.process.rss_bytes",
		"kora.session_cache.entries", "kora.session_cache.bytes", "kora.session_cache.max_entries", "kora.session_cache.max_bytes",
		"kora.analytics.queue.items", "kora.analytics.queue.upper_bound_bytes", "kora.analytics.queue.max_items", "kora.analytics.queue.max_bytes", "kora.analytics.queue.oldest_age_upper_bound_ns", "kora.analytics.events.dropped", "kora.analytics.events.payload_rejected", "kora.analytics.events.wal_write_failures", "kora.analytics.listeners.overflows", "kora.analytics.listeners.registration_rejected",
		"kora.analytics.process_queued_bytes", "kora.analytics.process_queue_max_bytes", "kora.analytics.process_queue_rejected",
		"kora.analytics.listeners.process_active", "kora.analytics.listeners.process_capacity", "kora.analytics.listeners.process_rejected",
		"kora.analytics.query_cache.entries", "kora.analytics.query_cache.max_entries", "kora.analytics.query_cache.bytes", "kora.analytics.query_cache.max_bytes", "kora.analytics.query_cache.evictions", "kora.analytics.query_cache.rejected",
		"kora.session_cache.shared_bytes", "kora.session_cache.shared_max_bytes",
		"kora.uploads.active", "kora.uploads.capacity", "kora.uploads.active_reserved_bytes", "kora.uploads.reserved_bytes_capacity", "kora.uploads.rejected",
		"kora.tenant.runtimes.capacity",
		"kora.tenant.activation.waiters", "kora.tenant.activation.max_waiters", "kora.tenant.activation.wait_total", "kora.tenant.activation.wait_duration_ns", "kora.tenant.activation.rejected",
		"kora.analytics.listeners.active", "kora.analytics.listeners.capacity", "kora.analytics.listeners.queued_items", "kora.analytics.listeners.queue_upper_bound_bytes", "kora.analytics.listeners.queue_max_bytes", "kora.analytics.listeners.oldest_age_upper_bound_ns",
		"kora.tenant.durable_scan.probes", "kora.tenant.durable_scan.due_tenants", "kora.tenant.durable_scan.probe_failures", "kora.tenant.durable_scan.scan_failures", "kora.tenant.durable_scan.wake_failures", "kora.tenant.durable_scan.capacity_rejections", "kora.tenant.durable_scan.completed_sweeps", "kora.tenant.durable_scan.last_sweep_age_ns",
		"kora.scheduler.durable.active", "kora.scheduler.durable.claims", "kora.scheduler.durable.claim_failures", "kora.scheduler.durable.lease_renew_failures", "kora.scheduler.durable.lease_lost", "kora.scheduler.durable.uncertain_outcomes", "kora.scheduler.durable.completion_failures", "kora.scheduler.durable.pending_occurrences", "kora.scheduler.durable.oldest_pending_age_ns", "kora.scheduler.durable.backlog_sample_failures",
		"kora.analytics.rebuild.active", "kora.analytics.rebuild.claims", "kora.analytics.rebuild.claim_failures", "kora.analytics.rebuild.admission_deferrals", "kora.analytics.rebuild.execution_failures", "kora.analytics.rebuild.lease_renew_failures", "kora.analytics.rebuild.lease_lost", "kora.analytics.rebuild.completion_failures",
		"kora.outbox.pending_sampled", "kora.outbox.failed_sampled", "kora.outbox.publishing_sampled", "kora.outbox.sample_saturated", "kora.outbox.oldest_unpublished_age_ns", "kora.outbox.metrics_refresh_failures", "kora.outbox.lease_lost",
		"kora.platform.deletion.cleanup_attempts", "kora.platform.deletion.cleanup_failures", "kora.platform.deletion.admission_rejections",
		"kora.analytics.wal.pending", "kora.analytics.wal.oldest_age_ns", "kora.analytics.wal.age_unknown", "kora.analytics.wal.bytes", "kora.analytics.wal.bytes_unknown", "kora.analytics.wal.cold_bytes", "kora.analytics.wal.cold_bytes_unknown",
		"kora.cloud.product_events.queue_items", "kora.cloud.product_events.queue_capacity", "kora.cloud.product_events.dropped",
	}
	descriptions := []string{
		"Allocated physical database connection capacity across configured endpoints", "Physical database connections currently in use", "Database connection waiters", "Cumulative database connection waits", "Cumulative database connection wait duration", "Database connection admission rejections",
		"Open database/sql connections across platform and resident tenant pools", "In-use database/sql connections across platform and resident tenant pools", "Idle database/sql connections across platform and resident tenant pools", "Configured database/sql maximum open connections across platform and resident tenant pools", "Cumulative database/sql connection waits", "Cumulative database/sql connection wait duration",
		"SQL driver operations executed through the shared connector", "Cumulative duration of SQL driver operations", "Database lock contention errors observed by the shared connector",
		"Active database work capacity across configured endpoints", "Active database work units", "Database work waiters", "Cumulative database work waits", "Cumulative database work wait duration", "Database work admission rejections", "Active heavy maintenance work units", "Heavy maintenance work capacity across configured endpoints",
		"Tenant runtimes in the active directory snapshot", "Tenant runtime generations in retirement",
		"Async hook items queued", "Estimated bytes retained in the async hook queue", "Async hook queue item capacity", "Async hook queue byte capacity", "Rejected async hook items",
		"Active script executions", "Script execution waiters", "Tenant gates with active or waiting script executions", "Maximum script tenant gates",
		"Realtime streams currently using process admission", "Process-wide realtime stream capacity", "Realtime streams rejected by process admission",
		"Active NATS realtime subscriptions", "Messages pending in NATS client subscription buffers", "Bytes pending in NATS client subscription buffers", "Span between consecutive metric samples that observed a NATS client subscription backlog as nonempty; not message age", "Messages pending in Engine application subscription channels", "Conservative byte upper bound for Engine application subscription channels", "Aggregate Engine application channel item capacity", "Aggregate Engine application channel byte capacity", "Conservative upper bound on age of messages waiting in Engine application subscription channels", "Cumulative messages dropped by NATS client realtime subscriptions",
		"Current process goroutine count", "Current process heap allocation bytes", "Current process heap system bytes", "Current process resident set size from the Linux process statm interface",
		"Cached session entries across resident tenants", "Estimated bytes retained by session caches across resident tenants", "Session cache entry capacity across resident tenants", "Session cache byte capacity across resident tenants",
		"Analytics wake-up events queued across resident tenants", "Conservative serialized-payload upper bound for queued analytics events", "Analytics wake-up event capacity across resident tenants", "Analytics wake-up queue byte capacity across resident tenants", "Conservative upper bound on age of queued in-memory analytics wake-up events", "Analytics events rejected or dropped by resident buses since bus startup", "Analytics events rejected by the configured encoded-payload limit", "Analytics event WAL persistence failures", "Analytics listener events dropped because subscriber queues were full", "Analytics subscriber registrations rejected by capacity or lifecycle limits",
		"Conservative queued analytics payload bytes reserved process-wide", "Process-wide conservative analytics queue byte capacity", "Analytics wake-up or subscriber queue reservations rejected by the process-wide byte budget",
		"Active analytics listener registrations reserved across the Engine process", "Process-wide analytics listener registration capacity", "Analytics listener registrations rejected by the process-wide subscription limit",
		"Cached semantic analytics query results", "Semantic analytics query cache entry capacity", "Retained serialized semantic analytics result and key bytes", "Semantic analytics query cache byte capacity", "Cumulative semantic analytics cache evictions", "Cumulative semantic analytics results rejected by the cache byte budget",
		"Session snapshot bytes retained across process and tenant session managers", "Shared process session snapshot byte capacity",
		"Uploads admitted by process memory reservation", "Process-wide concurrent upload limit", "Conservative upload processing bytes reserved", "Process-wide conservative upload byte budget", "Upload requests rejected by process admission", "Configured resident tenant runtime capacity",
		"Tenant runtime operations waiting for activation admission", "Tenant activation waiter capacity", "Cumulative tenant activation admission waits", "Cumulative tenant activation admission wait duration", "Tenant activation admission rejections",
		"Registered analytics event listeners across resident tenants", "Configured analytics listener capacity across resident tenants", "Queued analytics listener events", "Conservative serialized-payload upper bound across analytics listener queues", "Conservative serialized-payload capacity across analytics listener queues", "Conservative upper bound on age of queued in-memory analytics subscriber events",
		"Cold-tenant durable-work database probes", "Cold tenants found with due or pending durable work", "Cold-tenant durable-work probe failures", "Directory page or cursor failures in the durable-work scanner", "Tenant activation failures while waking durable work", "Durable-work scan capacity rejections", "Completed durable-work directory sweeps", "Time since last completed durable-work directory sweep",
		"Durable scheduled occurrences currently executing", "Durable scheduled occurrences claimed", "Durable scheduled occurrence claim failures", "Durable scheduler lease renewal failures", "Durable scheduler claims fenced by lease loss", "Durable scheduled occurrences requiring reconciliation", "Durable scheduled occurrence completion failures", "Sampled pending durable scheduled occurrence count across resident tenants", "Oldest sampled pending durable scheduled occurrence age across resident tenants", "Cumulative durable scheduler backlog sample failures",
		"Analytics rebuild jobs currently executing across resident tenants", "Analytics rebuild jobs claimed by resident workers", "Analytics rebuild claim query failures", "Analytics rebuild jobs deferred by active-work admission", "Analytics rebuild execution failures", "Analytics rebuild lease renewal failures", "Analytics rebuild worker claims lost to a newer fencing token", "Analytics rebuild durable completion or release failures",
		"Sampled unpublished pending outbox rows across resident tenants; saturates at the per-state sample limit", "Sampled failed outbox rows across resident tenants; saturates at the per-state sample limit", "Sampled publishing outbox rows across resident tenants; saturates at the per-state sample limit", "Whether any resident tenant outbox state reached the bounded sample limit", "Age of the oldest sampled unpublished outbox event across resident tenants", "Cumulative outbox backlog metric refresh failures", "Cumulative outbox publisher claims fenced by a newer owner",
		"Pending tenant-deletion cleanup attempts", "Pending tenant-deletion cleanup failures", "Pending tenant-deletion cleanup operations deferred by database admission",
		"Whether any scanned analytics WAL backlog is pending", "Age of the oldest known persisted analytics WAL record", "Whether an oldest pending analytics WAL timestamp is unknown", "Bytes retained by resident analytics WAL files", "Whether resident analytics WAL bytes could not be fully measured", "Bytes retained by cold active tenant analytics WAL files in the last completed directory sweep", "Whether cold active tenant analytics WAL bytes could not be fully measured during the last completed sweep",
		"Best-effort Cloud product telemetry events waiting in resident tenant relay queues", "Configured capacity of resident tenant Cloud product telemetry queues", "Cloud product telemetry events dropped because payloads were oversized, unsupported, or queues were full",
	}
	if len(descriptions) != len(names) {
		return nil, errors.New("Engine resource metric metadata is incomplete")
	}
	units := make([]string, len(names))
	for index, name := range names {
		units[index] = resourceMetricUnit(name)
	}

	instruments := make([]metric.Int64Observable, 0, len(names))
	observables := make([]metric.Observable, 0, len(names))
	counters := map[string]bool{
		"kora.db.connections.wait_total": true, "kora.db.connections.wait_duration_ns": true, "kora.db.connections.rejected": true,
		"kora.db.pools.wait_total": true, "kora.db.pools.wait_duration_ns": true,
		"kora.db.operations.total": true, "kora.db.operations.duration_ns": true, "kora.db.lock_contention.total": true,
		"kora.db.work.wait_total": true, "kora.db.work.wait_duration_ns": true, "kora.db.work.rejected": true,
		"kora.realtime.streams.rejected": true, "kora.realtime.nats.client_dropped_messages": true, "kora.async_hooks.queue.rejected_items": true,
		"kora.uploads.rejected":                  true,
		"kora.analytics.events.payload_rejected": true, "kora.analytics.events.wal_write_failures": true,
		"kora.analytics.listeners.overflows": true, "kora.analytics.listeners.registration_rejected": true,
		"kora.analytics.process_queue_rejected":     true,
		"kora.analytics.listeners.process_rejected": true,
		"kora.tenant.activation.wait_total":         true, "kora.tenant.activation.wait_duration_ns": true, "kora.tenant.activation.rejected": true,
		"kora.tenant.durable_scan.probes": true, "kora.tenant.durable_scan.due_tenants": true,
		"kora.tenant.durable_scan.probe_failures": true, "kora.tenant.durable_scan.scan_failures": true, "kora.tenant.durable_scan.wake_failures": true,
		"kora.tenant.durable_scan.capacity_rejections": true, "kora.tenant.durable_scan.completed_sweeps": true,
		"kora.scheduler.durable.claims": true, "kora.scheduler.durable.claim_failures": true,
		"kora.scheduler.durable.lease_renew_failures": true, "kora.scheduler.durable.lease_lost": true, "kora.scheduler.durable.uncertain_outcomes": true,
		"kora.scheduler.durable.completion_failures": true, "kora.scheduler.durable.backlog_sample_failures": true,
		"kora.analytics.rebuild.claims": true, "kora.analytics.rebuild.claim_failures": true,
		"kora.analytics.rebuild.admission_deferrals": true, "kora.analytics.rebuild.execution_failures": true,
		"kora.analytics.rebuild.lease_renew_failures": true, "kora.analytics.rebuild.lease_lost": true, "kora.analytics.rebuild.completion_failures": true,
		"kora.outbox.metrics_refresh_failures": true, "kora.outbox.lease_lost": true,
		"kora.platform.deletion.cleanup_attempts": true, "kora.platform.deletion.cleanup_failures": true,
		"kora.platform.deletion.admission_rejections": true,
		"kora.cloud.product_events.dropped":           true,
	}
	for index, name := range names {
		var instrument metric.Int64Observable
		var err error
		if counters[name] {
			instrument, err = meter.Int64ObservableCounter(name, metric.WithDescription(descriptions[index]), metric.WithUnit(units[index]))
		} else {
			instrument, err = meter.Int64ObservableGauge(name, metric.WithDescription(descriptions[index]), metric.WithUnit(units[index]))
		}
		if err != nil {
			return nil, err
		}
		instruments = append(instruments, instrument)
		observables = append(observables, instrument)
	}
	metricIndex := make(map[string]int, len(names))
	for index, name := range names {
		metricIndex[name] = index
	}
	callback := func(ctx context.Context, observer metric.Observer) error {
		values := make(map[string]int64, len(names))
		for _, snapshot := range dbaccess.Snapshots() {
			values["kora.db.connections.capacity"] += int64(snapshot.Capacity)
			values["kora.db.connections.in_use"] += int64(snapshot.InUse)
			values["kora.db.connections.waiters"] += int64(snapshot.Waiters)
			values["kora.db.connections.wait_total"] += int64(snapshot.WaitCount)
			values["kora.db.connections.wait_duration_ns"] += snapshot.WaitDuration.Nanoseconds()
			values["kora.db.connections.rejected"] += int64(snapshot.Rejected)
			values["kora.db.work.capacity"] += int64(snapshot.Work.Capacity)
			values["kora.db.work.active"] += int64(snapshot.Work.Active)
			values["kora.db.work.waiters"] += int64(snapshot.Work.Waiters)
			values["kora.db.work.wait_total"] += int64(snapshot.Work.WaitCount)
			values["kora.db.work.wait_duration_ns"] += snapshot.Work.WaitDuration.Nanoseconds()
			values["kora.db.work.rejected"] += int64(snapshot.Work.Rejected)
			values["kora.db.work.maintenance_active"] += int64(snapshot.Work.MaintenanceActive)
			values["kora.db.work.maintenance_capacity"] += int64(snapshot.Work.MaintenanceCapacity)
			values["kora.db.operations.total"] += int64(snapshot.OperationCount)
			values["kora.db.operations.duration_ns"] += snapshot.OperationDuration.Nanoseconds()
			values["kora.db.lock_contention.total"] += int64(snapshot.LockContention)
		}
		observePool := func(database *sql.DB) {
			if database == nil {
				return
			}
			stats := database.Stats()
			values["kora.db.pools.open"] += int64(stats.OpenConnections)
			values["kora.db.pools.in_use"] += int64(stats.InUse)
			values["kora.db.pools.idle"] += int64(stats.Idle)
			values["kora.db.pools.max_open"] += int64(stats.MaxOpenConnections)
			values["kora.db.pools.wait_total"] += stats.WaitCount
			values["kora.db.pools.wait_duration_ns"] += stats.WaitDuration.Nanoseconds()
		}
		observePool(source.PlatformDB)
		for _, snapshot := range tenants.AllSnapshots() {
			if snapshot != nil {
				observePool(snapshot.DB)
			}
		}
		values["kora.tenant.runtimes.active"], values["kora.tenant.runtimes.retiring"] = int64Pair(tenants.ResourceCounts())
		values["kora.tenant.runtimes.capacity"] = int64(tenants.ResidentCapacity())
		if source.TenantActivation != nil {
			snapshot := source.TenantActivation.AdmissionSnapshot()
			values["kora.tenant.activation.waiters"] = int64(snapshot.Waiters)
			values["kora.tenant.activation.max_waiters"] = int64(snapshot.MaxWaiters)
			values["kora.tenant.activation.wait_total"] = int64(snapshot.WaitCount)
			values["kora.tenant.activation.wait_duration_ns"] = snapshot.WaitDuration.Nanoseconds()
			values["kora.tenant.activation.rejected"] = int64(snapshot.Rejected)
		}
		if source.DurableWorkScan != nil {
			snapshot := source.DurableWorkScan.Snapshot()
			values["kora.tenant.durable_scan.probes"] = int64(snapshot.Probes)
			values["kora.tenant.durable_scan.due_tenants"] = int64(snapshot.DueTenants)
			values["kora.tenant.durable_scan.probe_failures"] = int64(snapshot.ProbeFailures)
			values["kora.tenant.durable_scan.scan_failures"] = int64(snapshot.ScanFailures)
			values["kora.tenant.durable_scan.wake_failures"] = int64(snapshot.WakeFailures)
			values["kora.tenant.durable_scan.capacity_rejections"] = int64(snapshot.CapacityRejections)
			values["kora.tenant.durable_scan.completed_sweeps"] = int64(snapshot.CompletedSweeps)
			values["kora.tenant.durable_scan.last_sweep_age_ns"] = snapshot.LastCompletedSweepAge.Nanoseconds()
			if snapshot.ColdWALPending {
				values["kora.analytics.wal.pending"] = 1
			}
			if snapshot.ColdWALBytesKnown {
				values["kora.analytics.wal.cold_bytes"] = addMetricBytes(values["kora.analytics.wal.cold_bytes"], snapshot.ColdWALBytes)
			} else {
				values["kora.analytics.wal.cold_bytes_unknown"] = 1
			}
			if snapshot.ColdWALAgeUnknown {
				values["kora.analytics.wal.age_unknown"] = 1
			}
			if snapshot.ColdWALOldestAge.Nanoseconds() > values["kora.analytics.wal.oldest_age_ns"] {
				values["kora.analytics.wal.oldest_age_ns"] = snapshot.ColdWALOldestAge.Nanoseconds()
			}
		}
		if source.DeletionWorker != nil {
			snapshot := source.DeletionWorker.Snapshot()
			values["kora.platform.deletion.cleanup_attempts"] = int64(snapshot.Attempts)
			values["kora.platform.deletion.cleanup_failures"] = int64(snapshot.Failures)
			values["kora.platform.deletion.admission_rejections"] = int64(snapshot.AdmissionRejections)
		}
		if source.NATSRealtime != nil {
			snapshot := source.NATSRealtime.RealtimeBudget()
			values["kora.realtime.nats.subscriptions.active"] = int64(snapshot.Subscriptions)
			values["kora.realtime.nats.client_pending_messages"] = int64(snapshot.ClientPendingMessages)
			values["kora.realtime.nats.client_pending_bytes"] = int64(snapshot.ClientPendingBytes)
			values["kora.realtime.nats.client_pending_nonempty_observed_span_ns"] = snapshot.ClientPendingNonemptySpan.Nanoseconds()
			values["kora.realtime.nats.application_pending_items"] = int64(snapshot.ApplicationPendingItems)
			values["kora.realtime.nats.application_pending_bytes"] = int64(snapshot.ApplicationPendingBytes)
			values["kora.realtime.nats.application_capacity_items"] = int64(snapshot.ApplicationCapacityItems)
			values["kora.realtime.nats.application_capacity_bytes"] = int64(snapshot.ApplicationCapacityBytes)
			values["kora.realtime.nats.application_oldest_age_upper_bound_ns"] = snapshot.ApplicationOldestAge.Nanoseconds()
			values["kora.realtime.nats.client_dropped_messages"] = int64(snapshot.ClientDroppedMessages)
		}
		if hooks != nil {
			snapshot := hooks.QueueSnapshot()
			values["kora.async_hooks.queue.items"], values["kora.async_hooks.queue.bytes"] = int64(snapshot.QueuedItems), int64(snapshot.QueuedBytes)
			values["kora.async_hooks.queue.max_items"], values["kora.async_hooks.queue.max_bytes"] = int64(snapshot.MaxQueuedItems), int64(snapshot.MaxQueuedBytes)
			values["kora.async_hooks.queue.rejected_items"] = int64(snapshot.RejectedItems)
		}
		if scripts != nil {
			snapshot := scripts.Snapshot()
			values["kora.scripts.active"], values["kora.scripts.waiters"] = int64(snapshot.Active), int64(snapshot.Waiters)
			values["kora.scripts.active_tenants"], values["kora.scripts.max_active_tenants"] = int64(snapshot.ActiveTenants), int64(snapshot.MaxActiveTenants)
		}
		if source.RealtimeStreams != nil {
			snapshot := source.RealtimeStreams.Snapshot()
			values["kora.realtime.streams.active"], values["kora.realtime.streams.capacity"], values["kora.realtime.streams.rejected"] = int64(snapshot.Active), int64(snapshot.Capacity), int64(snapshot.Rejected)
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		values["kora.process.goroutines"], values["kora.process.heap_alloc_bytes"], values["kora.process.heap_sys_bytes"] = int64(runtime.NumGoroutine()), int64(memory.HeapAlloc), int64(memory.HeapSys)
		values["kora.process.rss_bytes"] = processRSSBytes()
		if tenants != nil {
			for _, loaded := range tenants.AllSites() {
				if loaded == nil {
					continue
				}
				if relay := loaded.RuntimeServiceSnapshot().AnalyticsRelay; relay != nil {
					snapshot := relay.ProductEventQueueSnapshot()
					values["kora.cloud.product_events.queue_items"] += int64(snapshot.Queued)
					values["kora.cloud.product_events.queue_capacity"] += int64(snapshot.Capacity)
					values["kora.cloud.product_events.dropped"] += int64(snapshot.Dropped)
				}
				if worker := loaded.RuntimeServiceSnapshot().Scheduler; worker != nil {
					snapshot := worker.DurableMetrics()
					values["kora.scheduler.durable.active"] += snapshot.Active
					values["kora.scheduler.durable.claims"] += snapshot.Claims
					values["kora.scheduler.durable.claim_failures"] += snapshot.ClaimFailures
					values["kora.scheduler.durable.lease_renew_failures"] += snapshot.LeaseRenewFailures
					values["kora.scheduler.durable.lease_lost"] += snapshot.LeaseLost
					values["kora.scheduler.durable.uncertain_outcomes"] += snapshot.UncertainOutcomes
					values["kora.scheduler.durable.completion_failures"] += snapshot.CompletionFailures
					values["kora.scheduler.durable.pending_occurrences"] += snapshot.PendingOccurrences
					if age := snapshot.OldestPendingAge.Nanoseconds(); age > values["kora.scheduler.durable.oldest_pending_age_ns"] {
						values["kora.scheduler.durable.oldest_pending_age_ns"] = age
					}
					values["kora.scheduler.durable.backlog_sample_failures"] += snapshot.BacklogFailures
				}
				if runner := loaded.RuntimeServiceSnapshot().AnalyticsRebuild; runner != nil {
					snapshot := runner.MetricsSnapshot()
					values["kora.analytics.rebuild.active"] += snapshot.Active
					values["kora.analytics.rebuild.claims"] += snapshot.Claims
					values["kora.analytics.rebuild.claim_failures"] += snapshot.ClaimFailures
					values["kora.analytics.rebuild.admission_deferrals"] += snapshot.AdmissionDeferrals
					values["kora.analytics.rebuild.execution_failures"] += snapshot.ExecutionFailures
					values["kora.analytics.rebuild.lease_renew_failures"] += snapshot.LeaseRenewFailures
					values["kora.analytics.rebuild.lease_lost"] += snapshot.LeaseLost
					values["kora.analytics.rebuild.completion_failures"] += snapshot.CompletionFailures
				}
				if backlog := loaded.RuntimeServiceSnapshot().OutboxBacklog; backlog != nil {
					snapshot := backlog.Snapshot(time.Now())
					values["kora.outbox.pending_sampled"] += snapshot.Pending
					values["kora.outbox.failed_sampled"] += snapshot.Failed
					values["kora.outbox.publishing_sampled"] += snapshot.Publishing
					if snapshot.Saturated {
						values["kora.outbox.sample_saturated"] = 1
					}
					values["kora.outbox.metrics_refresh_failures"] += snapshot.RefreshFailures
					values["kora.outbox.lease_lost"] += snapshot.LeaseLost
					if age := snapshot.OldestUnpublishedAge.Nanoseconds(); age > values["kora.outbox.oldest_unpublished_age_ns"] {
						values["kora.outbox.oldest_unpublished_age_ns"] = age
					}
				}
				if loaded.SessionManager != nil {
					sessionCache := loaded.SessionManager.CacheSnapshot()
					values["kora.session_cache.entries"] += int64(sessionCache.Entries)
					values["kora.session_cache.bytes"] += int64(sessionCache.Bytes)
					values["kora.session_cache.max_entries"] += int64(sessionCache.MaxEntries)
					values["kora.session_cache.max_bytes"] += int64(sessionCache.MaxBytes)
				}
				if snapshotter, ok := loaded.AnalyticsEventBus.(interface {
					QueueBudget() analytics.QueueBudgetSnapshot
				}); ok {
					queue := snapshotter.QueueBudget()
					values["kora.analytics.queue.items"] += int64(queue.QueuedItems)
					values["kora.analytics.queue.upper_bound_bytes"] += int64(queue.UpperBoundBytes)
					values["kora.analytics.queue.max_items"] += int64(queue.MaxQueuedItems)
					values["kora.analytics.queue.max_bytes"] += int64(queue.MaxQueuedBytes)
					if int64(queue.OldestAge) > values["kora.analytics.queue.oldest_age_upper_bound_ns"] {
						values["kora.analytics.queue.oldest_age_upper_bound_ns"] = int64(queue.OldestAge)
					}
				}
				if loaded.AnalyticsEventBus != nil {
					values["kora.analytics.events.dropped"] += loaded.AnalyticsEventBus.Dropped()
					if snapshotter, ok := loaded.AnalyticsEventBus.(interface {
						DropCauses() analytics.DropCauseSnapshot
					}); ok {
						drops := snapshotter.DropCauses()
						values["kora.analytics.events.payload_rejected"] += drops.PayloadRejected
						values["kora.analytics.events.wal_write_failures"] += drops.WALWriteFailures
						values["kora.analytics.listeners.overflows"] += drops.SubscriberOverflows
						values["kora.analytics.listeners.registration_rejected"] += drops.SubscriberRegistrationsRejected
					}
				}
				if snapshotter, ok := loaded.AnalyticsEventBus.(interface {
					WALBacklog() analytics.WALBacklogSnapshot
				}); ok {
					backlog := snapshotter.WALBacklog()
					if backlog.Pending {
						values["kora.analytics.wal.pending"] = 1
						values["kora.analytics.wal.bytes"] = addMetricBytes(values["kora.analytics.wal.bytes"], backlog.Bytes)
					}
					if !backlog.BytesKnown {
						values["kora.analytics.wal.bytes_unknown"] = 1
					}
					if !backlog.AgeKnown && backlog.Pending {
						values["kora.analytics.wal.age_unknown"] = 1
					}
					if backlog.OldestAge.Nanoseconds() > values["kora.analytics.wal.oldest_age_ns"] {
						values["kora.analytics.wal.oldest_age_ns"] = backlog.OldestAge.Nanoseconds()
					}
				}
				if snapshotter, ok := loaded.AnalyticsEventBus.(interface {
					ListenerQueueBudget() analytics.ListenerQueueSnapshot
				}); ok {
					queue := snapshotter.ListenerQueueBudget()
					values["kora.analytics.listeners.active"] += int64(queue.Listeners)
					values["kora.analytics.listeners.capacity"] += int64(queue.MaxListeners)
					values["kora.analytics.listeners.queued_items"] += int64(queue.QueuedItems)
					values["kora.analytics.listeners.queue_upper_bound_bytes"] += int64(queue.UpperBoundBytes)
					values["kora.analytics.listeners.queue_max_bytes"] += int64(queue.MaxQueuedBytes)
					if int64(queue.OldestAge) > values["kora.analytics.listeners.oldest_age_upper_bound_ns"] {
						values["kora.analytics.listeners.oldest_age_upper_bound_ns"] = int64(queue.OldestAge)
					}
				}
			}
		}
		if source.SessionCache != nil {
			snapshot := source.SessionCache.Snapshot()
			values["kora.session_cache.shared_bytes"], values["kora.session_cache.shared_max_bytes"] = int64(snapshot.Bytes), int64(snapshot.MaxBytes)
		}
		if source.Uploads != nil {
			snapshot := source.Uploads.Snapshot()
			values["kora.uploads.active"], values["kora.uploads.capacity"], values["kora.uploads.active_reserved_bytes"], values["kora.uploads.reserved_bytes_capacity"], values["kora.uploads.rejected"] = int64(snapshot.Active), int64(snapshot.Capacity), int64(snapshot.ActiveBytes), int64(snapshot.MaxBytes), int64(snapshot.Rejected)
		}
		if source.AnalyticsQueueAdmission != nil {
			snapshot := source.AnalyticsQueueAdmission.Snapshot()
			values["kora.analytics.process_queued_bytes"] = int64(snapshot.UsedBytes)
			values["kora.analytics.process_queue_max_bytes"] = int64(snapshot.MaxBytes)
			values["kora.analytics.process_queue_rejected"] = int64(snapshot.RejectedItems)
			values["kora.analytics.listeners.process_active"] = int64(snapshot.Listeners)
			values["kora.analytics.listeners.process_capacity"] = int64(snapshot.MaxListeners)
			values["kora.analytics.listeners.process_rejected"] = int64(snapshot.RejectedListeners)
		}
		if source.AnalyticsQueryCache != nil {
			snapshot := source.AnalyticsQueryCache.AnalyticsQueryCacheSnapshot()
			values["kora.analytics.query_cache.entries"] = int64(snapshot.Entries)
			values["kora.analytics.query_cache.max_entries"] = int64(snapshot.MaxEntries)
			values["kora.analytics.query_cache.bytes"] = int64(snapshot.Bytes)
			values["kora.analytics.query_cache.max_bytes"] = int64(snapshot.MaxBytes)
			values["kora.analytics.query_cache.evictions"] = int64(snapshot.Evictions)
			values["kora.analytics.query_cache.rejected"] = int64(snapshot.Rejected)
		}
		for index, instrument := range instruments {
			observer.ObserveInt64(instrument, values[names[index]])
		}
		return nil
	}
	registration, err := meter.RegisterCallback(callback, observables...)
	if err != nil {
		return nil, err
	}
	return &ResourceMetrics{registration: registration}, nil
}

func resourceMetricUnit(name string) string {
	switch {
	case strings.HasSuffix(name, "_ns"):
		return "ns"
	case strings.Contains(name, "bytes"):
		return "By"
	case strings.Contains(name, "wait_total") || strings.Contains(name, "waiters"):
		return "{wait}"
	case strings.Contains(name, "operations.total"):
		return "{operation}"
	case strings.Contains(name, "lock_contention"):
		return "{error}"
	case strings.Contains(name, "connections.") || strings.Contains(name, ".pools."):
		return "{connection}"
	case strings.Contains(name, "goroutines"):
		return "{goroutine}"
	case strings.Contains(name, "active"):
		return "{item}"
	default:
		return "{item}"
	}
}

func addMetricBytes(current, addition int64) int64 {
	if addition <= 0 {
		return current
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if addition > maxInt64-current {
		return maxInt64
	}
	return current + addition
}

func (m *ResourceMetrics) Unregister() error {
	if m == nil {
		return nil
	}
	m.once.Do(func() { m.err = m.registration.Unregister() })
	return m.err
}

func int64Pair(a, b int) (int64, int64) { return int64(a), int64(b) }
