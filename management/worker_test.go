package management

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type workerStore struct {
	mu             sync.Mutex
	pending        []Operation
	finished       []FinishResult
	released       int
	renewErr       error
	renewAttempted chan struct{}
}

func (s *workerStore) Accept(context.Context, AcceptCommand) (Operation, error) { panic("not used") }
func (s *workerStore) Get(context.Context, contract.PrincipalID, contract.OperationID) (Operation, error) {
	panic("not used")
}
func (s *workerStore) ClaimDue(_ context.Context, owner string, limit uint16, lease time.Duration) ([]Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if int(limit) > len(s.pending) {
		limit = uint16(len(s.pending))
	}
	claims := make([]Claim, 0, limit)
	for i := uint16(0); i < limit; i++ {
		operation := s.pending[0]
		s.pending = s.pending[1:]
		operation.State = contract.OperationRunning
		claims = append(claims, Claim{Operation: operation, Owner: owner, Fence: 1, LeaseUntil: time.Now().Add(lease)})
	}
	return claims, nil
}
func (s *workerStore) Renew(_ context.Context, claim Claim, lease time.Duration) (Claim, error) {
	if s.renewAttempted != nil {
		select {
		case s.renewAttempted <- struct{}{}:
		default:
		}
	}
	if s.renewErr != nil {
		return Claim{}, s.renewErr
	}
	claim.LeaseUntil = time.Now().Add(lease)
	return claim, nil
}
func (s *workerStore) Release(context.Context, Claim) error {
	s.mu.Lock()
	s.released++
	s.mu.Unlock()
	return nil
}
func (s *workerStore) Finish(_ context.Context, _ Claim, result FinishResult) error {
	s.mu.Lock()
	s.finished = append(s.finished, result)
	s.mu.Unlock()
	return nil
}

func TestWorkerBoundsConcurrencyAndJoinsAllOperationsOnShutdown(t *testing.T) {
	store := &workerStore{pending: make([]Operation, 5)}
	for i := range store.pending {
		store.pending[i] = Operation{ID: contract.OperationID(contract.NewID()), Tenant: "tenant-1", State: contract.OperationRequested}
	}
	var active, maximum atomic.Int32
	processed := make(chan struct{}, 5)
	executor := OperationExecutorFunc(func(ctx context.Context, _ Claim) (FinishResult, error) {
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		select {
		case <-ctx.Done():
			active.Add(-1)
			return FinishResult{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		active.Add(-1)
		processed <- struct{}{}
		return FinishResult{State: contract.OperationSucceeded}, nil
	})
	worker, err := NewWorker(store, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, WorkerConfig{Owner: "engine:1", Concurrency: 2, Lease: 3 * time.Second, Poll: 10 * time.Millisecond})
	}()
	for i := 0; i < 5; i++ {
		select {
		case <-processed:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("worker did not process its durable queue")
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not join on shutdown")
	}
	if maximum.Load() > 2 || active.Load() != 0 {
		t.Fatalf("worker concurrency/join = max %d active %d", maximum.Load(), active.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finished) != 5 {
		t.Fatalf("finished %d operations, want 5", len(store.finished))
	}
}

type OperationExecutorFunc func(context.Context, Claim) (FinishResult, error)

func (f OperationExecutorFunc) Execute(ctx context.Context, claim Claim) (FinishResult, error) {
	return f(ctx, claim)
}

func TestWorkerPassesValidatedFenceToExecutor(t *testing.T) {
	operation := Operation{ID: contract.OperationID(contract.NewID()), Tenant: "tenant-1", State: contract.OperationRequested}
	store := &workerStore{pending: []Operation{operation}}
	seen := make(chan Claim, 1)
	executor := OperationExecutorFunc(func(_ context.Context, claim Claim) (FinishResult, error) {
		seen <- claim
		return FinishResult{State: contract.OperationSucceeded}, nil
	})
	worker, err := NewWorker(store, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, WorkerConfig{Owner: "engine:fenced", Concurrency: 1, Lease: 3 * time.Second, Poll: 10 * time.Millisecond})
	}()
	select {
	case claim := <-seen:
		if claim.Operation.ID != operation.ID || claim.Owner != "engine:fenced" || claim.Fence != 1 || claim.LeaseUntil.IsZero() {
			cancel()
			t.Fatalf("executor received claim %+v", claim)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("executor did not receive validated claim")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not join")
	}
}

func TestWorkerRecordsUnknownOutcomeAndStopsExecutionOnCancellation(t *testing.T) {
	store := &workerStore{pending: []Operation{{ID: contract.OperationID(contract.NewID()), Tenant: "tenant-1", State: contract.OperationRequested}}}
	started := make(chan struct{})
	executor := OperationExecutorFunc(func(ctx context.Context, _ Claim) (FinishResult, error) {
		close(started)
		<-ctx.Done()
		return FinishResult{}, errors.New("provider response was lost")
	})
	worker, err := NewWorker(store, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, WorkerConfig{Owner: "engine:cancel", Concurrency: 1, Lease: 3 * time.Second, Poll: 10 * time.Millisecond})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("executor did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not join cancelled operation")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finished) != 1 || store.finished[0].State != contract.OperationRecoveryRequired || store.finished[0].Error == nil {
		t.Fatalf("unknown outcome was not retained for recovery: %+v", store.finished)
	}
}

func TestWorkerDoesNotExecuteClaimRejectedByInitialRenewal(t *testing.T) {
	store := &workerStore{
		pending:        []Operation{{ID: contract.OperationID(contract.NewID()), Tenant: "tenant-1", State: contract.OperationRequested}},
		renewErr:       contract.NewError(contract.CodeConflict, "management operation claim is stale or expired"),
		renewAttempted: make(chan struct{}, 1),
	}
	var executions atomic.Int32
	executor := OperationExecutorFunc(func(context.Context, Claim) (FinishResult, error) {
		executions.Add(1)
		return FinishResult{State: contract.OperationSucceeded}, nil
	})
	worker, err := NewWorker(store, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, WorkerConfig{Owner: "engine:stale", Concurrency: 1, Lease: 3 * time.Second, Poll: 10 * time.Millisecond})
	}()
	select {
	case <-store.renewAttempted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("worker did not validate the operation claim")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not join after a stale claim")
	}
	if got := executions.Load(); got != 0 {
		t.Fatalf("executor ran %d times with a rejected claim", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.finished) != 0 {
		t.Fatalf("stale claim recorded a finish result: %+v", store.finished)
	}
}

func TestNormalizeFinishResultMakesInvalidExecutorOutputRecoverable(t *testing.T) {
	for _, test := range []struct {
		name   string
		result FinishResult
	}{
		{name: "failure without details", result: FinishResult{State: contract.OperationFailed}},
		{name: "malformed observed revision", result: FinishResult{State: contract.OperationSucceeded, ObservedRevision: "../latest"}},
		{name: "success with error", result: FinishResult{State: contract.OperationSucceeded, Error: &contract.Error{Type: contract.CodeInternal, Message: "bad"}}},
		{name: "nonterminal state", result: FinishResult{State: contract.OperationRunning}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalizeFinishResult(test.result, nil)
			if got.State != contract.OperationRecoveryRequired || got.Error == nil {
				t.Fatalf("normalized result = %+v", got)
			}
		})
	}
}

func TestNormalizeFinishResultDoesNotPersistProviderError(t *testing.T) {
	got := normalizeFinishResult(FinishResult{State: contract.OperationSucceeded}, errors.New("request to https://provider.test/?token=secret failed"))
	if got.State != contract.OperationRecoveryRequired || got.Error == nil {
		t.Fatalf("normalized result = %+v", got)
	}
	if strings.Contains(got.Error.Message, "secret") || strings.Contains(got.Error.Message, "provider.test") {
		t.Fatalf("provider error leaked into persisted outcome: %q", got.Error.Message)
	}
}
