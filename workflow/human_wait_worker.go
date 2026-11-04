package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

const maxHumanWaitRecoveryBatch uint16 = 500

// HumanWaitRecoveryWorker polls durable request state for one loaded tenant.
// Run returns after cancellation and owns no goroutine other than its caller's
// loop; runtime composition is responsible for launching and joining it.
type HumanWaitRecoveryWorker struct {
	store     HumanWaitRecoveryStore
	tenant    contract.TenantID
	batch     uint16
	interval  time.Duration
	onError   func(error)
	admission *dbaccess.Admission
}

func NewHumanWaitRecoveryWorker(store HumanWaitRecoveryStore, tenant contract.TenantID, batch uint16, interval time.Duration, onError func(error), admission ...*dbaccess.Admission) (*HumanWaitRecoveryWorker, error) {
	if store == nil {
		return nil, fmt.Errorf("human wait recovery requires a store")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("human wait recovery tenant: %w", err)
	}
	if batch == 0 || batch > maxHumanWaitRecoveryBatch {
		return nil, fmt.Errorf("human wait recovery batch must be from 1 to %d", maxHumanWaitRecoveryBatch)
	}
	if interval < 100*time.Millisecond || interval > time.Hour {
		return nil, fmt.Errorf("human wait recovery interval must be from 100ms to 1h")
	}
	if len(admission) > 1 {
		return nil, fmt.Errorf("human wait recovery accepts at most one admission controller")
	}
	w := &HumanWaitRecoveryWorker{store: store, tenant: tenant, batch: batch, interval: interval, onError: onError}
	if len(admission) == 1 {
		w.admission = admission[0]
	}
	return w, nil
}

// Run performs one recovery pass immediately, then one bounded pass per tick.
// A failed pass is reported and retried at the next interval; durable request
// state remains available for recovery. Cancellation interrupts the store call.
func (w *HumanWaitRecoveryWorker) Run(ctx context.Context) {
	if w == nil || ctx == nil {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		w.runPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *HumanWaitRecoveryWorker) runPass(ctx context.Context) {
	if w.admission != nil {
		release, err := w.admission.Acquire(ctx)
		if err != nil {
			if ctx.Err() == nil && w.onError != nil {
				w.onError(err)
			}
			return
		}
		defer release()
	}
	if _, err := w.store.ExpireDue(ctx, w.tenant, w.batch); err != nil && ctx.Err() == nil && w.onError != nil {
		w.onError(err)
	}
	if timed, ok := w.store.(TimedWaitRecoveryStore); ok && ctx.Err() == nil {
		if _, err := timed.ResumeDueTimedWaits(ctx, w.tenant, w.batch); err != nil && ctx.Err() == nil && w.onError != nil {
			w.onError(err)
		}
	}
	if signals, ok := w.store.(EventWaitRecoveryStore); ok && ctx.Err() == nil {
		if _, err := signals.ResumeEventSignals(ctx, w.tenant, w.batch); err != nil && ctx.Err() == nil && w.onError != nil {
			w.onError(err)
		}
	}
	if ctx.Err() == nil {
		if _, err := w.store.ResumeTerminalHumanWaits(ctx, w.tenant, w.batch); err != nil && ctx.Err() == nil && w.onError != nil {
			w.onError(err)
		}
	}
}
