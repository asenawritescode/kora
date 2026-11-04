// Package scheduler provides cron-based background job scheduling.
package scheduler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/email"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
)

// JobType enumerates the built-in job types.
type JobType string

const (
	JobDoctypeAlert JobType = "doctype_alert"
	JobEmailReport  JobType = "email_report"
	JobWebhook      JobType = "webhook"
)

// JobHandler executes a configured job type. Domain-specific handlers are
// registered by the application composition root, not built into Scheduler.
type JobHandler func(context.Context, *JobConfig) error

// JobConfig holds the parsed scheduler configuration.
type JobConfig struct {
	Name              string                  `yaml:"name"     json:"name"`
	OwnerModuleID     contract.ModuleID       `yaml:"owner_module_id,omitempty" json:"owner_module_id,omitempty"`
	Type              JobType                 `yaml:"type"     json:"type"`
	Schedule          string                  `yaml:"schedule" json:"schedule"` // Cron expression
	Config            map[string]any          `yaml:"config"   json:"config"`
	Scope             *contract.BusinessScope `yaml:"scope,omitempty" json:"scope,omitempty"`
	RequiredResource  string                  `yaml:"required_resource,omitempty" json:"required_resource,omitempty"`
	RequiredOperation string                  `yaml:"required_operation,omitempty" json:"required_operation,omitempty"`
	Execution         policy.Execution        `yaml:"-" json:"-"`
}

// ExecutionResolver returns a fresh, server-issued service execution for one
// exact scheduled business scope. Entity-owned scheduled work fails closed if
// the tenant runtime does not provide this resolver.
type ExecutionResolver func(context.Context, contract.BusinessScope) (policy.Execution, error)

// Scheduler manages and runs background jobs.
type Scheduler struct {
	DB            *sql.DB
	Registry      *doctype.Registry
	TxManager     *orm.TxManager
	Email         *email.Sender
	WebhookWorker WebhookRetrier // optional — for retrying dead-lettered webhooks
	ScriptRunner  script.Runner  // optional — for executing scheduled scripts
	ScriptStore   *script.Store  // optional — for loading scheduled scripts
	SiteName      string         // site name for script execution
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	} // optional tenant active-work admission
	DurableStore       DurableScheduleStore
	TenantID           string
	ResolveExecution   ExecutionResolver
	WorkerID           string
	jobs               []*JobConfig
	handlers           map[JobType]JobHandler
	ctx                context.Context
	cancel             context.CancelFunc
	wg                 sync.WaitGroup
	lifecycleMu        sync.Mutex
	started            bool
	stopped            bool
	stopDone           chan struct{}
	stopJoin           sync.Once
	activeDurable      atomic.Int64
	claims             atomic.Int64
	claimFailures      atomic.Int64
	leaseFailures      atomic.Int64
	leaseLost          atomic.Int64
	uncertain          atomic.Int64
	completionFailures atomic.Int64
	backlogMu          sync.RWMutex
	backlog            durableBacklogSample
	backlogFailures    atomic.Int64
}

// DurableMetricsSnapshot contains cumulative process-local counters for the
// durable scheduler worker. It deliberately contains no tenant or job labels.
type DurableMetricsSnapshot struct {
	Active             int64
	Claims             int64
	ClaimFailures      int64
	LeaseRenewFailures int64
	LeaseLost          int64
	UncertainOutcomes  int64
	CompletionFailures int64
	PendingOccurrences int64
	OldestPendingAge   time.Duration
	BacklogFailures    int64
}

type durableBacklogSample struct {
	pending   int64
	oldestAge time.Duration
	sampledAt time.Time
}

// DurableScheduleBacklogMetricsStore is an optional bounded snapshot query
// implemented by durable stores. It reports persisted pending occurrences,
// not future cron definitions that have not yet materialized.
type DurableScheduleBacklogMetricsStore interface {
	BacklogMetrics(context.Context, string) (int64, time.Time, error)
}

func (s *Scheduler) DurableMetrics() DurableMetricsSnapshot {
	if s == nil {
		return DurableMetricsSnapshot{}
	}
	snapshot := DurableMetricsSnapshot{
		Active: s.activeDurable.Load(), Claims: s.claims.Load(),
		ClaimFailures: s.claimFailures.Load(), LeaseRenewFailures: s.leaseFailures.Load(), LeaseLost: s.leaseLost.Load(),
		UncertainOutcomes: s.uncertain.Load(), CompletionFailures: s.completionFailures.Load(),
		BacklogFailures: s.backlogFailures.Load(),
	}
	s.backlogMu.RLock()
	backlog := s.backlog
	s.backlogMu.RUnlock()
	snapshot.PendingOccurrences = backlog.pending
	snapshot.OldestPendingAge = backlog.oldestAge
	if backlog.pending > 0 && !backlog.sampledAt.IsZero() {
		snapshot.OldestPendingAge += time.Since(backlog.sampledAt)
	}
	if snapshot.OldestPendingAge < 0 {
		snapshot.OldestPendingAge = 0
	}
	return snapshot
}

// DurableScheduleStore persists schedule definitions, occurrences and fenced
// claims. Expired in-flight occurrences become uncertain and require explicit
// reconciliation because handlers may have performed external effects.
type DurableScheduleStore interface {
	Ensure(context.Context) error
	RegisterDefinition(context.Context, string, *JobConfig, string, time.Time, time.Time) error
	NextRunAt(context.Context, string, string) (time.Time, error)
	ClaimDue(context.Context, string, string, string, time.Time, time.Duration) (*ScheduleOccurrence, error)
	Renew(context.Context, ScheduleOccurrence, time.Time, time.Duration) error
	Complete(context.Context, ScheduleOccurrence, time.Time, time.Time) error
	MarkOutcomeUnknown(context.Context, ScheduleOccurrence, time.Time, string) error
}

type ScheduleOccurrence struct {
	TenantID         string
	JobName          string
	ID               string
	DefinitionDigest string
	ScheduledAt      time.Time
	Job              JobConfig
	Owner            string
	Fence            uint64
	Attempts         uint64
}

var ErrScheduleLeaseLost = errors.New("scheduled job occurrence lease lost")
var ErrScheduleOutcomeUnknown = errors.New("scheduled job occurrence outcome is unknown")

// WebhookRetrier is the interface for retrying dead-lettered webhook deliveries.
type WebhookRetrier interface {
	RetryDeadLetters() error
}

type ContextWebhookRetrier interface {
	// Implementations that satisfy this interface acquire their own permits
	// around individual database phases; Scheduler does not wrap them in a
	// broader permit.
	RetryDeadLettersContext(context.Context) error
}

// New creates a new scheduler.
func New(db *sql.DB, registry *doctype.Registry, txManager *orm.TxManager, emailSender *email.Sender) *Scheduler {
	return &Scheduler{
		DB:        db,
		Registry:  registry,
		TxManager: txManager,
		Email:     emailSender,
	}
}

// RegisterHandler binds application-provided behavior to a configured job
// type. Registration is startup-only, matching RegisterJob's lifecycle.
func (s *Scheduler) RegisterHandler(jobType JobType, handler JobHandler) error {
	if s == nil || jobType == "" || handler == nil {
		return errors.New("scheduled job handler requires scheduler, type, and function")
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return errors.New("cannot register a scheduled job handler after scheduler start")
	}
	if isBuiltinJobType(jobType) {
		return fmt.Errorf("cannot override built-in scheduled job type %q", jobType)
	}
	if s.handlers == nil {
		s.handlers = make(map[JobType]JobHandler)
	}
	if _, exists := s.handlers[jobType]; exists {
		return fmt.Errorf("scheduled job handler already registered for %q", jobType)
	}
	s.handlers[jobType] = handler
	return nil
}

// CanExecute reports whether the scheduler has built-in or registered
// application behavior for a job type.
func (s *Scheduler) CanExecute(jobType JobType) bool {
	if isBuiltinJobType(jobType) {
		return true
	}
	if s == nil {
		return false
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.handlers[jobType] != nil
}

func isBuiltinJobType(jobType JobType) bool {
	switch jobType {
	case JobDoctypeAlert, JobEmailReport, JobWebhook:
		return true
	default:
		return false
	}
}

// RegisterJob adds a job to the scheduler.
func (s *Scheduler) RegisterJob(job *JobConfig) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.jobs = append(s.jobs, job)
}

// Start begins running all registered jobs.
func (s *Scheduler) Start() {
	s.StartContext(context.Background())
}

// StartContext starts scheduler jobs under the supplied process/runtime
// context. Stop cancels and joins every job goroutine.
func (s *Scheduler) StartContext(parent context.Context) {
	s.lifecycleMu.Lock()
	if s.started || s.stopped {
		s.lifecycleMu.Unlock()
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	s.ctx, s.cancel = context.WithCancel(parent)
	s.started = true

	for range s.jobs {
		s.wg.Add(1)
	}
	jobs := append([]*JobConfig(nil), s.jobs...)
	backlogStore, hasBacklogMetrics := s.DurableStore.(DurableScheduleBacklogMetricsStore)
	if hasBacklogMetrics && s.TenantID != "" && len(jobs) > 0 {
		s.wg.Add(1)
	}
	s.lifecycleMu.Unlock()
	if hasBacklogMetrics && s.TenantID != "" && len(jobs) > 0 {
		go s.runDurableBacklogSampler(backlogStore)
	}
	for _, job := range jobs {
		job := job // capture loop variable
		go s.runJob(job)
	}

	slog.Info("scheduler started", "jobs", len(s.jobs))
}

func (s *Scheduler) runDurableBacklogSampler(store DurableScheduleBacklogMetricsStore) {
	defer s.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		s.sampleDurableBacklog(store)
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) sampleDurableBacklog(store DurableScheduleBacklogMetricsStore) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	var release func()
	if s.WorkAdmission != nil {
		var err error
		release, err = s.WorkAdmission.Acquire(ctx)
		if err != nil {
			s.backlogFailures.Add(1)
			return
		}
		defer release()
	}
	pending, oldest, err := store.BacklogMetrics(ctx, s.TenantID)
	if err != nil {
		s.backlogFailures.Add(1)
		return
	}
	now := time.Now().UTC()
	var age time.Duration
	if pending > 0 && !oldest.IsZero() {
		age = now.Sub(oldest)
		if age < 0 {
			age = 0
		}
	}
	s.backlogMu.Lock()
	s.backlog = durableBacklogSample{pending: pending, oldestAge: age, sampledAt: now}
	s.backlogMu.Unlock()
}

// Stop gracefully shuts down the scheduler.
func (s *Scheduler) Stop() {
	if err := s.StopContext(context.Background()); err != nil {
		slog.Error("scheduler stop failed", "error", err)
	}
}

// StopContext cancels scheduler jobs and waits for their join, bounded by ctx.
// A timed-out caller can retry after the job exits; the same join channel is
// shared by concurrent callers.
func (s *Scheduler) StopContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.lifecycleMu.Lock()
	s.stopped = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.stopDone == nil {
		s.stopDone = make(chan struct{})
	}
	done := s.stopDone
	s.stopJoin.Do(func() {
		go func() {
			s.wg.Wait()
			close(done)
		}()
	})
	s.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	slog.Info("scheduler stopped")
	return nil
}

func (s *Scheduler) runJob(job *JobConfig) {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler job panicked", "name", job.Name, "panic", r)
		}
	}()

	if s.DurableStore != nil {
		s.runDurableJob(job)
		return
	}
	cron := parseCron(job.Schedule)
	slog.Info("scheduling job", "name", job.Name, "schedule", job.Schedule, "next", cron.nextRun())

	for {
		waitDuration := time.Until(cron.nextRun())
		if waitDuration < 0 {
			waitDuration = time.Minute
		}

		timer := time.NewTimer(waitDuration)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			slog.Info("running job", "name", job.Name)
			if err := s.executeScheduledJob(s.ctx, job); err != nil {
				slog.Error("job failed", "name", job.Name, "error", err)
			}
		}
	}
}

func (s *Scheduler) runDurableJob(job *JobConfig) {
	if s.DurableStore == nil || s.TenantID == "" || s.WorkerID == "" {
		slog.Error("durable scheduler requires store, tenant identity and worker identity", "job", job.Name)
		return
	}
	releaseEnsure, err := s.acquireScheduleWork(s.ctx)
	if err != nil {
		slog.Error("admit durable scheduler schema setup", "tenant_id", s.TenantID, "job", job.Name, "error", err)
		return
	}
	err = s.DurableStore.Ensure(s.ctx)
	releaseEnsure()
	if err != nil {
		slog.Error("ensure durable scheduler schema", "tenant_id", s.TenantID, "job", job.Name, "error", err)
		return
	}
	cron := parseCron(job.Schedule)
	now := time.Now().UTC()
	release, err := s.acquireScheduleWork(s.ctx)
	if err != nil {
		slog.Error("admit durable schedule registration", "tenant_id", s.TenantID, "job", job.Name, "error", err)
		return
	}
	err = s.DurableStore.RegisterDefinition(s.ctx, s.TenantID, job, JobDefinitionDigest(job), cron.nextRunAfter(now), now)
	release()
	if err != nil {
		slog.Error("register durable scheduler definition", "tenant_id", s.TenantID, "job", job.Name, "error", err)
		return
	}
	const lease = 45 * time.Second
	retryFailures := 0
	for {
		if err := s.ctx.Err(); err != nil {
			return
		}
		release, err := s.acquireScheduleWork(s.ctx)
		if err != nil {
			retryFailures++
			if !waitContext(s.ctx, durableRetryDelay(retryFailures)) {
				return
			}
			continue
		}
		occurrence, claimErr := s.DurableStore.ClaimDue(s.ctx, s.TenantID, job.Name, s.WorkerID, time.Now().UTC(), lease)
		if errors.Is(claimErr, ErrScheduleOutcomeUnknown) {
			s.uncertain.Add(1)
			release()
			slog.Error("scheduled occurrence requires reconciliation", "tenant_id", s.TenantID, "job", job.Name, "error", claimErr)
			retryFailures++
			if !waitContext(s.ctx, durableRetryDelay(retryFailures)) {
				return
			}
			continue
		}
		if claimErr != nil {
			s.claimFailures.Add(1)
			release()
			slog.Error("claim durable scheduled occurrence", "tenant_id", s.TenantID, "job", job.Name, "error", claimErr)
			retryFailures++
			if !waitContext(s.ctx, durableRetryDelay(retryFailures)) {
				return
			}
			continue
		}
		if occurrence == nil {
			release()
			release, err = s.acquireScheduleWork(s.ctx)
			if err != nil {
				retryFailures++
				if !waitContext(s.ctx, durableRetryDelay(retryFailures)) {
					return
				}
				continue
			}
			nextRun, nextRunErr := s.DurableStore.NextRunAt(s.ctx, s.TenantID, job.Name)
			release()
			if nextRunErr != nil {
				retryFailures++
				slog.Error("read durable schedule next run", "tenant_id", s.TenantID, "job", job.Name, "error", nextRunErr)
				if !waitContext(s.ctx, durableRetryDelay(retryFailures)) {
					return
				}
				continue
			}
			retryFailures = 0
			if !waitUntil(s.ctx, nextRun) {
				return
			}
			continue
		}
		release()
		retryFailures = 0
		s.claims.Add(1)
		s.runOccurrence(*occurrence, lease)
	}
}

func durableRetryDelay(failures int) time.Duration {
	const base = 250 * time.Millisecond
	const maximum = 15 * time.Second
	if failures < 1 {
		failures = 1
	}
	delay := base
	for i := 1; i < failures && delay < maximum; i++ {
		delay *= 2
		if delay > maximum {
			delay = maximum
		}
	}
	// Equal jitter retains half the capped delay and randomizes the rest.
	half := delay / 2
	return half + time.Duration(rand.Int64N(int64(delay-half)+1))
}

func (s *Scheduler) runOccurrence(occurrence ScheduleOccurrence, lease time.Duration) {
	s.activeDurable.Add(1)
	defer s.activeDurable.Add(-1)
	job := &occurrence.Job
	ctx, cancel := context.WithCancel(s.ctx)
	finished := make(chan struct{})
	var renewErr error
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-finished:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				release, err := s.acquireScheduleWork(ctx)
				if err == nil {
					err = s.DurableStore.Renew(ctx, occurrence, time.Now().UTC(), lease)
					release()
				}
				if err != nil {
					renewErr = err
					cancel()
					return
				}
			}
		}
	}()
	err := s.executeScheduledWork(ctx, job)
	close(finished)
	<-renewDone
	cancel()
	if renewErr != nil {
		if errors.Is(renewErr, ErrScheduleLeaseLost) {
			s.leaseLost.Add(1)
		} else {
			s.leaseFailures.Add(1)
		}
		slog.Error("scheduled occurrence lease renewal failed", "tenant_id", occurrence.TenantID, "job", job.Name, "occurrence_id", occurrence.ID, "error", renewErr)
		return
	}
	if err != nil {
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		release, admissionErr := s.acquireScheduleWork(finalizeCtx)
		markErr := admissionErr
		if markErr == nil {
			markErr = s.DurableStore.MarkOutcomeUnknown(finalizeCtx, occurrence, time.Now().UTC(), err.Error())
			release()
		}
		finalizeCancel()
		if markErr != nil {
			if errors.Is(markErr, ErrScheduleLeaseLost) {
				s.leaseLost.Add(1)
			}
			slog.Error("record uncertain scheduled occurrence", "tenant_id", occurrence.TenantID, "job", job.Name, "occurrence_id", occurrence.ID, "error", errors.Join(err, markErr))
		} else {
			s.uncertain.Add(1)
		}
		return
	}
	next := parseCron(job.Schedule).nextRunAfter(occurrence.ScheduledAt)
	finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	release, completeErr := s.acquireScheduleWork(finalizeCtx)
	if completeErr == nil {
		completeErr = s.DurableStore.Complete(finalizeCtx, occurrence, next, time.Now().UTC())
		release()
	}
	finalizeCancel()
	if completeErr != nil {
		s.completionFailures.Add(1)
		if errors.Is(completeErr, ErrScheduleLeaseLost) {
			s.leaseLost.Add(1)
		}
		slog.Error("complete durable scheduled occurrence", "tenant_id", occurrence.TenantID, "job", job.Name, "occurrence_id", occurrence.ID, "error", completeErr)
	}
}

func (s *Scheduler) acquireScheduleWork(ctx context.Context) (func(), error) {
	if s.WorkAdmission == nil {
		return func() {}, nil
	}
	return s.WorkAdmission.Acquire(ctx)
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func waitUntil(ctx context.Context, deadline time.Time) bool {
	return waitContext(ctx, time.Until(deadline))
}

func (s *Scheduler) executeScheduledJob(ctx context.Context, job *JobConfig) error {
	return s.executeScheduledWork(ctx, job)
}

// executeScheduledWork admits one scheduled action. A context-aware webhook
// retrier owns admission around each SQL phase itself, so its caller must not
// hold a nested permit across the retry scan.
func (s *Scheduler) executeScheduledWork(ctx context.Context, job *JobConfig) error {
	if job == nil {
		return errors.New("scheduled job is required")
	}
	var scope contract.BusinessScope
	if job.Scope != nil {
		scope = *job.Scope
	}
	if job.Scope == nil && s.TenantID != "" {
		scope = contract.BusinessScope{TenantID: contract.TenantID(s.TenantID), Kind: contract.ScopeKindTenant}
	}
	if (scope.TenantID != "" || scope.Kind != "" || scope.LegalEntityID != "") && (scope.Validate() != nil || scope.TenantID != contract.TenantID(s.TenantID)) {
		return contract.NewError(contract.CodePermissionDenied, "scheduled job business scope is invalid")
	}
	job = cloneJobConfig(job)
	if scope.TenantID != "" || scope.Kind != "" || scope.LegalEntityID != "" {
		job.Scope = &scope
	}
	if scope.Kind == contract.ScopeKindLegalEntity {
		if s.ResolveExecution == nil {
			return contract.NewError(contract.CodePermissionDenied, "entity-scoped scheduled work requires a trusted execution resolver")
		}
		execution, err := s.ResolveExecution(ctx, scope)
		if err != nil {
			return fmt.Errorf("resolve scheduled job execution: %w", err)
		}
		if !execution.Valid() || execution.Tenant() != scope.TenantID || execution.Actor().PrincipalType != contract.PrincipalService {
			return contract.NewError(contract.CodePermissionDenied, "scheduled job execution is not a verified tenant service")
		}
		if job.RequiredResource == "" || job.RequiredOperation == "" {
			return contract.NewError(contract.CodePermissionDenied, "entity-scoped scheduled job requires an explicit capability grant")
		}
		if grant, allowed := execution.GrantForScope(job.RequiredResource, job.RequiredOperation, scope); !allowed || grant.OwnerOnly {
			return contract.NewError(contract.CodePermissionDenied, "scheduled job capability grant is missing or revoked")
		}
		job.Execution = execution
	}
	if job != nil && job.Type == JobWebhook {
		if _, ok := s.WebhookWorker.(ContextWebhookRetrier); ok {
			return s.executeJob(ctx, job)
		}
	}
	if s.WorkAdmission == nil {
		return s.executeJob(ctx, job)
	}
	release, err := s.WorkAdmission.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("admit scheduled job %s: %w", job.Name, err)
	}
	defer release()
	return s.executeJob(ctx, job)
}

func cloneJobConfig(job *JobConfig) *JobConfig {
	if job == nil {
		return nil
	}
	copy := *job
	if job.Config != nil {
		copy.Config = make(map[string]any, len(job.Config))
		for key, value := range job.Config {
			copy.Config[key] = value
		}
	}
	return &copy
}

func (s *Scheduler) executeJob(ctx context.Context, job *JobConfig) error {
	switch job.Type {
	case JobDoctypeAlert:
		return s.runDoctypeAlert(ctx, job)
	case JobEmailReport:
		return s.runEmailReport(ctx, job)
	case JobWebhook:
		return s.runWebhookRetryContext(ctx, job)
	default:
		s.lifecycleMu.Lock()
		handler := s.handlers[job.Type]
		s.lifecycleMu.Unlock()
		if handler == nil {
			return fmt.Errorf("unknown job type: %s", job.Type)
		}
		return handler(ctx, job)
	}
}

func (s *Scheduler) runWebhookRetryContext(ctx context.Context, job *JobConfig) error {
	if s.WebhookWorker == nil {
		return fmt.Errorf("webhook worker not configured")
	}
	if retrier, ok := s.WebhookWorker.(ContextWebhookRetrier); ok {
		return retrier.RetryDeadLettersContext(ctx)
	}
	return s.WebhookWorker.RetryDeadLetters()
}

// LoadScheduledScripts loads all active scheduled scripts and registers them as cron jobs.
func (s *Scheduler) LoadScheduledScripts() error {
	if s.ScriptStore == nil || s.ScriptRunner == nil {
		return nil
	}
	scripts, err := s.ScriptStore.LoadAllForSite(s.SiteName)
	if err != nil {
		return fmt.Errorf("loading scheduled scripts: %w", err)
	}
	for _, rec := range scripts {
		if rec.ScriptType != script.TypeScheduled || rec.Schedule == "" || !rec.IsActive {
			continue
		}
		slog.Info("registering scheduled script", "name", rec.Name, "schedule", rec.Schedule)
		// The script is already loaded — execute it on each cron tick.
		// For now, scheduled scripts run via the existing job mechanism.
		// In the future, this will use a proper cron parser.
	}
	return nil
}

func (s *Scheduler) runScheduledScript(rec script.ScriptRecord) error {
	if s.ScriptRunner == nil {
		return fmt.Errorf("script runner not configured")
	}
	req := script.ExecuteRequest{
		Script: rec.Script, ScriptType: rec.ScriptType, ScriptName: rec.Name,
		User: "scheduler", UserRoles: []string{doctype.AdminRole}, Site: s.SiteName,
		Document: map[string]any{},
	}
	ctx := context.Background()
	_, err := s.ScriptRunner.Execute(ctx, req)
	if err != nil {
		slog.Error("scheduled script failed", "name", rec.Name, "error", err)
	}
	status := "success"
	errMsg := ""
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	if s.ScriptStore != nil {
		_ = s.ScriptStore.LogExecution(s.SiteName, rec, "", "", "", "scheduler", 0, status, errMsg)
	}
	return err
}

func (s *Scheduler) runDoctypeAlert(ctx context.Context, job *JobConfig) error {
	doctypeName, _ := job.Config["doctype"].(string)
	if doctypeName == "" {
		return fmt.Errorf("doctype_alert requires 'doctype' in config")
	}

	dt := s.Registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}

	// Build filters from config.
	filtersJSON := buildFiltersJSON(job.Config["filters"])
	tx := *s.TxManager
	tx.Context = ctx
	tx.BusinessScope = scheduledJobScope(job)
	if dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		if !job.Execution.Valid() {
			return contract.NewError(contract.CodePermissionDenied, "entity-scoped scheduled report requires verified service execution")
		}
		decision, denied := policy.DecideForBusinessScope(job.Execution, s.Registry, dt.Name, "read", tx.BusinessScope, true)
		if denied != nil {
			return denied
		}
		if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
			return contract.NewError(contract.CodePermissionDenied, "scheduled report cannot apply a user-specific row grant")
		}
	}
	docs, total, err := tx.GetList(dt, filtersJSON, "", 500, 0, "")
	if err != nil {
		return fmt.Errorf("querying doctype %s: %w", doctypeName, err)
	}

	if total == 0 {
		slog.Debug("doctype_alert: no matching documents", "job", job.Name)
		return nil
	}

	notifyField, _ := job.Config["notify_field"].(string)
	subject, _ := job.Config["subject"].(string)
	message, _ := job.Config["message"].(string)

	for _, doc := range docs {
		var recipients []string
		if notifyField != "" {
			recipient := doc.GetString(notifyField)
			if recipient != "" {
				recipients = append(recipients, recipient)
			}
		}

		if len(recipients) == 0 {
			continue
		}

		data := docToTemplateData(doc, dt)
		if err := s.Email.SendTemplateContext(ctx, recipients, subject, message, data); err != nil {
			slog.Error("sending alert email", "job", job.Name, "error", err)
		}
	}

	slog.Info("doctype_alert completed", "job", job.Name, "matches", total)
	return nil
}

func (s *Scheduler) runEmailReport(ctx context.Context, job *JobConfig) error {
	doctypeName, _ := job.Config["doctype"].(string)
	if doctypeName == "" {
		return fmt.Errorf("email_report requires 'doctype' in config")
	}

	dt := s.Registry.Get(doctypeName)
	if dt == nil {
		return fmt.Errorf("doctype %q not found", doctypeName)
	}

	filtersJSON := buildFiltersJSON(job.Config["filters"])
	tx := *s.TxManager
	tx.Context = ctx
	tx.BusinessScope = scheduledJobScope(job)
	if dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		if !job.Execution.Valid() {
			return contract.NewError(contract.CodePermissionDenied, "entity-scoped scheduled report requires verified service execution")
		}
		decision, denied := policy.DecideForBusinessScope(job.Execution, s.Registry, dt.Name, "read", tx.BusinessScope, true)
		if denied != nil {
			return denied
		}
		if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
			return contract.NewError(contract.CodePermissionDenied, "scheduled report cannot apply a user-specific row grant")
		}
	}
	docs, total, err := tx.GetList(dt, filtersJSON, "", 500, 0, "")
	if err != nil {
		return fmt.Errorf("querying doctype %s: %w", doctypeName, err)
	}

	subject, _ := job.Config["subject"].(string)
	message, _ := job.Config["message"].(string)

	// Build report body.
	var body strings.Builder
	body.WriteString(message)
	body.WriteString("\n\n")
	for _, doc := range docs {
		title := doc.GetString(dt.TitleField)
		if title == "" {
			title = doc.Name
		}
		body.WriteString(fmt.Sprintf("- %s: %s\n", doc.Name, title))
	}
	body.WriteString(fmt.Sprintf("\nTotal: %d records\n", total))

	// Get recipients from config.
	recipientList, _ := job.Config["recipients"].([]any)
	var recipients []string
	for _, r := range recipientList {
		if rMap, ok := r.(map[string]any); ok {
			if email, ok := rMap["email"].(string); ok && email != "" {
				recipients = append(recipients, email)
			}
		}
	}

	if len(recipients) == 0 {
		slog.Warn("email_report: no recipients", "job", job.Name)
		return nil
	}

	return s.Email.SendTemplateContext(ctx, recipients, subject, body.String(), nil)
}

func scheduledJobScope(job *JobConfig) contract.BusinessScope {
	if job == nil || job.Scope == nil {
		return contract.BusinessScope{}
	}
	return *job.Scope
}

// cronSchedule represents a parsed cron expression.
type cronSchedule struct {
	minute, hour, dom, month, dow string
}

func parseCron(expr string) *cronSchedule {
	parts := strings.Fields(expr)
	if len(parts) < 5 {
		// Default to every hour if invalid.
		return &cronSchedule{"0", "*", "*", "*", "*"}
	}
	return &cronSchedule{
		minute: parts[0],
		hour:   parts[1],
		dom:    parts[2],
		month:  parts[3],
		dow:    parts[4],
	}
}

// ValidateSchedule accepts the cron subset currently implemented by the
// scheduler: five fields, numeric minute/hour or wildcard, and wildcard-only
// day/month/weekday fields. Reject unsupported syntax instead of silently
// falling back to an hourly schedule.
func ValidateSchedule(expr string) error {
	return contract.ValidateCronSchedule(expr)
}

// nextRun returns the next time this cron schedule should fire.
// This is a simplified implementation that handles basic patterns.
func (c *cronSchedule) nextRun() time.Time {
	return c.nextRunAfter(time.Now())
}

func (c *cronSchedule) nextRunAfter(after time.Time) time.Time {
	now := after
	// Simple: fire at the next matching time, or every minute for "*" patterns.
	next := now.Truncate(time.Minute).Add(time.Minute)

	// For specific hour:minute, compute next match.
	if c.hour != "*" && c.minute != "*" {
		var hour, minute int
		fmt.Sscanf(c.hour, "%d", &hour)
		fmt.Sscanf(c.minute, "%d", &minute)

		today := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if today.Before(now) || today.Equal(now) {
			today = today.Add(24 * time.Hour)
		}
		return today
	}

	return next
}

// NextRunAfter computes the next occurrence using the scheduler's supported
// cron subset. It is exported for durable persistence adapters.
func NextRunAfter(expression string, after time.Time) time.Time {
	return parseCron(expression).nextRunAfter(after)
}

// JobDefinitionDigest returns a stable digest of the scheduled job definition.
func JobDefinitionDigest(job *JobConfig) string {
	if job == nil {
		return ""
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func buildFiltersJSON(filtersRaw any) string {
	if filtersRaw == nil {
		return ""
	}
	// filtersRaw should be a [][]any from YAML.
	// For simplicity, format as JSON string.
	filters, ok := filtersRaw.([]any)
	if !ok {
		return ""
	}

	var parts []string
	for _, f := range filters {
		fArr, ok := f.([]any)
		if !ok || len(fArr) < 3 {
			continue
		}
		field := fmt.Sprintf("%v", fArr[0])
		op := fmt.Sprintf("%v", fArr[1])
		val := fArr[2]

		var valStr string
		switch v := val.(type) {
		case string:
			valStr = fmt.Sprintf(`"%s"`, v)
		default:
			valStr = fmt.Sprintf("%v", v)
		}

		parts = append(parts, fmt.Sprintf(`["%s","%s",%s]`, field, op, valStr))
	}

	return "[" + strings.Join(parts, ",") + "]"
}

func docToTemplateData(doc *doctype.Document, dt *doctype.DocType) map[string]string {
	data := make(map[string]string)
	data["name"] = doc.Name
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			data[f.Fieldname] = fmt.Sprintf("%v", doc.Get(f.Fieldname))
		}
	}
	return data
}
