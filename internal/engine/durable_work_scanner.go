package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/scheduler"
	schedulersql "github.com/asenawritescode/kora/internal/scheduler/sqlstore"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/tenantruntime"
)

type durableScanDirectory interface {
	GetDirectoryDescriptorPageContext(context.Context, string, string, uint16) ([]site.Descriptor, error)
	GetByIDContext(context.Context, string) (site.DBSiteInfo, error)
}

type DurableWorkScannerConfig struct {
	ParentContext   context.Context
	PlatformDB      *sql.DB
	PlatformDialect db.Dialect
	Directory       durableScanDirectory
	Tenants         *net.SiteRouter
	Coordinator     *TenantRuntimeCoordinator
	WorkAdmission   interface {
		Acquire(context.Context) (func(), error)
	}
	TenantWorkAdmission func(*site.Config) (interface {
		Acquire(context.Context) (func(), error)
	}, error)
	Common              *site.CommonConfig
	Analytics           *analytics.Config
	CellID              string
	DirectoryConsumerID string
	PageSize            uint16
	ProbeTimeout        time.Duration
	PageYield           time.Duration
	Interval            time.Duration
}

// DurableWorkScanner walks local credential-free descriptors in bounded
// pages, briefly opens at most one cold tenant database at a time, and wakes
// the tenant runtime that owns pending work. Durable SQL state remains the
// recovery authority; scanner notifications are never the work queue.
type DurableWorkScanner struct {
	parent        context.Context
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	startMu       sync.Mutex
	started       bool
	store         *durableScanStateStore
	directory     durableScanDirectory
	tenants       *net.SiteRouter
	coordinator   *TenantRuntimeCoordinator
	workAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	tenantWorkAdmission func(*site.Config) (interface {
		Acquire(context.Context) (func(), error)
	}, error)
	common                 *site.CommonConfig
	analytics              *analytics.Config
	cellID                 string
	scanKey                string
	pageSize               uint16
	probeWait              time.Duration
	pageYield              time.Duration
	interval               time.Duration
	probes                 atomic.Uint64
	due                    atomic.Uint64
	probeFails             atomic.Uint64
	scanFails              atomic.Uint64
	wakeFails              atomic.Uint64
	capacity               atomic.Uint64
	sweeps                 atomic.Uint64
	lastMu                 sync.RWMutex
	lastSweep              time.Time
	walMu                  sync.Mutex
	walPending             bool
	walUnknown             bool
	walOldest              time.Duration
	walObservedAt          time.Time
	walBytes               int64
	walBytesKnown          bool
	walScanActive          bool
	walPublished           bool
	walPublishedPending    bool
	walPublishedUnknown    bool
	walPublishedOldest     time.Duration
	walPublishedAt         time.Time
	walPublishedBytes      int64
	walPublishedBytesKnown bool
}

type DurableWorkScannerSnapshot struct {
	Probes                uint64
	DueTenants            uint64
	ProbeFailures         uint64
	ScanFailures          uint64
	WakeFailures          uint64
	CapacityRejections    uint64
	CompletedSweeps       uint64
	LastCompletedSweep    time.Time
	LastCompletedSweepAge time.Duration
	ColdWALPending        bool
	ColdWALAgeUnknown     bool
	ColdWALOldestAge      time.Duration
	ColdWALBytes          int64
	ColdWALBytesKnown     bool
}

func NewDurableWorkScanner(config DurableWorkScannerConfig) (*DurableWorkScanner, error) {
	if config.ParentContext == nil || config.PlatformDB == nil || config.PlatformDialect == nil || config.Directory == nil || config.Tenants == nil || config.Coordinator == nil || config.Common == nil || strings.TrimSpace(config.DirectoryConsumerID) == "" {
		return nil, errors.New("durable work scanner requires process context, platform database, directory, tenant manager, coordinator and stable scan identity")
	}
	store, err := newDurableScanStateStore(config.PlatformDB, config.PlatformDialect)
	if err != nil {
		return nil, err
	}
	if config.PageSize == 0 {
		config.PageSize = 64
	}
	if config.PageSize > 500 {
		return nil, errors.New("durable work scanner page size cannot exceed 500")
	}
	if config.ProbeTimeout <= 0 {
		config.ProbeTimeout = 5 * time.Second
	}
	if config.PageYield <= 0 {
		config.PageYield = 25 * time.Millisecond
	}
	if config.Interval <= 0 {
		config.Interval = 15 * time.Second
	}
	cell := strings.TrimSpace(config.CellID)
	if cell == "" {
		cell = "default"
	}
	scanKey := strings.TrimSpace(config.DirectoryConsumerID) + ":durable-work:" + cell
	return &DurableWorkScanner{
		parent: config.ParentContext,
		store:  store, directory: config.Directory, tenants: config.Tenants, coordinator: config.Coordinator,
		workAdmission:       config.WorkAdmission,
		tenantWorkAdmission: config.TenantWorkAdmission,
		common:              config.Common, analytics: config.Analytics, cellID: strings.TrimSpace(config.CellID), scanKey: scanKey,
		pageSize: config.PageSize, probeWait: config.ProbeTimeout, pageYield: config.PageYield, interval: config.Interval,
		done: make(chan struct{}),
	}, nil
}

// Start starts one joined process-owned scanner. Schema and cursor readiness
// are checked before the goroutine is published.
func (s *DurableWorkScanner) Start() error {
	if s == nil {
		return errors.New("durable work scanner is required")
	}
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return nil
	}
	setupCtx, setupCancel := context.WithTimeout(s.parent, s.probeWait)
	defer setupCancel()
	if err := s.withWork(setupCtx, func() error { return s.store.Ensure(setupCtx) }); err != nil {
		return err
	}
	s.ctx, s.cancel = context.WithCancel(s.parent)
	s.started = true
	go s.run()
	return nil
}

func (s *DurableWorkScanner) run() {
	defer close(s.done)
	for {
		if err := s.ctx.Err(); err != nil {
			return
		}
		for {
			completed, err := s.scanPage(s.ctx)
			if err != nil {
				s.scanFails.Add(1)
				if !errors.Is(err, context.Canceled) {
					slog.Warn("durable tenant work scan failed", "scan_key", s.scanKey, "error_type", fmt.Sprintf("%T", err))
				}
				break
			}
			if completed {
				break
			}
			if !waitContext(s.ctx, s.pageYield) {
				return
			}
		}
		if !waitContext(s.ctx, s.interval) {
			return
		}
	}
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// ScanOnce processes one bounded directory page. A full page advances the
// durable keyset cursor; the final/empty page closes the sweep and resets it.
func (s *DurableWorkScanner) ScanOnce(ctx context.Context) error {
	_, err := s.scanPage(ctx)
	return err
}

func (s *DurableWorkScanner) scanPage(ctx context.Context) (bool, error) {
	if s == nil || ctx == nil {
		return false, errors.New("durable work scan requires scanner and context")
	}
	var cursor string
	var lastCompletedAt time.Time
	err := s.withWork(ctx, func() error {
		var loadErr error
		cursor, lastCompletedAt, loadErr = s.store.Load(ctx, s.scanKey)
		return loadErr
	})
	if err != nil {
		return false, err
	}
	if !lastCompletedAt.IsZero() {
		s.lastMu.Lock()
		if lastCompletedAt.After(s.lastSweep) {
			s.lastSweep = lastCompletedAt
		}
		s.lastMu.Unlock()
	}
	s.beginColdWALScan(cursor)
	var page []site.Descriptor
	err = s.withWork(ctx, func() error {
		var pageErr error
		page, pageErr = s.directory.GetDirectoryDescriptorPageContext(ctx, s.cellID, cursor, s.pageSize)
		return pageErr
	})
	if err != nil {
		return false, fmt.Errorf("read tenant scan page: %w", err)
	}
	if len(page) > int(s.pageSize) {
		return false, fmt.Errorf("tenant directory returned %d descriptors for page limit %d", len(page), s.pageSize)
	}
	for _, descriptor := range page {
		if err := ctx.Err(); err != nil {
			s.markColdWALBytesUnknown()
			return false, err
		}
		if !activeScanDescriptor(descriptor, s.cellID) {
			continue
		}
		if s.tenants.SnapshotByID(descriptor.SiteID) != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, s.probeWait)
		due, probeErr := s.probeTenant(probeCtx, descriptor)
		cancel()
		s.probes.Add(1)
		if probeErr != nil {
			s.markColdWALBytesUnknown()
			s.probeFails.Add(1)
			// A future full sweep retries this tenant; do not let one unavailable
			// tenant pin the cursor and starve the rest of the assigned directory.
			continue
		}
		if !due {
			continue
		}
		s.due.Add(1)
		wakeCtx, cancelWake := context.WithTimeout(ctx, s.probeWait)
		wakeErr := s.coordinator.ActivateFromDirectory(wakeCtx, descriptor.SiteID)
		cancelWake()
		if errors.Is(wakeErr, tenantruntime.ErrResidentCapacity) {
			s.capacity.Add(1)
			continue
		}
		if wakeErr != nil && !errors.Is(wakeErr, context.Canceled) {
			s.wakeFails.Add(1)
		}
	}
	next := ""
	completed := len(page) < int(s.pageSize)
	if !completed {
		next = page[len(page)-1].SiteID
	}
	if err := s.withWork(ctx, func() error { return s.store.Advance(ctx, s.scanKey, cursor, next, completed) }); err != nil {
		if len(page) > 0 {
			s.markColdWALBytesUnknown()
		}
		return false, err
	}
	if completed {
		s.lastMu.Lock()
		s.lastSweep = time.Now().UTC()
		s.lastMu.Unlock()
		s.publishColdWALSnapshot()
		s.sweeps.Add(1)
	}
	return completed, nil
}

func (s *DurableWorkScanner) withWork(ctx context.Context, work func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.workAdmission == nil {
		return work()
	}
	release, err := s.workAdmission.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

func activeScanDescriptor(descriptor site.Descriptor, cellID string) bool {
	if descriptor.SiteID == "" || (descriptor.Status != "active" && descriptor.Status != "degraded") {
		return false
	}
	return site.BelongsToDeployment(descriptor.RuntimeCellID, cellID)
}

func (s *DurableWorkScanner) probeTenant(ctx context.Context, descriptor site.Descriptor) (bool, error) {
	var info site.DBSiteInfo
	err := s.withWork(ctx, func() error {
		var lookupErr error
		info, lookupErr = s.directory.GetByIDContext(ctx, descriptor.SiteID)
		return lookupErr
	})
	if err != nil {
		return false, err
	}
	// The page descriptor may be stale by the time this cold probe runs.
	// Recheck the current assignment fetched with the tenant coordinates before
	// examining WAL or opening its tenant database.
	if !site.BelongsToDeployment(info.RuntimeCellID, s.cellID) {
		return false, nil
	}
	if info.Status != "active" && info.Status != "degraded" {
		return false, nil
	}
	config := site.ReconstructSiteConfigFromDBInfo(info, s.common)
	if s.analytics != nil && strings.TrimSpace(s.analytics.WALDir) != "" && strings.TrimSpace(info.SiteID) != "" {
		backlog, err := analytics.InspectWALBacklog(AnalyticsTenantWALDir(s.analytics.WALDir, info.SiteID))
		if err != nil {
			slog.Warn("analytics WAL probe failed", "tenant_id", descriptor.SiteID, "error", err)
		}
		s.recordColdWALBacklog(backlog)
		if backlog.Pending {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	var admission interface {
		Acquire(context.Context) (func(), error)
	}
	if s.tenantWorkAdmission != nil {
		admission, err = s.tenantWorkAdmission(config)
	} else {
		admission, err = site.ConnectionAdmission(config)
	}
	if err != nil {
		return false, err
	}
	var due bool
	due, err = withTenantProbeAdmission(ctx, admission, func() (bool, error) {
		database, err := site.ConnectContext(ctx, config)
		if err != nil {
			return false, err
		}
		defer database.Close()
		dialect := db.Resolve(config.DBType)
		activeConfig, _, err := configstore.NewStore(database, dialect).LoadActiveSnapshotContext(ctx, info.Name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("load active scheduled job definitions: %w", err)
		}
		if activeConfig != nil && len(activeConfig.ScheduledJobs) > 0 {
			jobStore, err := schedulersql.New(database, dialect)
			if err != nil {
				return false, err
			}
			if err := jobStore.Ensure(ctx); err != nil {
				return false, err
			}
			now := time.Now().UTC()
			for _, definition := range activeConfig.ScheduledJobs {
				var jobConfig map[string]any
				if len(definition.Config) > 0 {
					if err := json.Unmarshal(definition.Config, &jobConfig); err != nil {
						return false, fmt.Errorf("decode scheduled job %q config: %w", definition.Name, err)
					}
				}
				job := &scheduler.JobConfig{Name: definition.Name, OwnerModuleID: definition.OwnerModuleID, Type: scheduler.JobType(definition.Type), Schedule: definition.Schedule, Config: jobConfig}
				if err := jobStore.RegisterDefinition(ctx, info.SiteID, job, scheduler.JobDefinitionDigest(job), scheduler.NextRunAfter(job.Schedule, now), now); err != nil {
					return false, err
				}
			}
		}
		return hasDueTenantWork(ctx, database, dialect)
	})
	if err != nil || due {
		return due, err
	}
	if s.analytics != nil && strings.TrimSpace(s.analytics.WALDir) != "" && strings.TrimSpace(info.SiteID) != "" {
		backlog, walErr := analytics.InspectWALBacklog(AnalyticsTenantWALDir(s.analytics.WALDir, info.SiteID))
		if walErr != nil {
			if backlog.Pending {
				s.recordColdWALBacklog(backlog)
				return true, nil
			}
			return false, fmt.Errorf("probe tenant analytics WAL: %w", walErr)
		}
		s.recordColdWALBacklog(backlog)
		return backlog.Pending, nil
	}
	return false, nil
}

func withTenantProbeAdmission(ctx context.Context, admission interface {
	Acquire(context.Context) (func(), error)
}, work func() (bool, error)) (bool, error) {
	if admission == nil || work == nil {
		return false, errors.New("cold tenant probe requires admission and work")
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("admit cold tenant durable-work probe: %w", err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return work()
}

func (s *DurableWorkScanner) recordColdWALBacklog(backlog analytics.WALBacklogSnapshot) {
	if s == nil {
		return
	}
	s.walMu.Lock()
	defer s.walMu.Unlock()
	if !backlog.BytesKnown || !s.walBytesKnown {
		s.walBytesKnown = false
	} else {
		const maxInt64 = int64(^uint64(0) >> 1)
		if backlog.Bytes < 0 || backlog.Bytes > maxInt64-s.walBytes {
			s.walBytesKnown = false
		} else {
			s.walBytes += backlog.Bytes
		}
	}
	if !backlog.Pending {
		return
	}
	s.walPending = true
	if !backlog.AgeKnown {
		s.walUnknown = true
	} else {
		currentAge := s.walOldest
		if !s.walObservedAt.IsZero() {
			currentAge += time.Since(s.walObservedAt)
		}
		if s.walObservedAt.IsZero() || backlog.OldestAge > currentAge {
			s.walOldest = backlog.OldestAge
			s.walObservedAt = time.Now().UTC()
		}
	}
}

func (s *DurableWorkScanner) beginColdWALScan(cursor string) {
	if s == nil {
		return
	}
	s.walMu.Lock()
	defer s.walMu.Unlock()
	if cursor == "" {
		s.walPending, s.walUnknown, s.walOldest, s.walObservedAt = false, false, 0, time.Time{}
		s.walBytes, s.walBytesKnown, s.walScanActive = 0, true, true
		return
	}
	if !s.walScanActive {
		// The persisted cursor outlived the process, but the in-memory sum for
		// earlier pages did not. Keep this partial-sweep sample explicitly unknown.
		s.walBytes, s.walBytesKnown, s.walScanActive = 0, false, true
	}
}

func (s *DurableWorkScanner) markColdWALBytesUnknown() {
	if s == nil {
		return
	}
	s.walMu.Lock()
	s.walBytesKnown = false
	s.walMu.Unlock()
}

func (s *DurableWorkScanner) publishColdWALSnapshot() {
	if s == nil {
		return
	}
	s.walMu.Lock()
	defer s.walMu.Unlock()
	s.walPublished = true
	s.walPublishedPending = s.walPending
	s.walPublishedUnknown = s.walUnknown
	s.walPublishedOldest = s.walOldest
	if s.walPending && !s.walObservedAt.IsZero() {
		s.walPublishedOldest += time.Since(s.walObservedAt)
	}
	s.walPublishedAt = time.Now().UTC()
	s.walPublishedBytes = s.walBytes
	s.walPublishedBytesKnown = s.walBytesKnown
	s.walScanActive = false
}

func hasDueTenantWork(ctx context.Context, database *sql.DB, dialect db.Dialect) (bool, error) {
	now := dialect.NowTimestamp()
	checks := []struct {
		tables []string
		query  string
	}{
		{[]string{"_kora_schedule_definition"}, `SELECT 1 FROM _kora_schedule_definition WHERE next_run_at<=` + now + ` LIMIT 1`},
		{[]string{"_kora_schedule_occurrence"}, `SELECT 1 FROM _kora_schedule_occurrence WHERE state='pending' OR (state='running' AND lease_until IS NOT NULL AND lease_until<=` + now + `) LIMIT 1`},
		{[]string{"_kora_schedule_occurrence"}, `SELECT 1 FROM _kora_schedule_occurrence WHERE state='pending' AND scheduled_at<=` + now + ` LIMIT 1`},
		{[]string{"_kora_outbox"}, `SELECT 1 FROM _kora_outbox WHERE (status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=` + now + `)) OR (status='publishing' AND lease_until IS NOT NULL AND lease_until<=` + now + `) LIMIT 1`},
		{[]string{"_kora_outbox", "_kora_outbox_receipt"}, `SELECT 1 FROM _kora_outbox o WHERE (o.event_type LIKE 'kora.%.after!_insert' ESCAPE '!' OR o.event_type LIKE 'kora.%.after!_save' ESCAPE '!' OR o.event_type LIKE 'kora.%.after!_delete' ESCAPE '!' OR o.event_type LIKE 'kora.%.after!_submit' ESCAPE '!' OR o.event_type LIKE 'kora.%.after!_cancel' ESCAPE '!') AND NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name='engine-webhooks-v1' AND r.event_id=o.id) LIMIT 1`},
		{[]string{"_kora_outbox", "_kora_outbox_receipt", "_kora_workflow_subscription", "_kora_workflow_instance"}, `SELECT 1 FROM _kora_outbox o JOIN _kora_workflow_subscription sub ON sub.event_type=o.event_type JOIN _kora_workflow_instance i ON i.tenant_id=sub.tenant_id AND i.id=sub.instance_id AND i.current_step=sub.step_id AND i.status='waiting' WHERE NOT EXISTS (SELECT 1 FROM _kora_outbox_receipt r WHERE r.consumer_name='workflow-signals' AND r.event_id=o.id) LIMIT 1`},
		{[]string{"_kora_workflow_instance"}, `SELECT 1 FROM _kora_workflow_instance WHERE ((status IN ('ready','retrying','running') AND (next_wake_at IS NULL OR next_wake_at<=` + now + `) AND (lease_until IS NULL OR lease_until<=` + now + `)) OR (status='waiting' AND next_wake_at IS NOT NULL AND next_wake_at<=` + now + `)) LIMIT 1`},
		{[]string{"_kora_script_invocation"}, `SELECT 1 FROM _kora_script_invocation WHERE (status='pending' AND next_attempt_at<=` + now + `) OR (status='running' AND lease_until IS NOT NULL AND lease_until<=` + now + `) LIMIT 1`},
		{[]string{"_kora_workflow_process_timer"}, `SELECT 1 FROM _kora_workflow_process_timer WHERE due_at<=` + now + ` LIMIT 1`},
		{[]string{"_kora_workflow_signal", "_kora_workflow_signal_receipt", "_kora_workflow_subscription", "_kora_workflow_instance"}, `SELECT 1 FROM _kora_workflow_signal s JOIN _kora_workflow_subscription sub ON sub.tenant_id=s.tenant_id AND sub.event_type=s.event_type JOIN _kora_workflow_instance i ON i.tenant_id=sub.tenant_id AND i.id=sub.instance_id AND i.current_step=sub.step_id AND i.status='waiting' LEFT JOIN _kora_workflow_signal_receipt r ON r.tenant_id=s.tenant_id AND r.event_id=s.event_id AND r.instance_id=i.id WHERE r.event_id IS NULL LIMIT 1`},
		{[]string{"_kora_request"}, `SELECT 1 FROM _kora_request WHERE status='pending' AND due_at IS NOT NULL AND due_at<=` + now + ` LIMIT 1`},
		{[]string{"_kora_notification_delivery"}, `SELECT 1 FROM _kora_notification_delivery WHERE (status='pending' AND available_at<=` + now + `) OR (status='sending' AND lease_until<=` + now + `) LIMIT 1`},
		{[]string{"_kora_analytics_rebuild_job", "_kora_analytics_rebuild_lease"}, `SELECT 1 FROM _kora_analytics_rebuild_job j LEFT JOIN _kora_analytics_rebuild_lease l ON l.site=j.site AND l.job_id=j.id WHERE j.status IN ('queued','running') AND (l.job_id IS NULL OR l.lease_until<=` + now + `) LIMIT 1`},
	}
	needed := make(map[string]struct{})
	for _, check := range checks {
		for _, table := range check.tables {
			needed[table] = struct{}{}
		}
	}
	tables, err := durableTablesPresent(ctx, database, dialect.DriverName(), needed)
	if err != nil {
		return false, err
	}
	for _, check := range checks {
		available := true
		for _, table := range check.tables {
			if !tables[table] {
				available = false
				break
			}
		}
		if !available {
			continue
		}
		var one int
		err = database.QueryRowContext(ctx, check.query).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("probe durable work tables %v: %w", check.tables, err)
		}
	}
	return false, nil
}

func durableTablesPresent(ctx context.Context, database *sql.DB, driver string, wanted map[string]struct{}) (map[string]bool, error) {
	var query string
	switch driver {
	case "mysql":
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN (`
	case "postgres":
		query = `SELECT table_name FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN (`
	case "libsql":
		query = `SELECT name FROM sqlite_master WHERE type='table' AND name IN (`
	default:
		return nil, fmt.Errorf("unsupported durable work scan dialect %q", driver)
	}
	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)
	for index, name := range names {
		if index > 0 {
			query += ","
		}
		query += "'" + name + "'"
	}
	query += ")"
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list durable work tables: %w", err)
	}
	present := make(map[string]bool, len(names))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan durable work table name: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read durable work table names: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close durable work table names: %w", err)
	}
	return present, nil
}

func (s *DurableWorkScanner) Snapshot() DurableWorkScannerSnapshot {
	if s == nil {
		return DurableWorkScannerSnapshot{}
	}
	s.lastMu.RLock()
	last := s.lastSweep
	s.lastMu.RUnlock()
	age := time.Duration(0)
	if !last.IsZero() {
		age = time.Since(last)
	}
	s.walMu.Lock()
	walPending, walUnknown, walOldest, walObservedAt := s.walPending, s.walUnknown, s.walOldest, s.walObservedAt
	walBytes, walBytesKnown := int64(0), false
	if s.walPublished {
		walPending, walUnknown, walOldest, walObservedAt = s.walPublishedPending, s.walPublishedUnknown, s.walPublishedOldest, s.walPublishedAt
		walBytes, walBytesKnown = s.walPublishedBytes, s.walPublishedBytesKnown
	}
	s.walMu.Unlock()
	if walPending && !walObservedAt.IsZero() {
		walOldest += time.Since(walObservedAt)
	}
	return DurableWorkScannerSnapshot{
		Probes: s.probes.Load(), DueTenants: s.due.Load(), ProbeFailures: s.probeFails.Load(),
		ScanFailures: s.scanFails.Load(), WakeFailures: s.wakeFails.Load(), CapacityRejections: s.capacity.Load(),
		CompletedSweeps: s.sweeps.Load(), LastCompletedSweep: last, LastCompletedSweepAge: age,
		ColdWALPending: walPending, ColdWALAgeUnknown: walUnknown, ColdWALOldestAge: walOldest,
		ColdWALBytes: walBytes, ColdWALBytesKnown: walBytesKnown,
	}
}

// Shutdown cancels and joins the process-owned scanner before its database and
// tenant runtime dependencies are closed.
func (s *DurableWorkScanner) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.startMu.Lock()
	started := s.started
	if s.cancel != nil {
		s.cancel()
	}
	s.startMu.Unlock()
	if !started {
		return nil
	}
	return waitForWorker(ctx, "durable tenant work scanner", s.done)
}
