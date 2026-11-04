package tenantruntime

import (
	"context"
	"errors"
	"sync"
)

var ErrGenerationNotRetired = errors.New("tenant runtime generation is still active")

// Generation owns one immutable tenant runtime lifetime: request handles and
// background workers are retired together, while resource close stays with
// the caller that owns the surrounding runtime snapshot.
type Generation struct {
	id      uint64
	workers *Workers

	mu      sync.Mutex
	refs    uint64
	retired bool
	drained chan struct{}
	idle    chan struct{}
}

func NewGeneration(id uint64, parent context.Context) *Generation {
	idle := make(chan struct{})
	close(idle)
	return &Generation{id: id, workers: NewWorkers(parent), drained: make(chan struct{}), idle: idle}
}

// Acquire pins this generation for one request or short-lived operation.
func (g *Generation) Acquire() (*Handle, bool) {
	if g == nil {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return nil, false
	}
	if g.refs == 0 {
		g.idle = make(chan struct{})
	}
	g.refs++
	return &Handle{generation: g}, true
}

// WaitIdle waits for all currently admitted request handles to release. It is
// used after policy stops new admissions but before destructive tenant work.
func (g *Generation) WaitIdle(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	idle := g.idle
	g.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RetireIfIdle atomically prevents new handles and cancels workers only when
// the generation currently has no admitted request or short-lived operation.
// The runtime manager calls it while holding its publication lock.
func (g *Generation) RetireIfIdle() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	if g.retired || g.refs != 0 {
		g.mu.Unlock()
		return false
	}
	g.retired = true
	close(g.drained)
	g.mu.Unlock()
	g.workers.Cancel()
	return true
}

// Retire prevents new handles and closes the drain signal after current users
// release their handles. Worker cancellation/join remains an explicit step.
func (g *Generation) Retire() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if !g.retired {
		g.retired = true
		if g.refs == 0 {
			close(g.drained)
		}
	}
	g.mu.Unlock()
	g.workers.Cancel()
}

func (g *Generation) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		return context.Canceled
	}
	g.mu.Lock()
	if !g.retired {
		g.mu.Unlock()
		return ErrGenerationNotRetired
	}
	drained := g.drained
	g.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *Generation) Drained() <-chan struct{} {
	if g == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return g.drained
}

func (g *Generation) WorkerContext() context.Context {
	if g == nil {
		return context.Background()
	}
	return g.workers.Context()
}

func (g *Generation) StartWorker(run func(context.Context)) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired {
		return false
	}
	return g.workers.Go(run)
}

func (g *Generation) StopWorkers() {
	_ = g.StopWorkersContext(context.Background())
}

func (g *Generation) StopWorkersContext(ctx context.Context) error {
	if g != nil {
		return g.workers.StopContext(ctx)
	}
	return nil
}

func (g *Generation) ID() uint64 {
	if g == nil {
		return 0
	}
	return g.id
}

// Handle pins one generation until Release. Release is safe to call repeatedly.
type Handle struct {
	generation *Generation
	snapshot   *Snapshot
	once       sync.Once
}

// Snapshot returns the immutable activation captured when the handle was acquired.
func (h *Handle) Snapshot() *Snapshot {
	if h == nil {
		return nil
	}
	return h.snapshot
}

func (h *Handle) Generation() uint64 {
	if h == nil || h.generation == nil {
		return 0
	}
	return h.generation.ID()
}

func (h *Handle) Release() {
	if h == nil || h.generation == nil {
		return
	}
	h.once.Do(func() {
		g := h.generation
		g.mu.Lock()
		if g.refs > 0 {
			g.refs--
		}
		if g.refs == 0 {
			close(g.idle)
		}
		if g.retired && g.refs == 0 {
			close(g.drained)
		}
		g.mu.Unlock()
	})
}
