package management

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// renewGateStore passes the pre-execution claim check, then holds the first
// renewal tick until the test releases it. The held call honours its context
// like a real database round trip.
type renewGateStore struct {
	workerStore
	calls    atomic.Int32
	inFlight chan struct{}
	release  chan struct{}
	err      error
}

func (s *renewGateStore) Renew(ctx context.Context, claim Claim, _ time.Duration) (Claim, error) {
	switch s.calls.Add(1) {
	case 1:
		return claim, nil
	case 2:
		close(s.inFlight)
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return Claim{}, ctx.Err()
	}
	if s.err != nil {
		return Claim{}, s.err
	}
	return claim, nil
}

func TestWorkerLeaseRenewalAroundExecutionEnd(t *testing.T) {
	stale := contract.NewError(contract.CodeConflict, "management operation claim is stale or expired")
	for _, test := range []struct {
		name string
		// releaseFirst lets the renewal tick fail while execution is running.
		releaseFirst bool
		renewErr     error
		want         contract.OperationState
	}{
		{name: "renewal in flight when execution succeeds", want: contract.OperationSucceeded},
		{name: "renewal fails after execution succeeded", renewErr: errors.New("connection reset"), want: contract.OperationSucceeded},
		{name: "renewal fails while executing", releaseFirst: true, renewErr: stale, want: contract.OperationRecoveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := &renewGateStore{inFlight: make(chan struct{}), release: make(chan struct{}), err: test.renewErr}
				if test.releaseFirst {
					close(store.release)
				}
				executor := OperationExecutorFunc(func(ctx context.Context, _ Claim) (FinishResult, error) {
					if test.releaseFirst {
						<-ctx.Done()
						return FinishResult{}, ctx.Err()
					}
					<-store.inFlight
					return FinishResult{State: contract.OperationSucceeded}, nil
				})
				worker, err := NewWorker(store, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				claim := Claim{Operation: Operation{ID: contract.OperationID(contract.NewID()), Tenant: "tenant-1", State: contract.OperationRunning}, Owner: "engine:renew", Fence: 1}
				done := make(chan error, 1)
				go func() { done <- worker.process(context.Background(), claim, 3*time.Second) }()
				if !test.releaseFirst {
					// Execution returns while the renewal tick is held; let the
					// worker settle before the tick is allowed to complete.
					<-store.inFlight
					synctest.Wait()
					close(store.release)
				}
				processErr := <-done
				store.mu.Lock()
				defer store.mu.Unlock()
				if len(store.finished) != 1 || store.finished[0].State != test.want {
					t.Fatalf("finished = %+v (process error %v), want one %s result", store.finished, processErr, test.want)
				}
				if (processErr == nil) != (test.want == contract.OperationSucceeded) {
					t.Fatalf("process error = %v for %s result", processErr, test.want)
				}
			})
		})
	}
}
