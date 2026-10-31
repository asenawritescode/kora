package workflow

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

type workerTestStore struct {
	claims      []Claim
	claimLimit  uint16
	claimCalls  atomic.Int32
	renewCalls  atomic.Int32
	releaseCall atomic.Int32
	renewErr    error
}

type workflowWorkAdmission struct {
	calls    atomic.Int32
	releases atomic.Int32
	failAt   int32
}

func (a *workflowWorkAdmission) Acquire(context.Context) (func(), error) {
	call := a.calls.Add(1)
	if call == a.failAt {
		return nil, errors.New("work admission exhausted")
	}
	return func() { a.releases.Add(1) }, nil
}

func (s *workerTestStore) ClaimDue(_ context.Context, _ contract.TenantID, _ string, limit uint16, _ time.Duration) ([]Claim, error) {
	s.claimLimit = limit
	s.claimCalls.Add(1)
	if len(s.claims) > int(limit) {
		return append([]Claim(nil), s.claims[:limit]...), nil
	}
	return append([]Claim(nil), s.claims...), nil
}
func (s *workerTestStore) Renew(context.Context, Claim, time.Duration) (Claim, error) {
	s.renewCalls.Add(1)
	if s.renewErr != nil {
		return Claim{}, s.renewErr
	}
	return Claim{}, nil
}
func (*workerTestStore) BeginExternalAttempt(context.Context, Claim, string, contract.IdempotencyKey) error {
	return nil
}
func (*workerTestStore) CommitStep(context.Context, StepCommit) error { return nil }
func (s *workerTestStore) Release(context.Context, Claim) error {
	s.releaseCall.Add(1)
	return nil
}

func validWorkerConfig() ExecutionWorkerConfig {
	return ExecutionWorkerConfig{Tenant: "tenant-a", Owner: "worker-a", Concurrency: 2, Batch: 2, Lease: time.Second, PollInterval: time.Second}
}

func TestNewExecutionWorkerRejectsBatchAboveConcurrency(t *testing.T) {
	config := validWorkerConfig()
	config.Batch = 3
	if _, err := NewExecutionWorker(&workerTestStore{}, ClaimHandlerFunc(func(context.Context, Claim) error { return nil }), config, nil); err == nil {
		t.Fatal("worker accepted more claims than concurrent execution slots")
	}
}

func TestExecutionWorkerRunOnceBoundsClaimsAndJoinsHandlers(t *testing.T) {
	store := &workerTestStore{claims: []Claim{{InstanceID: "instance-1", StepID: "one"}, {InstanceID: "instance-2", StepID: "two"}, {InstanceID: "instance-3", StepID: "three"}}}
	var active, maximum atomic.Int32
	var handlers sync.WaitGroup
	handler := ClaimHandlerFunc(func(ctx context.Context, _ Claim) error {
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		handlers.Add(1)
		defer func() { active.Add(-1); handlers.Done() }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(40 * time.Millisecond):
			return nil
		}
	})
	worker, err := NewExecutionWorker(store, handler, validWorkerConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal("run bounded batch:", err)
	}
	handlers.Wait()
	if store.claimLimit != 2 || store.claimCalls.Load() != 1 || maximum.Load() > 2 || active.Load() != 0 {
		t.Fatalf("worker bounds: limit=%d claim_calls=%d max_active=%d active=%d", store.claimLimit, store.claimCalls.Load(), maximum.Load(), active.Load())
	}
}

func TestExecutionWorkerAdmitsClaimBatchAndEachStep(t *testing.T) {
	store := &workerTestStore{claims: []Claim{{InstanceID: "instance-1", StepID: "one"}, {InstanceID: "instance-2", StepID: "two"}}}
	admission := &workflowWorkAdmission{}
	config := validWorkerConfig()
	config.WorkAdmission = admission
	worker, err := NewExecutionWorker(store, ClaimHandlerFunc(func(ctx context.Context, _ Claim) error {
		if !dbaccess.HasWorkAdmission(ctx) {
			return errors.New("workflow handler context did not carry its active-work permit")
		}
		_, release, err := dbaccess.AcquireWorkAdmission(ctx, admission)
		if err != nil {
			return err
		}
		release()
		return nil
	}), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal("run admitted workflow batch:", err)
	}
	if got := admission.calls.Load(); got != 3 {
		t.Fatalf("admission calls = %d, want claim batch plus two steps", got)
	}
	if got := admission.releases.Load(); got != 3 {
		t.Fatalf("released permits = %d, want all three", got)
	}
}

func TestExecutionWorkerDoesNotClaimWithoutAdmission(t *testing.T) {
	store := &workerTestStore{claims: []Claim{{InstanceID: "instance-1", StepID: "one"}}}
	admission := &workflowWorkAdmission{failAt: 1}
	config := validWorkerConfig()
	config.WorkAdmission = admission
	worker, err := NewExecutionWorker(store, ClaimHandlerFunc(func(context.Context, Claim) error { return nil }), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce succeeded without claim admission")
	}
	if store.claimCalls.Load() != 0 || admission.releases.Load() != 0 {
		t.Fatalf("claim/admission effects = %d/%d, want none", store.claimCalls.Load(), admission.releases.Load())
	}
}

func TestExecutionWorkerCancellationJoinsHandlerAndReleasesClaim(t *testing.T) {
	store := &workerTestStore{claims: []Claim{{InstanceID: "instance-1", StepID: "one"}}}
	started, finished := make(chan struct{}), make(chan struct{})
	worker, err := NewExecutionWorker(store, ClaimHandlerFunc(func(ctx context.Context, _ Claim) error {
		close(started)
		defer close(finished)
		<-ctx.Done()
		return ctx.Err()
	}), validWorkerConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.RunOnce(ctx) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunOnce did not join its cancelled handler")
	}
	select {
	case <-finished:
	default:
		t.Fatal("RunOnce returned before the handler exited")
	}
	if store.releaseCall.Load() != 1 {
		t.Fatalf("cancelled claim release calls=%d, want 1", store.releaseCall.Load())
	}
}

func TestExecutionWorkerRenewalFailureCancelsAndJoinsHandler(t *testing.T) {
	store := &workerTestStore{claims: []Claim{{InstanceID: "instance-1", StepID: "one"}}, renewErr: errors.New("lease lost")}
	started, finished := make(chan struct{}), make(chan struct{})
	config := validWorkerConfig()
	config.Lease = time.Second
	worker, err := NewExecutionWorker(store, ClaimHandlerFunc(func(ctx context.Context, _ Claim) error {
		close(started)
		defer close(finished)
		<-ctx.Done()
		return ctx.Err()
	}), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.RunOnce(context.Background()) }()
	<-started
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("lease renewal failure was hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("renewal failure did not cancel and join the handler")
	}
	select {
	case <-finished:
	default:
		t.Fatal("worker returned before handler stopped")
	}
	if store.renewCalls.Load() == 0 || store.releaseCall.Load() != 1 {
		t.Fatalf("renew/release calls=%d/%d", store.renewCalls.Load(), store.releaseCall.Load())
	}
}
