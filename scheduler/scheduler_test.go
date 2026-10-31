package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
)

func TestSchedulerJobUsesCancellationContextForDatabaseReads(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", SortField: "name", SortOrder: "ASC"})
	s := New(database, registry, &orm.TxManager{DB: database, Registry: registry, Dialect: db.Resolve("mysql")}, nil)
	err = s.executeJob(ctx, &JobConfig{Type: JobDoctypeAlert, Config: map[string]any{"doctype": "Task"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled job error = %v, want context.Canceled", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("cancelled job unexpectedly reached the database: %v", err)
	}
}

type schedulerTestAdmission struct {
	acquired int
	released int
	active   int
	err      error
}

func (a *schedulerTestAdmission) Acquire(context.Context) (func(), error) {
	a.acquired++
	if a.err != nil {
		return nil, a.err
	}
	a.active++
	return func() { a.released++; a.active-- }, nil
}

type schedulerTestRetrier struct{ calls int }

func (r *schedulerTestRetrier) RetryDeadLetters() error { r.calls++; return nil }

type admissionCheckingRetrier struct {
	admission *schedulerTestAdmission
	active    int
}

func (r *admissionCheckingRetrier) RetryDeadLetters() error { return nil }
func (r *admissionCheckingRetrier) RetryDeadLettersContext(context.Context) error {
	r.active = r.admission.active
	return nil
}

type durableMetricsTestStore struct {
	renewErr    error
	completeErr error
}

type scheduleBacklogMetricsTestStore struct {
	pending int64
	oldest  time.Time
	err     error
	calls   int
}

type backlogSamplerJoinStore struct {
	durableMetricsTestStore
	sampled chan struct{}
}

func (s backlogSamplerJoinStore) BacklogMetrics(context.Context, string) (int64, time.Time, error) {
	select {
	case s.sampled <- struct{}{}:
	default:
	}
	return 0, time.Time{}, nil
}

func (s *scheduleBacklogMetricsTestStore) BacklogMetrics(ctx context.Context, tenant string) (int64, time.Time, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return 0, time.Time{}, err
	}
	if tenant != "tenant-a" {
		return 0, time.Time{}, errors.New("unexpected tenant")
	}
	return s.pending, s.oldest, s.err
}

func TestDurableBacklogSamplerAdmitsAndCachesBoundedSnapshot(t *testing.T) {
	oldest := time.Now().UTC().Add(-2 * time.Minute)
	store := &scheduleBacklogMetricsTestStore{pending: 3, oldest: oldest}
	admission := &schedulerTestAdmission{}
	s := &Scheduler{TenantID: "tenant-a", WorkAdmission: admission}
	s.ctx = context.Background()
	s.sampleDurableBacklog(store)
	metrics := s.DurableMetrics()
	if store.calls != 1 || admission.acquired != 1 || admission.released != 1 || admission.active != 0 {
		t.Fatalf("backlog sample work: calls=%d admission=%+v", store.calls, admission)
	}
	if metrics.PendingOccurrences != 3 || metrics.OldestPendingAge < time.Minute || metrics.BacklogFailures != 0 {
		t.Fatalf("durable backlog metrics = %+v, want count 3 and old pending work", metrics)
	}
}

func TestDurableBacklogSamplerCountsAdmissionAndQueryFailures(t *testing.T) {
	store := &scheduleBacklogMetricsTestStore{pending: 1, oldest: time.Now().Add(-time.Minute)}
	admission := &schedulerTestAdmission{err: errors.New("capacity full")}
	s := &Scheduler{TenantID: "tenant-a", WorkAdmission: admission}
	s.ctx = context.Background()
	s.sampleDurableBacklog(store)
	if store.calls != 0 || s.DurableMetrics().BacklogFailures != 1 {
		t.Fatalf("rejected backlog sample: calls=%d metrics=%+v", store.calls, s.DurableMetrics())
	}

	admission = &schedulerTestAdmission{}
	s.WorkAdmission = admission
	store.err = errors.New("database unavailable")
	s.sampleDurableBacklog(store)
	metrics := s.DurableMetrics()
	if store.calls != 1 || admission.released != 1 || metrics.BacklogFailures != 2 || metrics.PendingOccurrences != 0 {
		t.Fatalf("failed backlog sample: calls=%d admission=%+v metrics=%+v", store.calls, admission, metrics)
	}
}

func TestDurableBacklogSamplerIsJoinedOnStop(t *testing.T) {
	store := backlogSamplerJoinStore{sampled: make(chan struct{}, 1)}
	s := New(nil, nil, nil, nil)
	s.DurableStore = store
	s.TenantID = "tenant-a"
	s.WorkerID = "worker-a"
	s.RegisterJob(&JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "@every 1h"})
	s.StartContext(context.Background())
	select {
	case <-store.sampled:
	case <-time.After(time.Second):
		t.Fatal("backlog sampler did not take its initial sample")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.StopContext(ctx); err != nil {
		t.Fatalf("StopContext did not join scheduler and backlog sampler: %v", err)
	}
}

func TestDurableMetricsSeparateFencedLeaseLossFromRenewalFailure(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = durableMetricsTestStore{completeErr: ErrScheduleLeaseLost}
	s.WebhookWorker = &schedulerTestRetrier{}
	s.runOccurrence(ScheduleOccurrence{TenantID: "tenant-a", Job: JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "* * * * *"}}, 30*time.Second)
	metrics := s.DurableMetrics()
	if metrics.LeaseLost != 1 || metrics.CompletionFailures != 1 || metrics.LeaseRenewFailures != 0 {
		t.Fatalf("lease-loss metrics = %+v, want one fenced loss distinct from renewal failures", metrics)
	}
}

func (s durableMetricsTestStore) Ensure(context.Context) error { return nil }
func (s durableMetricsTestStore) RegisterDefinition(context.Context, string, *JobConfig, string, time.Time, time.Time) error {
	return nil
}
func (s durableMetricsTestStore) NextRunAt(context.Context, string, string) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}
func (s durableMetricsTestStore) ClaimDue(context.Context, string, string, string, time.Time, time.Duration) (*ScheduleOccurrence, error) {
	return nil, nil
}
func (s durableMetricsTestStore) Renew(context.Context, ScheduleOccurrence, time.Time, time.Duration) error {
	return s.renewErr
}
func (s durableMetricsTestStore) Complete(context.Context, ScheduleOccurrence, time.Time, time.Time) error {
	return s.completeErr
}
func (durableMetricsTestStore) MarkOutcomeUnknown(context.Context, ScheduleOccurrence, time.Time, string) error {
	return nil
}

type ensureAdmissionStore struct {
	ensured int
}

func (s *ensureAdmissionStore) Ensure(context.Context) error {
	s.ensured++
	return nil
}
func (*ensureAdmissionStore) RegisterDefinition(context.Context, string, *JobConfig, string, time.Time, time.Time) error {
	return nil
}
func (*ensureAdmissionStore) NextRunAt(context.Context, string, string) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}
func (*ensureAdmissionStore) ClaimDue(context.Context, string, string, string, time.Time, time.Duration) (*ScheduleOccurrence, error) {
	return nil, nil
}
func (*ensureAdmissionStore) Renew(context.Context, ScheduleOccurrence, time.Time, time.Duration) error {
	return nil
}
func (*ensureAdmissionStore) Complete(context.Context, ScheduleOccurrence, time.Time, time.Time) error {
	return nil
}
func (*ensureAdmissionStore) MarkOutcomeUnknown(context.Context, ScheduleOccurrence, time.Time, string) error {
	return nil
}

func TestDurableSchedulerSchemaSetupUsesWorkAdmission(t *testing.T) {
	store := &ensureAdmissionStore{}
	admission := &schedulerTestAdmission{err: errors.New("capacity exhausted")}
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = store
	s.WorkAdmission = admission
	s.TenantID = "tenant-a"
	s.WorkerID = "worker-a"
	s.runDurableJob(&JobConfig{Name: "daily", Schedule: "0 0 * * *"})
	if admission.acquired != 1 || admission.released != 0 {
		t.Fatalf("admission acquired/released = %d/%d, want one rejected setup admission", admission.acquired, admission.released)
	}
	if store.ensured != 0 {
		t.Fatalf("schema Ensure calls = %d, want zero after rejected admission", store.ensured)
	}
}

type durableLoopTestStore struct {
	mu          sync.Mutex
	claims      []*ScheduleOccurrence
	claimErr    error
	nextRun     time.Time
	nextRunErr  error
	claimCalls  int
	nextCalls   int
	completions int
	nextRead    chan struct{}
}

func (*durableLoopTestStore) Ensure(context.Context) error { return nil }
func (*durableLoopTestStore) RegisterDefinition(context.Context, string, *JobConfig, string, time.Time, time.Time) error {
	return nil
}
func (s *durableLoopTestStore) NextRunAt(context.Context, string, string) (time.Time, error) {
	s.mu.Lock()
	s.nextCalls++
	nextRun, err, nextRead := s.nextRun, s.nextRunErr, s.nextRead
	s.mu.Unlock()
	if nextRead != nil {
		select {
		case nextRead <- struct{}{}:
		default:
		}
	}
	return nextRun, err
}
func (s *durableLoopTestStore) ClaimDue(context.Context, string, string, string, time.Time, time.Duration) (*ScheduleOccurrence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls++
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if len(s.claims) == 0 {
		return nil, nil
	}
	claim := s.claims[0]
	s.claims = s.claims[1:]
	return claim, nil
}
func (*durableLoopTestStore) Renew(context.Context, ScheduleOccurrence, time.Time, time.Duration) error {
	return nil
}
func (s *durableLoopTestStore) Complete(context.Context, ScheduleOccurrence, time.Time, time.Time) error {
	s.mu.Lock()
	s.completions++
	s.mu.Unlock()
	return nil
}
func (*durableLoopTestStore) MarkOutcomeUnknown(context.Context, ScheduleOccurrence, time.Time, string) error {
	return nil
}

func TestDurableSchedulerClaimsOnStartupThenCancelsFutureTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &durableLoopTestStore{nextRun: time.Now().Add(time.Hour), nextRead: make(chan struct{}, 1)}
	s := New(nil, nil, nil, nil)
	s.ctx = ctx
	s.DurableStore = store
	s.TenantID = "tenant-a"
	s.WorkerID = "worker-a"
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDurableJob(&JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "@every 1h"})
	}()
	select {
	case <-store.nextRead:
	case <-time.After(time.Second):
		t.Fatal("durable scheduler did not claim immediately and read its persisted cursor")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("durable scheduler did not cancel its one-shot timer")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimCalls != 1 || store.nextCalls != 1 {
		t.Fatalf("startup calls = %d claims, %d next-run reads; want 1 and 1", store.claimCalls, store.nextCalls)
	}
}

func TestDurableSchedulerDrainsRepeatedOverdueOccurrencesBeforeSleeping(t *testing.T) {
	now := time.Now().UTC()
	job := JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "@every 1h"}
	store := &durableLoopTestStore{
		claims: []*ScheduleOccurrence{
			{TenantID: "tenant-a", JobName: job.Name, ScheduledAt: now.Add(-2 * time.Hour), Job: job},
			{TenantID: "tenant-a", JobName: job.Name, ScheduledAt: now.Add(-time.Hour), Job: job},
		},
		nextRun:  now.Add(time.Hour),
		nextRead: make(chan struct{}, 1),
	}
	retrier := &schedulerTestRetrier{}
	ctx, cancel := context.WithCancel(context.Background())
	s := New(nil, nil, nil, nil)
	s.ctx = ctx
	s.DurableStore = store
	s.TenantID = "tenant-a"
	s.WorkerID = "worker-a"
	s.WebhookWorker = retrier
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDurableJob(&job)
	}()
	select {
	case <-store.nextRead:
	case <-time.After(time.Second):
		t.Fatal("durable scheduler did not drain overdue occurrences")
	}
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	if retrier.calls != 2 || store.completions != 2 || store.claimCalls != 3 {
		t.Fatalf("drain result = %d executions, %d completions, %d claims; want 2, 2, 3", retrier.calls, store.completions, store.claimCalls)
	}
}

func TestDurableSchedulerBacksOffAfterDatabaseError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &durableLoopTestStore{claimErr: errors.New("database unavailable")}
	s := New(nil, nil, nil, nil)
	s.ctx = ctx
	s.DurableStore = store
	s.TenantID = "tenant-a"
	s.WorkerID = "worker-a"
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDurableJob(&JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "@every 1h"})
	}()
	time.Sleep(50 * time.Millisecond) // the minimum first retry is 125ms
	cancel()
	<-done
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claimCalls != 1 {
		t.Fatalf("claim calls during initial backoff = %d, want 1", store.claimCalls)
	}
}

func TestDurableRetryDelayIsCappedAndJittered(t *testing.T) {
	for failures := 1; failures <= 20; failures++ {
		delay := durableRetryDelay(failures)
		if delay < 125*time.Millisecond || delay > 15*time.Second {
			t.Fatalf("retry delay for %d failures = %s, outside capped jitter bounds", failures, delay)
		}
	}
}

type blockingSchedulerRetrier struct{}

func (blockingSchedulerRetrier) RetryDeadLetters() error { return nil }
func (blockingSchedulerRetrier) RetryDeadLettersContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type failingSchedulerRetrier struct{}

func (failingSchedulerRetrier) RetryDeadLetters() error { return errors.New("effect failed") }
func (failingSchedulerRetrier) RetryDeadLettersContext(context.Context) error {
	return errors.New("effect failed")
}

func TestDurableSchedulerMetricsTrackLeaseLossAndUncertainOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := New(nil, nil, nil, nil)
	s.ctx = ctx
	s.DurableStore = durableMetricsTestStore{renewErr: errors.New("lease lost")}
	s.WebhookWorker = blockingSchedulerRetrier{}
	s.runOccurrence(ScheduleOccurrence{Job: JobConfig{Name: "retry", Type: JobWebhook}}, 30*time.Millisecond)
	cancel()
	metrics := s.DurableMetrics()
	if metrics.Active != 0 || metrics.LeaseRenewFailures != 1 || metrics.UncertainOutcomes != 0 {
		t.Fatalf("durable scheduler metrics = %+v, want inactive with one lease failure and no locally confirmed uncertain outcome", metrics)
	}
}

func TestDurableSchedulerMetricsTrackPersistedUncertainOutcome(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = durableMetricsTestStore{}
	s.WebhookWorker = failingSchedulerRetrier{}
	s.runOccurrence(ScheduleOccurrence{Job: JobConfig{Name: "retry", Type: JobWebhook}}, 30*time.Millisecond)
	metrics := s.DurableMetrics()
	if metrics.Active != 0 || metrics.UncertainOutcomes != 1 {
		t.Fatalf("durable scheduler metrics = %+v, want inactive with one persisted uncertain outcome", metrics)
	}
}

func TestDurableSchedulerMetricsTrackCompletionFailure(t *testing.T) {
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = durableMetricsTestStore{completeErr: errors.New("database unavailable")}
	s.WebhookWorker = &schedulerTestRetrier{}
	s.runOccurrence(ScheduleOccurrence{Job: JobConfig{Name: "retry", Type: JobWebhook}}, 30*time.Millisecond)
	metrics := s.DurableMetrics()
	if metrics.Active != 0 || metrics.CompletionFailures != 1 || metrics.UncertainOutcomes != 0 {
		t.Fatalf("durable scheduler metrics = %+v, want one completion failure and no uncertain outcome", metrics)
	}
}

func TestDurableOccurrenceReleasesClaimPermitForEffectAndAdmitsCompletion(t *testing.T) {
	admission := &schedulerTestAdmission{}
	retrier := &admissionCheckingRetrier{admission: admission}
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = durableMetricsTestStore{}
	s.WorkAdmission = admission
	s.WebhookWorker = retrier
	s.runOccurrence(ScheduleOccurrence{Job: JobConfig{Name: "retry", Type: JobWebhook}}, time.Second)
	if retrier.active != 0 {
		t.Fatalf("scheduled effect observed %d DB permits, want none", retrier.active)
	}
	if admission.acquired != 1 || admission.released != 1 || admission.active != 0 {
		t.Fatalf("admission=(%d acquired,%d released,%d active), want completion SQL only", admission.acquired, admission.released, admission.active)
	}
}

func TestDurableOccurrenceAdmitsUncertainOutcomeWrite(t *testing.T) {
	admission := &schedulerTestAdmission{}
	s := New(nil, nil, nil, nil)
	s.ctx = context.Background()
	s.DurableStore = durableMetricsTestStore{}
	s.WorkAdmission = admission
	s.WebhookWorker = failingSchedulerRetrier{}
	s.runOccurrence(ScheduleOccurrence{Job: JobConfig{Name: "retry", Type: JobWebhook}}, time.Second)
	if admission.acquired != 1 || admission.released != 1 || admission.active != 0 {
		t.Fatalf("admission=(%d acquired,%d released,%d active), want one admitted uncertain-outcome write", admission.acquired, admission.released, admission.active)
	}
	if got := s.DurableMetrics().UncertainOutcomes; got != 1 {
		t.Fatalf("uncertain outcomes = %d, want one persisted outcome", got)
	}
}

func TestScheduledJobUsesOneAdmissionAroundEffect(t *testing.T) {
	admission := &schedulerTestAdmission{}
	retrier := &schedulerTestRetrier{}
	s := New(nil, nil, nil, nil)
	s.WorkAdmission = admission
	s.WebhookWorker = retrier
	if err := s.executeScheduledJob(context.Background(), &JobConfig{Name: "retry", Type: JobWebhook}); err != nil {
		t.Fatal("execute scheduled job:", err)
	}
	if admission.acquired != 1 || admission.released != 1 || retrier.calls != 1 {
		t.Fatalf("admission=(%d acquired,%d released), effect calls=%d; want 1/1/1", admission.acquired, admission.released, retrier.calls)
	}

	admission.err = errors.New("capacity exhausted")
	if err := s.executeScheduledJob(context.Background(), &JobConfig{Name: "retry", Type: JobWebhook}); err == nil {
		t.Fatal("scheduled job succeeded without an admission permit")
	}
	if admission.acquired != 2 || retrier.calls != 1 {
		t.Fatalf("rejected admission reached effect: acquired=%d calls=%d", admission.acquired, retrier.calls)
	}
}

func TestSchedulerStopCancelsAndJoinsJobs(t *testing.T) {
	s := New(&sql.DB{}, nil, nil, nil)
	s.RegisterJob(&JobConfig{Name: "wait", Type: JobWebhook, Schedule: "* * * * *"})
	s.StartContext(context.Background())
	s.Stop()
	if s.ctx.Err() == nil {
		t.Fatal("Stop must cancel the worker context")
	}
	// Stop returns only after every registered job goroutine has joined.
}

func TestSchedulerStopContextCanRetryAfterJoinTimeout(t *testing.T) {
	s := New(&sql.DB{}, nil, nil, nil)
	s.wg.Add(1) // represent a worker held in an uninterruptible operation
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := s.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext error = %v, want deadline", err)
	}
	cancel()
	joined := make(chan struct{})
	go func() {
		s.wg.Done()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("held worker did not release")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.StopContext(ctx); err != nil {
		t.Fatalf("retry StopContext: %v", err)
	}
}

func TestSchedulerStartAndStopAreSingleOwnerTransitions(t *testing.T) {
	s := New(&sql.DB{}, nil, nil, nil)
	s.RegisterJob(&JobConfig{Name: "wait", Type: JobWebhook, Schedule: "* * * * *"})
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartContext(parent)
	firstCtx := s.ctx
	if firstCtx == nil {
		t.Fatal("StartContext did not create an owned worker context")
	}
	s.StartContext(context.Background())
	s.RegisterJob(&JobConfig{Name: "late", Type: JobWebhook, Schedule: "* * * * *"})
	if len(s.jobs) != 1 {
		t.Fatalf("late registration changed the active worker set: got %d configured jobs", len(s.jobs))
	}
	if s.ctx != firstCtx {
		t.Fatal("repeated StartContext replaced the active lifecycle context")
	}
	s.Stop()
	if firstCtx.Err() == nil {
		t.Fatal("Stop did not cancel the active worker context")
	}
	joined := make(chan struct{})
	go func() {
		s.Stop()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("repeated Stop did not return after joining workers")
	}
	s.StartContext(context.Background())
	if s.ctx != firstCtx {
		t.Fatal("stopped scheduler unexpectedly started another worker generation")
	}
}

func TestParseCron_EveryMinute(t *testing.T) {
	c := parseCron("* * * * *")
	if c == nil {
		t.Fatal("parseCron returned nil")
	}
	if c.minute != "*" {
		t.Errorf("minute = %q, want %q", c.minute, "*")
	}
	if c.hour != "*" {
		t.Errorf("hour = %q, want %q", c.hour, "*")
	}
	if c.dom != "*" {
		t.Errorf("dom = %q, want %q", c.dom, "*")
	}
	if c.month != "*" {
		t.Errorf("month = %q, want %q", c.month, "*")
	}
	if c.dow != "*" {
		t.Errorf("dow = %q, want %q", c.dow, "*")
	}
}

func TestParseCron_SpecificHour(t *testing.T) {
	tests := []struct {
		name        string
		expr        string
		wantMinute  string
		wantHour    string
		wantInvalid bool
	}{
		{
			name:       "every hour at minute 30",
			expr:       "30 * * * *",
			wantMinute: "30",
			wantHour:   "*",
		},
		{
			name:       "daily at 9am",
			expr:       "0 9 * * *",
			wantMinute: "0",
			wantHour:   "9",
		},
		{
			name:        "specific time with dom",
			expr:        "15 14 1 * *",
			wantMinute:  "15",
			wantHour:    "14",
			wantInvalid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parseCron(tt.expr)
			if c.minute != tt.wantMinute {
				t.Errorf("minute = %q, want %q", c.minute, tt.wantMinute)
			}
			if c.hour != tt.wantHour {
				t.Errorf("hour = %q, want %q", c.hour, tt.wantHour)
			}
		})
	}
}

func TestParseCron_Invalid(t *testing.T) {
	tests := []struct {
		name string
		expr string
	}{
		{"empty string", ""},
		{"too few fields", "30 * *"},
		{"only one field", "30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := parseCron(tt.expr)
			// Invalid cron should return default (every hour at minute 0).
			if c == nil {
				t.Fatal("parseCron should not return nil for invalid expressions")
			}
			if c.minute != "0" {
				t.Errorf("invalid cron: minute = %q, want default %q", c.minute, "0")
			}
		})
	}
}

func TestNextRun_EveryMinute(t *testing.T) {
	c := parseCron("* * * * *")
	next := c.nextRun()
	now := time.Now()

	if next.Before(now) {
		t.Errorf("nextRun before now: %v < %v", next, now)
	}

	// Should be within the next 2 minutes.
	if next.After(now.Add(2 * time.Minute)) {
		t.Errorf("nextRun too far: %v", next)
	}

	if next.Second() != 0 {
		t.Errorf("nextRun should be on a minute boundary, got second = %d", next.Second())
	}
}

func TestDurableScheduleDefinitionIsDeterministicAndUsesSnapshotTime(t *testing.T) {
	jobA := &JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "0 * * * *", Config: map[string]any{"b": 2, "a": 1}}
	jobB := &JobConfig{Name: "hourly", Type: JobWebhook, Schedule: "0 * * * *", Config: map[string]any{"a": 1, "b": 2}}
	if JobDefinitionDigest(jobA) != JobDefinitionDigest(jobB) {
		t.Fatal("map ordering changed the immutable job definition digest")
	}
	jobB.Config["a"] = 3
	if JobDefinitionDigest(jobA) == JobDefinitionDigest(jobB) {
		t.Fatal("job configuration change did not change the immutable digest")
	}
	after := time.Date(2026, time.October, 15, 10, 0, 0, 0, time.UTC)
	if got, want := NextRunAfter("0 9 * * *", after), time.Date(2026, time.October, 16, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("NextRunAfter = %s, want %s", got, want)
	}
}

func TestNextRun_SpecificTime(t *testing.T) {
	// Test with a specific hour:minute.
	now := time.Now()

	// Pick a time in the past today.
	pastHour := (now.Hour() - 1 + 24) % 24
	expr := "30 " + itoa(pastHour) + " * * *"
	c := parseCron(expr)
	next := c.nextRun()

	todayExpected := time.Date(now.Year(), now.Month(), now.Day(), pastHour, 30, 0, 0, now.Location())

	if todayExpected.Before(now) {
		// Should have wrapped to tomorrow.
		tomorrowExpected := todayExpected.Add(24 * time.Hour)
		if !next.Equal(tomorrowExpected) {
			t.Errorf("nextRun = %v, want %v (wrapped to tomorrow)", next, tomorrowExpected)
		}
	} else {
		if !next.Equal(todayExpected) {
			t.Errorf("nextRun = %v, want %v", next, todayExpected)
		}
	}
}

func TestRegisterJob(t *testing.T) {
	s := &Scheduler{}
	if len(s.jobs) != 0 {
		t.Error("new scheduler should have no jobs")
	}

	job := &JobConfig{
		Name:     "test-job",
		Type:     JobDoctypeAlert,
		Schedule: "0 * * * *",
		Config:   map[string]any{"doctype": "Task"},
	}
	s.RegisterJob(job)

	if len(s.jobs) != 1 {
		t.Fatalf("jobs length = %d, want 1", len(s.jobs))
	}
	if s.jobs[0].Name != "test-job" {
		t.Errorf("job name = %q, want %q", s.jobs[0].Name, "test-job")
	}
}

func TestValidateSchedule(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		wantErr  bool
	}{
		{name: "daily at midnight", schedule: "0 0 * * *"},
		{name: "top of hour", schedule: "0 * * * *"},
		{name: "too few fields", schedule: "0 0 *", wantErr: true},
		{name: "unsupported step syntax", schedule: "*/5 * * * *", wantErr: true},
		{name: "unsupported weekday constraint", schedule: "0 0 * * 1", wantErr: true},
		{name: "out of range minute", schedule: "60 0 * * *", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSchedule(tt.schedule)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateSchedule(%q) error = %v, wantErr %t", tt.schedule, err, tt.wantErr)
			}
		})
	}
}

func TestJobType_Constants(t *testing.T) {
	tests := []struct {
		jobType JobType
		want    string
	}{
		{JobDoctypeAlert, "doctype_alert"},
		{JobEmailReport, "email_report"},
		{JobWebhook, "webhook"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if string(tt.jobType) != tt.want {
				t.Errorf("JobType = %q, want %q", string(tt.jobType), tt.want)
			}
		})
	}
}

func TestApplicationJobHandlerDispatchesWithoutBuiltInDomainType(t *testing.T) {
	s := &Scheduler{}
	jobType := JobType("application_task")
	called := false
	if err := s.RegisterHandler(jobType, func(ctx context.Context, job *JobConfig) error {
		called = true
		if ctx == nil || job == nil || job.Config["marker"] != "passed" {
			t.Fatal("application handler did not receive its context and configured job")
		}
		return nil
	}); err != nil {
		t.Fatal("RegisterHandler:", err)
	}
	if err := s.executeJob(context.Background(), &JobConfig{Type: jobType, Config: map[string]any{"marker": "passed"}}); err != nil {
		t.Fatal("executeJob:", err)
	}
	if !called {
		t.Fatal("application handler was not invoked")
	}
}

// itoa is a simple int-to-string helper for test purposes.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
