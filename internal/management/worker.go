package management

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type OperationExecutor interface {
	// Execute receives the fenced claim so every durable effect can validate the
	// same operation identity and lease generation. A process can crash after an
	// effect and before Finish commits, so effects must remain idempotent.
	Execute(context.Context, Claim) (FinishResult, error)
}

type WorkerConfig struct {
	Owner       string
	Deployment  contract.DeploymentID
	Concurrency uint16
	Lease       time.Duration
	Poll        time.Duration
}

type Worker struct {
	store    Store
	executor OperationExecutor
	logger   *slog.Logger
}

func NewWorker(store Store, executor OperationExecutor, logger *slog.Logger) (*Worker, error) {
	if store == nil || executor == nil {
		return nil, fmt.Errorf("management worker requires store and executor")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{store: store, executor: executor, logger: logger}, nil
}

// Run polls the durable journal, executes a bounded batch, and joins every
// operation and lease-renewal goroutine before returning. Expired claims are
// reclaimable by another process; the stable operation ID lets handlers make
// repeated side effects idempotent or reconcile them after restart.
func (w *Worker) Run(ctx context.Context, config WorkerConfig) error {
	if w == nil || w.store == nil || w.executor == nil {
		return fmt.Errorf("management worker is not configured")
	}
	if ctx == nil {
		return fmt.Errorf("management worker requires a context")
	}
	if config.Owner == "" || len(config.Owner) > 160 || config.Concurrency == 0 || config.Concurrency > 100 || config.Lease < 3*time.Second || config.Lease > 5*time.Minute || config.Poll < 10*time.Millisecond || config.Poll > time.Minute {
		return fmt.Errorf("management worker owner, concurrency, lease, or poll interval is invalid")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		var claims []Claim
		var err error
		if config.Deployment != "" {
			scoped, ok := w.store.(DeploymentClaimStore)
			if !ok {
				return fmt.Errorf("management store does not support deployment-scoped claims")
			}
			claims, err = scoped.ClaimDueForDeployment(ctx, config.Owner, config.Concurrency, config.Lease, config.Deployment)
		} else {
			claims, err = w.store.ClaimDue(ctx, config.Owner, config.Concurrency, config.Lease)
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.logger.ErrorContext(ctx, "claim management operations", "error", err)
		} else if len(claims) > 0 {
			var joined sync.WaitGroup
			for _, claim := range claims {
				joined.Add(1)
				go func(claim Claim) {
					defer joined.Done()
					if err := w.process(ctx, claim, config.Lease); err != nil {
						w.logger.ErrorContext(ctx, "execute management operation", "operation_id", claim.Operation.ID, "error", err)
					}
				}(claim)
			}
			joined.Wait()
			continue
		}
		timer := time.NewTimer(config.Poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (w *Worker) process(parent context.Context, claim Claim, lease time.Duration) error {
	if parent.Err() != nil {
		ctx, cancel := detachedTimeout(parent, 2*time.Second)
		defer cancel()
		return w.store.Release(ctx, claim)
	}
	execCtx, cancelExec := context.WithCancel(parent)
	// A claim can expire between ClaimDue returning and this goroutine starting.
	// Revalidate it before exposing the command to an executor so an already
	// stale owner cannot begin a side effect. The renewal loop below keeps the
	// lease current while execution is in progress.
	current, err := w.store.Renew(execCtx, claim, lease)
	if err != nil {
		cancelExec()
		return fmt.Errorf("validate management operation claim before execution: %w", err)
	}
	renewal := startLeaseRenewal(execCtx, w.store, current, lease, cancelExec)
	result, executeErr := w.executor.Execute(execCtx, current)
	// Stop renewing before cancelling execution so a renewal already in flight
	// completes instead of being aborted and mistaken for a lost lease.
	current, renewErr := renewal.stop()
	cancelExec()
	if renewErr != nil {
		executeErr = errors.Join(executeErr, renewErr)
	}

	result = normalizeFinishResult(result, executeErr)
	finishCtx, cancelFinish := detachedTimeout(parent, 5*time.Second)
	defer cancelFinish()
	if err := w.store.Finish(finishCtx, current, result); err != nil {
		return errors.Join(executeErr, err)
	}
	return executeErr
}

// leaseRenewal keeps a claim's lease current while its executor runs. A
// renewal failure is fatal only while execution is still running, when it also
// cancels the execution. After execution returns, the outcome is known and
// Finish is fenced by the same lease, so a late renewal failure is not recorded.
type leaseRenewal struct {
	store   Store
	lease   time.Duration
	cancel  context.CancelFunc
	halt    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	claim   Claim
	running bool
	err     error
}

func startLeaseRenewal(ctx context.Context, store Store, claim Claim, lease time.Duration, cancel context.CancelFunc) *leaseRenewal {
	r := &leaseRenewal{store: store, lease: lease, cancel: cancel, halt: make(chan struct{}), done: make(chan struct{}), claim: claim, running: true}
	go r.loop(ctx)
	return r
}

func (r *leaseRenewal) loop(ctx context.Context) {
	defer close(r.done)
	ticker := time.NewTicker(r.lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-r.halt:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case <-r.halt:
				return
			default:
			}
			if !r.renew(ctx) {
				return
			}
		}
	}
}

func (r *leaseRenewal) renew(ctx context.Context) bool {
	r.mu.Lock()
	current := r.claim
	r.mu.Unlock()
	renewed, err := r.store.Renew(ctx, current, r.lease)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.claim = renewed
		return true
	}
	if r.running {
		r.err = err
		r.cancel()
	}
	return false
}

// stop marks execution finished, waits for an in-flight renewal to complete,
// and returns the latest claim with any renewal failure seen during execution.
func (r *leaseRenewal) stop() (Claim, error) {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
	close(r.halt)
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claim, r.err
}

func normalizeFinishResult(result FinishResult, executeErr error) FinishResult {
	// Error strings from providers often contain URLs, credentials, or request
	// bodies. Keep the full error in the Engine log, but persist only a safe,
	// stable message visible through the management operation API.
	recovery := func(message string) FinishResult {
		return FinishResult{State: contract.OperationRecoveryRequired, Error: &contract.Error{Type: contract.CodeInternal, Message: message}}
	}
	if executeErr != nil {
		return recovery("operation outcome is uncertain; inspect Engine logs using the operation ID and reconcile before retrying")
	}
	switch result.State {
	case contract.OperationSucceeded:
		if result.Error != nil {
			return recovery("operation executor returned an invalid success result; operator recovery is required")
		}
	case contract.OperationFailed, contract.OperationRecoveryRequired:
		if result.Error == nil || strings.TrimSpace(result.Error.Message) == "" || len(result.Error.Message) > 8192 {
			return recovery("operation executor returned an invalid failure result; operator recovery is required")
		}
	default:
		return recovery("operation executor returned an invalid terminal state; operator recovery is required")
	}
	if result.ObservedRevision != "" {
		if _, err := contract.ParseRevision(string(result.ObservedRevision)); err != nil {
			return recovery("operation executor returned an invalid observed revision; operator recovery is required")
		}
	}
	return result
}

func detachedTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
