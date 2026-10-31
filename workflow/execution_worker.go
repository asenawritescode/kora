package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

const (
	maxExecutionWorkerSlots = 64
	maxExecutionWorkerBatch = 64
	workerCleanupTimeout    = 2 * time.Second
)

// ClaimHandler executes one claimed workflow step and returns only after its
// fenced transition (or transaction-bound local effect) has committed.
type ClaimHandler interface {
	HandleClaim(context.Context, Claim) error
}

type ClaimHandlerFunc func(context.Context, Claim) error

func (f ClaimHandlerFunc) HandleClaim(ctx context.Context, claim Claim) error {
	return f(ctx, claim)
}

type ExecutionWorkerConfig struct {
	Tenant        contract.TenantID
	Owner         string
	Concurrency   uint16
	Batch         uint16
	Lease         time.Duration
	PollInterval  time.Duration
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
}

// ExecutionWorker owns a bounded tenant claim loop. It is not itself a
// business executor; the handler must perform exactly one step and commit it
// through the fenced transaction boundary before returning nil.
type ExecutionWorker struct {
	store   ExecutionStore
	handler ClaimHandler
	config  ExecutionWorkerConfig
	onError func(error)
}

func NewExecutionWorker(store ExecutionStore, handler ClaimHandler, config ExecutionWorkerConfig, onError func(error)) (*ExecutionWorker, error) {
	if store == nil || handler == nil {
		return nil, fmt.Errorf("workflow execution worker requires a store and claim handler")
	}
	if _, err := contract.ParseTenantID(string(config.Tenant)); err != nil {
		return nil, fmt.Errorf("workflow execution worker tenant: %w", err)
	}
	if config.Owner == "" || len(config.Owner) > 160 {
		return nil, fmt.Errorf("workflow execution worker owner must be 1 to 160 bytes")
	}
	if config.Concurrency == 0 || config.Concurrency > maxExecutionWorkerSlots {
		return nil, fmt.Errorf("workflow execution worker concurrency must be between 1 and %d", maxExecutionWorkerSlots)
	}
	if config.Batch == 0 || config.Batch > maxExecutionWorkerBatch || config.Batch > config.Concurrency {
		return nil, fmt.Errorf("workflow execution worker batch must be between 1 and concurrency (maximum %d)", maxExecutionWorkerBatch)
	}
	if config.Lease < time.Second || config.Lease > 5*time.Minute {
		return nil, fmt.Errorf("workflow execution worker lease must be between 1 second and 5 minutes")
	}
	if config.PollInterval < 100*time.Millisecond || config.PollInterval > time.Hour {
		return nil, fmt.Errorf("workflow execution worker poll interval must be from 100ms to 1h")
	}
	return &ExecutionWorker{store: store, handler: handler, config: config, onError: onError}, nil
}

// Run polls immediately and then at a fixed bounded interval. Cancellation
// waits for the active batch and each claim's lease-renewal loop to exit.
func (w *ExecutionWorker) Run(ctx context.Context) error {
	if w == nil || ctx == nil {
		return fmt.Errorf("workflow execution worker requires a context")
	}
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			if w.onError != nil {
				w.onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RunOnce claims no more work than the worker can execute concurrently. It
// returns only after all handlers and lease-renewal goroutines have joined.
func (w *ExecutionWorker) RunOnce(ctx context.Context) error {
	if w == nil || w.store == nil || w.handler == nil || ctx == nil {
		return fmt.Errorf("workflow execution worker is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	claimCtx, releaseClaim, err := w.acquire(ctx)
	if err != nil {
		return fmt.Errorf("admit workflow claim batch: %w", err)
	}
	claims, err := w.store.ClaimDue(claimCtx, w.config.Tenant, w.config.Owner, w.config.Batch, w.config.Lease)
	releaseClaim()
	if err != nil {
		return fmt.Errorf("claim due workflow steps: %w", err)
	}
	if len(claims) > int(w.config.Concurrency) {
		return fmt.Errorf("workflow store returned %d claims above configured concurrency %d", len(claims), w.config.Concurrency)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, len(claims))
	for _, claim := range claims {
		claim := claim
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.runClaim(ctx, claim); err != nil && ctx.Err() == nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var joined []error
	for err := range errCh {
		joined = append(joined, err)
	}
	return errors.Join(joined...)
}

func (w *ExecutionWorker) runClaim(ctx context.Context, claim Claim) error {
	workCtx, releaseWork, err := w.acquire(ctx)
	if err != nil {
		return fmt.Errorf("admit workflow step %s/%s: %w", claim.InstanceID, claim.StepID, err)
	}
	defer releaseWork()
	stepCtx, cancel := context.WithCancel(workCtx)
	renewDone := make(chan error, 1)
	go w.renewUntilDone(stepCtx, cancel, claim, renewDone)

	handlerErr := w.handler.HandleClaim(stepCtx, claim)
	cancel()
	renewErr := <-renewDone
	if ctx.Err() != nil {
		handlerErr = errors.Join(handlerErr, ctx.Err())
	}
	if handlerErr != nil || renewErr != nil {
		if renewErr != nil {
			handlerErr = errors.Join(handlerErr, fmt.Errorf("workflow lease renewal: %w", renewErr))
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), workerCleanupTimeout)
		cleanupAdmittedCtx, cleanupRelease := dbaccess.AttachWorkAdmission(cleanupCtx, nil)
		releaseErr := w.store.Release(cleanupAdmittedCtx, claim)
		cleanupRelease()
		cleanupCancel()
		if releaseErr != nil {
			handlerErr = errors.Join(handlerErr, fmt.Errorf("release workflow claim: %w", releaseErr))
		}
		return fmt.Errorf("execute workflow claim %s/%s: %w", claim.InstanceID, claim.StepID, handlerErr)
	}
	return nil
}

func (w *ExecutionWorker) acquire(ctx context.Context) (context.Context, func(), error) {
	if w.config.WorkAdmission == nil {
		return ctx, func() {}, nil
	}
	return dbaccess.AcquireWorkAdmission(ctx, w.config.WorkAdmission)
}

func (w *ExecutionWorker) renewUntilDone(ctx context.Context, cancel context.CancelFunc, claim Claim, done chan<- error) {
	interval := w.config.Lease / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewCtx, renewCancel := context.WithTimeout(ctx, interval)
			_, err := w.store.Renew(renewCtx, claim, w.config.Lease)
			renewCancel()
			if err != nil {
				cancel()
				done <- err
				return
			}
		}
	}
}
