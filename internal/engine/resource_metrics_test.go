package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/scheduler"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type analyticsCacheSnapshotStub struct {
	snapshot api.AnalyticsQueryCacheSnapshot
}

func (s analyticsCacheSnapshotStub) AnalyticsQueryCacheSnapshot() api.AnalyticsQueryCacheSnapshot {
	return s.snapshot
}

func TestResourceMetricsRegisterAndObserveProcessResourceGauges(t *testing.T) {
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	bus := analytics.NewChannelBusWithLimits(2, 1024, 2048, t.TempDir())
	defer bus.Close()
	fanout, err := analytics.NewMultiBus(bus)
	if err != nil {
		t.Fatal("create analytics listener fanout:", err)
	}
	defer fanout.Close()
	listener := fanout.NewListenerQueue(false)
	if err := fanout.TryAddListener(listener); err != nil {
		t.Fatal("register analytics listener:", err)
	}
	if err := bus.Publish(analytics.ChangeEvent{ID: "metric-event", Site: "tenant", Doctype: "Thing", DocName: "1", Operation: analytics.EventInsert}); err != nil {
		t.Fatal("publish analytics metric fixture:", err)
	}
	if err := bus.Publish(analytics.ChangeEvent{ID: "metric-oversized", Site: "tenant", Doctype: "Thing", DocName: "2", Operation: analytics.EventInsert, Data: map[string]any{"payload": strings.Repeat("x", 2048)}}); err == nil {
		t.Fatal("oversized analytics fixture was accepted")
	}
	deadline := time.Now().Add(time.Second)
	for len(listener) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(listener) != 1 {
		t.Fatal("analytics listener did not receive queued metric event")
	}
	schedulerWorker := scheduler.New(nil, nil, nil, nil)
	tenantDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create tenant pool metric fixture:", err)
	}
	defer tenantDB.Close()
	tenantDB.SetMaxOpenConns(3)
	loaded := &net.LoadedSite{SiteID: "metrics-tenant", Name: "metrics-tenant", DB: tenantDB, AnalyticsEventBus: fanout}
	services := loaded.RuntimeServiceSnapshot()
	cloudRelay := analytics.NewCloudRelay(nil, loaded.Name, analytics.CloudRelayConfig{AccountID: "metrics-account"})
	cloudEvent := analytics.CloudProductEventDTO{Kind: "first_login", Properties: analytics.FirstLoginPropertiesDTO{User: "user@example.invalid"}}
	for i := 0; i < 32; i++ {
		if !cloudRelay.EnqueueProductEvent(cloudEvent, "") {
			t.Fatalf("enqueue Cloud telemetry metric fixture %d", i)
		}
	}
	if cloudRelay.EnqueueProductEvent(cloudEvent, "") {
		t.Fatal("Cloud telemetry metric fixture exceeded bounded queue")
	}
	services.AnalyticsRelay = cloudRelay
	services.Scheduler = schedulerWorker
	services.AnalyticsRebuild = analytics.NewRebuildRunner(tenantDB, db.Resolve("mysql"), loaded.Name, nil, nil)
	backlog := &outbox.BacklogMetrics{}
	backlog.RecordLeaseLost()
	// Outbox metrics are sampled through their own publisher DB; the OTel
	// callback below must read only the cached snapshot.
	backlogDB, backlogMock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create outbox backlog DB fixture:", err)
	}
	defer backlogDB.Close()
	backlogMock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("pending", outbox.BacklogSampleLimit).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now().Add(-12 * time.Second)))
	backlogMock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("failed", outbox.BacklogSampleLimit).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	backlogMock.ExpectQuery(`SELECT created_at FROM _kora_outbox WHERE status = \? ORDER BY created_at ASC LIMIT \?`).
		WithArgs("publishing", outbox.BacklogSampleLimit).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}))
	if err := backlog.Refresh(context.Background(), &outbox.Publisher{DB: backlogDB, Dialect: db.Resolve("mysql")}); err != nil {
		t.Fatalf("refresh outbox backlog fixture: %v", err)
	}
	if err := backlogMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("outbox backlog SQL sample: %v", err)
	}
	services.OutboxBacklog = backlog
	loaded.RuntimeServices = &services
	router, err := net.NewSiteRouterWithRetirementLimit([]*net.LoadedSite{loaded}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	realtime, err := NewRealtimeStreamAdmission(3)
	if err != nil {
		t.Fatal("create realtime admission:", err)
	}
	release, err := realtime.Acquire(context.Background())
	if err != nil {
		t.Fatal("acquire realtime stream:", err)
	}
	defer release()
	sessionBudget, err := auth.NewSessionCacheBudget(4096)
	if err != nil {
		t.Fatal("create session cache budget:", err)
	}
	uploads, err := NewUploadAdmission(2, 4096)
	if err != nil {
		t.Fatal("create upload admission:", err)
	}
	releaseUpload, err := uploads.Acquire(context.Background(), 1024)
	if err != nil {
		t.Fatal("reserve upload budget:", err)
	}
	defer releaseUpload()
	listenerQueueAdmission, err := analytics.NewAnalyticsQueueAdmissionWithLimits(2048, 2)
	if err != nil {
		t.Fatal("create listener queue admission:", err)
	}
	if !listenerQueueAdmission.TryAcquire(512) || listenerQueueAdmission.TryAcquire(2048) {
		t.Fatal("listener queue admission did not enforce its process-wide byte limit")
	}
	if !listenerQueueAdmission.TryAcquireListener() || !listenerQueueAdmission.TryAcquireListener() || listenerQueueAdmission.TryAcquireListener() {
		t.Fatal("analytics listener admission did not enforce its process-wide count limit")
	}
	durableScan := &DurableWorkScanner{}
	durableScan.probes.Store(7)
	durableScan.due.Store(2)
	durableScan.probeFails.Store(1)
	durableScan.scanFails.Store(2)
	durableScan.wakeFails.Store(1)
	durableScan.capacity.Store(1)
	durableScan.sweeps.Store(3)
	durableScan.lastSweep = time.Now().Add(-2 * time.Second)
	durableScan.beginColdWALScan("")
	durableScan.recordColdWALBacklog(analytics.WALBacklogSnapshot{Pending: true, AgeKnown: true, OldestAge: 3 * time.Second, Bytes: 2048, BytesKnown: true})
	durableScan.recordColdWALBacklog(analytics.WALBacklogSnapshot{Pending: true, AgeKnown: false, BytesKnown: true})
	durableScan.publishColdWALSnapshot()
	platformDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("create platform pool metric fixture:", err)
	}
	defer platformDB.Close()
	platformDB.SetMaxOpenConns(7)
	deletionWorker := &PendingDeletionWorker{}
	deletionWorker.attempts.Store(4)
	deletionWorker.failures.Store(2)
	deletionWorker.rejected.Store(3)
	metrics, err := registerResourceMetrics(provider.Meter("resource-metrics-test"), router, nil, nil, ResourceMetricSources{PlatformDB: platformDB, RealtimeStreams: realtime, SessionCache: sessionBudget, Uploads: uploads, AnalyticsQueueAdmission: listenerQueueAdmission, DurableWorkScan: durableScan, DeletionWorker: deletionWorker, AnalyticsQueryCache: analyticsCacheSnapshotStub{snapshot: api.AnalyticsQueryCacheSnapshot{Entries: 2, MaxEntries: 256, Bytes: 4096, MaxBytes: 32 << 20, Evictions: 3, Rejected: 4}}})
	if err != nil {
		t.Fatal("register resource metrics:", err)
	}
	var result metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &result); err != nil {
		t.Fatal("collect resource metrics:", err)
	}
	values := make(map[string]int64)
	names := make(map[string]bool)
	for _, scope := range result.ScopeMetrics {
		for _, item := range scope.Metrics {
			names[item.Name] = true
			gauge, ok := item.Data.(metricdata.Gauge[int64])
			if ok && len(gauge.DataPoints) > 0 {
				values[item.Name] = gauge.DataPoints[0].Value
				continue
			}
			if sum, ok := item.Data.(metricdata.Sum[int64]); ok && len(sum.DataPoints) > 0 {
				values[item.Name] = sum.DataPoints[0].Value
			}
		}
	}
	if values["kora.tenant.runtimes.active"] != 1 || values["kora.tenant.runtimes.retiring"] != 0 || values["kora.tenant.runtimes.capacity"] != 2 {
		t.Fatalf("tenant resource gauges = %v", values)
	}
	if values["kora.process.goroutines"] <= 0 {
		t.Fatalf("goroutine gauge missing or invalid: %v", values)
	}
	if values["kora.platform.deletion.cleanup_attempts"] != 4 || values["kora.platform.deletion.cleanup_failures"] != 2 || values["kora.platform.deletion.admission_rejections"] != 3 {
		t.Fatalf("pending deletion worker counters = %v", values)
	}
	if values["kora.cloud.product_events.queue_items"] != 32 || values["kora.cloud.product_events.queue_capacity"] != 32 || values["kora.cloud.product_events.dropped"] != 1 {
		t.Fatalf("Cloud product event queue metrics = %v", values)
	}
	for _, name := range []string{"kora.analytics.rebuild.active", "kora.analytics.rebuild.claims", "kora.analytics.rebuild.claim_failures", "kora.analytics.rebuild.admission_deferrals", "kora.analytics.rebuild.execution_failures", "kora.analytics.rebuild.lease_renew_failures", "kora.analytics.rebuild.lease_lost", "kora.analytics.rebuild.completion_failures"} {
		if !names[name] || values[name] != 0 {
			t.Fatalf("analytics rebuild metric %q is missing or nonzero without worker activity: names=%v values=%v", name, names, values)
		}
	}
	if values["kora.outbox.pending_sampled"] != 1 || values["kora.outbox.failed_sampled"] != 0 || values["kora.outbox.publishing_sampled"] != 0 || values["kora.outbox.sample_saturated"] != 0 || values["kora.outbox.oldest_unpublished_age_ns"] < int64(12*time.Second) || values["kora.outbox.lease_lost"] != 1 {
		t.Fatalf("cached outbox backlog metrics = %v", values)
	}
	if values["kora.scheduler.durable.pending_occurrences"] != 0 || values["kora.scheduler.durable.oldest_pending_age_ns"] != 0 || values["kora.scheduler.durable.backlog_sample_failures"] != 0 || values["kora.scheduler.durable.lease_lost"] != 0 {
		t.Fatalf("cached scheduler backlog metrics = %v", values)
	}
	if values["kora.db.pools.max_open"] != 10 || values["kora.db.pools.open"] != 2 || values["kora.db.pools.in_use"] != 0 || values["kora.db.pools.idle"] != 2 {
		t.Fatalf("database/sql pool gauges = %v", values)
	}
	if processRSSBytes() > 0 && values["kora.process.rss_bytes"] <= 0 {
		t.Fatalf("resident-set gauge missing or invalid: %v", values)
	}
	if values["kora.realtime.streams.active"] != 1 || values["kora.realtime.streams.capacity"] != 3 || values["kora.realtime.streams.rejected"] != 0 {
		t.Fatalf("realtime stream gauges = %v", values)
	}
	if values["kora.analytics.queue.items"] != 0 || values["kora.analytics.queue.upper_bound_bytes"] != 0 || values["kora.analytics.queue.max_items"] != 2 || values["kora.analytics.queue.max_bytes"] != 2048 || values["kora.analytics.queue.oldest_age_upper_bound_ns"] != 0 {
		t.Fatalf("analytics queue metrics = %v", values)
	}
	if values["kora.analytics.listeners.active"] != 1 || values["kora.analytics.listeners.capacity"] != analytics.DefaultMaxListeners || values["kora.analytics.listeners.queued_items"] != 1 || values["kora.analytics.listeners.queue_upper_bound_bytes"] != 1024 || values["kora.analytics.listeners.queue_max_bytes"] != analytics.MaxListenerQueueSize*1024 || values["kora.analytics.listeners.oldest_age_upper_bound_ns"] <= 0 {
		t.Fatalf("analytics listener queue metrics = %v", values)
	}
	if values["kora.analytics.events.dropped"] != 1 {
		t.Fatalf("analytics rejected/dropped event gauge = %v, want 1", values)
	}
	if values["kora.analytics.events.payload_rejected"] != 1 || values["kora.analytics.events.wal_write_failures"] != 0 || values["kora.analytics.listeners.overflows"] != 0 || values["kora.analytics.listeners.registration_rejected"] != 0 {
		t.Fatalf("analytics drop-cause counters = %v", values)
	}
	if values["kora.session_cache.shared_bytes"] != 0 || values["kora.session_cache.shared_max_bytes"] != 4096 {
		t.Fatalf("shared session cache metrics = %v", values)
	}
	if values["kora.uploads.active"] != 1 || values["kora.uploads.capacity"] != 2 || values["kora.uploads.active_reserved_bytes"] != 1024 || values["kora.uploads.reserved_bytes_capacity"] != 4096 {
		t.Fatalf("upload admission metrics = %v", values)
	}
	if values["kora.analytics.process_queued_bytes"] != 512 || values["kora.analytics.process_queue_max_bytes"] != 2048 || values["kora.analytics.process_queue_rejected"] != 1 {
		t.Fatalf("process analytics listener queue metrics = %v", values)
	}
	if values["kora.analytics.listeners.process_active"] != 2 || values["kora.analytics.listeners.process_capacity"] != 2 || values["kora.analytics.listeners.process_rejected"] != 1 {
		t.Fatalf("process analytics listener count metrics = %v", values)
	}
	if values["kora.analytics.query_cache.entries"] != 2 || values["kora.analytics.query_cache.max_entries"] != 256 || values["kora.analytics.query_cache.bytes"] != 4096 || values["kora.analytics.query_cache.max_bytes"] != 32<<20 || values["kora.analytics.query_cache.evictions"] != 3 || values["kora.analytics.query_cache.rejected"] != 4 {
		t.Fatalf("analytics query cache metrics = %v", values)
	}
	if values["kora.tenant.durable_scan.probes"] != 7 || values["kora.tenant.durable_scan.due_tenants"] != 2 || values["kora.tenant.durable_scan.probe_failures"] != 1 || values["kora.tenant.durable_scan.scan_failures"] != 2 || values["kora.tenant.durable_scan.wake_failures"] != 1 || values["kora.tenant.durable_scan.capacity_rejections"] != 1 || values["kora.tenant.durable_scan.completed_sweeps"] != 3 || values["kora.tenant.durable_scan.last_sweep_age_ns"] <= 0 {
		t.Fatalf("durable work scan metrics = %v", values)
	}
	if values["kora.analytics.wal.pending"] != 1 || values["kora.analytics.wal.age_unknown"] != 1 || values["kora.analytics.wal.oldest_age_ns"] < int64(3*time.Second) {
		t.Fatalf("durable analytics WAL metrics = %v", values)
	}
	if values["kora.analytics.wal.bytes"] != 0 || values["kora.analytics.wal.bytes_unknown"] != 0 || values["kora.analytics.wal.cold_bytes"] != 2048 || values["kora.analytics.wal.cold_bytes_unknown"] != 0 {
		t.Fatalf("durable analytics WAL byte metrics = %v", values)
	}
	for _, name := range []string{"kora.db.connections.wait_total", "kora.db.connections.rejected", "kora.db.pools.open", "kora.db.pools.in_use", "kora.db.pools.idle", "kora.db.pools.max_open", "kora.db.pools.wait_total", "kora.db.pools.wait_duration_ns", "kora.db.operations.total", "kora.db.operations.duration_ns", "kora.db.lock_contention.total", "kora.db.work.wait_duration_ns", "kora.db.work.maintenance_active", "kora.db.work.maintenance_capacity", "kora.async_hooks.queue.rejected_items", "kora.tenant.runtimes.active", "kora.tenant.runtimes.capacity", "kora.tenant.activation.waiters", "kora.tenant.activation.max_waiters", "kora.tenant.activation.wait_total", "kora.tenant.activation.wait_duration_ns", "kora.tenant.activation.rejected", "kora.process.rss_bytes", "kora.analytics.events.dropped", "kora.analytics.events.payload_rejected", "kora.analytics.events.wal_write_failures", "kora.analytics.listeners.overflows", "kora.analytics.listeners.registration_rejected", "kora.analytics.listeners.active", "kora.analytics.listeners.capacity", "kora.analytics.listeners.queued_items", "kora.analytics.listeners.queue_upper_bound_bytes", "kora.analytics.listeners.queue_max_bytes", "kora.analytics.listeners.oldest_age_upper_bound_ns", "kora.analytics.process_queued_bytes", "kora.analytics.process_queue_max_bytes", "kora.analytics.process_queue_rejected", "kora.analytics.listeners.process_active", "kora.analytics.listeners.process_capacity", "kora.analytics.listeners.process_rejected", "kora.analytics.query_cache.entries", "kora.analytics.query_cache.max_entries", "kora.analytics.query_cache.bytes", "kora.analytics.query_cache.max_bytes", "kora.analytics.query_cache.evictions", "kora.analytics.query_cache.rejected", "kora.session_cache.bytes", "kora.session_cache.max_bytes", "kora.realtime.streams.active", "kora.realtime.streams.capacity", "kora.realtime.streams.rejected", "kora.realtime.nats.subscriptions.active", "kora.realtime.nats.client_pending_messages", "kora.realtime.nats.client_pending_bytes", "kora.realtime.nats.client_pending_nonempty_observed_span_ns", "kora.realtime.nats.application_pending_items", "kora.realtime.nats.application_pending_bytes", "kora.realtime.nats.application_capacity_items", "kora.realtime.nats.application_capacity_bytes", "kora.realtime.nats.application_oldest_age_upper_bound_ns", "kora.realtime.nats.client_dropped_messages", "kora.analytics.queue.items", "kora.analytics.queue.upper_bound_bytes", "kora.analytics.queue.max_items", "kora.analytics.queue.max_bytes", "kora.analytics.queue.oldest_age_upper_bound_ns", "kora.session_cache.shared_bytes", "kora.session_cache.shared_max_bytes", "kora.uploads.active", "kora.uploads.capacity", "kora.uploads.active_reserved_bytes", "kora.uploads.reserved_bytes_capacity", "kora.uploads.rejected", "kora.scheduler.durable.active", "kora.scheduler.durable.claims", "kora.scheduler.durable.claim_failures", "kora.scheduler.durable.lease_renew_failures", "kora.scheduler.durable.uncertain_outcomes", "kora.scheduler.durable.completion_failures", "kora.outbox.pending_sampled", "kora.outbox.failed_sampled", "kora.outbox.publishing_sampled", "kora.outbox.sample_saturated", "kora.outbox.oldest_unpublished_age_ns", "kora.outbox.metrics_refresh_failures", "kora.outbox.lease_lost"} {
		if !names[name] {
			t.Errorf("resource metric %s was not exported", name)
		}
	}
	for _, name := range []string{"kora.scheduler.durable.pending_occurrences", "kora.scheduler.durable.oldest_pending_age_ns", "kora.scheduler.durable.backlog_sample_failures", "kora.scheduler.durable.lease_lost"} {
		if !names[name] {
			t.Errorf("resource metric %s was not exported", name)
		}
	}
	for _, name := range []string{"kora.tenant.durable_scan.probes", "kora.tenant.durable_scan.due_tenants", "kora.tenant.durable_scan.probe_failures", "kora.tenant.durable_scan.scan_failures", "kora.tenant.durable_scan.wake_failures", "kora.tenant.durable_scan.capacity_rejections", "kora.tenant.durable_scan.completed_sweeps", "kora.tenant.durable_scan.last_sweep_age_ns", "kora.analytics.wal.pending", "kora.analytics.wal.oldest_age_ns", "kora.analytics.wal.age_unknown", "kora.analytics.wal.bytes", "kora.analytics.wal.bytes_unknown", "kora.analytics.wal.cold_bytes", "kora.analytics.wal.cold_bytes_unknown"} {
		if !names[name] {
			t.Errorf("resource metric %s was not exported", name)
		}
	}
	for _, name := range []string{"kora.cloud.product_events.queue_items", "kora.cloud.product_events.queue_capacity", "kora.cloud.product_events.dropped"} {
		if !names[name] {
			t.Errorf("resource metric %s was not exported", name)
		}
	}
	telemetryShutdown := false
	resources := &ProcessResources{ResourceMetrics: metrics, TelemetryShutdown: func(context.Context) error {
		telemetryShutdown = true
		return nil
	}}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatal("shutdown process resources:", err)
	}
	if !telemetryShutdown {
		t.Fatal("telemetry provider did not shut down after resource metrics were unregistered")
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal("shutdown metric provider:", err)
	}
}
