package tenantruntime

import (
	"context"
	"fmt"
	"sync"
)

// Workers owns cancellable goroutines for one tenant runtime generation.
// Once Stop begins, no new worker can be admitted and concurrent Stop callers
// all wait for the same generation to drain.
type Workers struct {
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	stopping bool
	joining  bool
	joined   chan struct{}
	workers  []chan struct{}
}

func NewWorkers(parent context.Context) *Workers {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Workers{ctx: ctx, cancel: cancel, joined: make(chan struct{})}
}

func (w *Workers) Context() context.Context {
	if w == nil {
		return context.Background()
	}
	return w.ctx
}

// Go registers the join handle before launching run. It returns false once
// shutdown has started or when no worker function was supplied.
func (w *Workers) Go(run func(context.Context)) bool {
	if w == nil || run == nil {
		return false
	}
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return false
	}
	done := make(chan struct{})
	w.workers = append(w.workers, done)
	ctx := w.ctx
	w.mu.Unlock()
	go func() {
		defer close(done)
		run(ctx)
	}()
	return true
}

// Cancel prevents further admission and cancels every worker context without
// waiting. The lifecycle owner can then stop accepting handles before joining.
func (w *Workers) Cancel() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.stopping {
		w.mu.Unlock()
		return
	}
	w.stopping = true
	w.mu.Unlock()
	w.cancel()
}

// Stop cancels the generation and joins every worker. It is safe for concurrent
// and repeated callers; no caller returns before the first drain has completed.
func (w *Workers) Stop() {
	_ = w.StopContext(context.Background())
}

// StopContext cancels all registered workers and waits for their common join
// signal, bounded by ctx. A timed-out caller can retry after workers exit.
func (w *Workers) StopContext(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if w.joining {
		joined := w.joined
		w.mu.Unlock()
		select {
		case <-joined:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.stopping = true
	w.joining = true
	workers := append([]chan struct{}(nil), w.workers...)
	joined := w.joined
	w.mu.Unlock()
	w.cancel()
	go func() {
		for _, done := range workers {
			<-done
		}
		close(joined)
	}()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("join tenant workers: %w", ctx.Err())
	}
}
