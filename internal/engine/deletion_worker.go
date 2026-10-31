package engine

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/site"
)

const pendingDeletionRetryInterval = 30 * time.Second

// PendingDeletionWorker owns recovery of durable tenant-deletion intents.
// Construction starts the cancellable worker; Start releases its initial wait
// after the Engine becomes ready. Shutdown cancels and joins it before the
// platform database is closed.
type PendingDeletionWorker struct {
	startOnce sync.Once
	startCh   chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
	attempts  atomic.Uint64
	failures  atomic.Uint64
	rejected  atomic.Uint64
}

type PendingDeletionWorkerSnapshot struct {
	Attempts            uint64
	Failures            uint64
	AdmissionRejections uint64
}

func (w *PendingDeletionWorker) Snapshot() PendingDeletionWorkerSnapshot {
	if w == nil {
		return PendingDeletionWorkerSnapshot{}
	}
	return PendingDeletionWorkerSnapshot{
		Attempts: w.attempts.Load(), Failures: w.failures.Load(),
		AdmissionRejections: w.rejected.Load(),
	}
}

type deletionWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

// StartPendingDeletionWorker starts the Engine-owned deletion recovery worker.
func StartPendingDeletionWorker(parent context.Context, platformDB *sql.DB, dbType string, common *site.CommonConfig) (*PendingDeletionWorker, error) {
	return startPendingDeletionWorker(parent, platformDB, dbType, common, site.RetryPendingSiteDeletions, pendingDeletionRetryInterval, nil)
}

func startPendingDeletionWorker(parent context.Context, platformDB *sql.DB, dbType string, common *site.CommonConfig, retry func(context.Context, *sql.DB, string, *site.CommonConfig) error, interval time.Duration, admissions ...deletionWorkAdmission) (*PendingDeletionWorker, error) {
	if parent == nil {
		return nil, errors.New("pending deletion worker context is required")
	}
	if platformDB == nil {
		return nil, errors.New("pending deletion worker platform database is required")
	}
	if retry == nil || interval <= 0 {
		return nil, errors.New("pending deletion worker retry function and positive interval are required")
	}
	ctx, cancel := context.WithCancel(parent)
	worker := &PendingDeletionWorker{startCh: make(chan struct{}), cancel: cancel, done: make(chan struct{})}
	var admission deletionWorkAdmission
	if len(admissions) > 0 {
		admission = admissions[0]
	}
	go worker.run(ctx, platformDB, dbType, common, retry, interval, admission)
	return worker, nil
}

func (w *PendingDeletionWorker) run(ctx context.Context, platformDB *sql.DB, dbType string, common *site.CommonConfig, retry func(context.Context, *sql.DB, string, *site.CommonConfig) error, interval time.Duration, admission deletionWorkAdmission) {
	defer close(w.done)
	select {
	case <-ctx.Done():
		return
	case <-w.startCh:
	}
	for {
		w.retry(ctx, platformDB, dbType, common, retry, admission)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (w *PendingDeletionWorker) retry(ctx context.Context, platformDB *sql.DB, dbType string, common *site.CommonConfig, retry func(context.Context, *sql.DB, string, *site.CommonConfig) error, admission deletionWorkAdmission) {
	retryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if admission != nil {
		release, err := admission.Acquire(retryCtx)
		if err != nil {
			if ctx.Err() == nil {
				w.rejected.Add(1)
				slog.Warn("pending site deletion cleanup deferred by database work admission", "error", err)
			}
			return
		}
		defer release()
	}
	w.attempts.Add(1)
	if err := retry(retryCtx, platformDB, dbType, common); err != nil {
		w.failures.Add(1)
		if ctx.Err() == nil {
			slog.Warn("pending site deletion cleanup will retry", "error", err)
		}
	}
}

// Start releases the initial readiness gate exactly once.
func (w *PendingDeletionWorker) Start() {
	if w == nil {
		return
	}
	w.startOnce.Do(func() { close(w.startCh) })
}

// Shutdown cancels the current recovery attempt and joins the worker.
func (w *PendingDeletionWorker) Shutdown(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.cancel()
	if err := waitForWorker(ctx, "site deletion recovery worker", w.done); err != nil {
		return err
	}
	return nil
}
