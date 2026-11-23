// Package sweep paces background workers. A worker runs a pass when a
// committed write wakes it, and otherwise on a jittered sweep, so an idle
// tenant costs one bounded query per sweep rather than one per second.
package sweep

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// Wake fans a commit signal out to one tenant's workers. Signal never blocks:
// each subscriber keeps at most one pending wake, so a burst of commits costs
// one extra pass. A nil Wake is a no-op.
type Wake struct {
	mu   sync.Mutex
	subs []chan struct{}
}

// NewWake returns a Wake with no subscribers.
func NewWake() *Wake { return &Wake{} }

// Subscribe returns the channel a worker waits on. Wakes sent before
// Subscribe are not replayed; the sweep covers them.
func (w *Wake) Subscribe() <-chan struct{} {
	if w == nil {
		return nil
	}
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.subs = append(w.subs, ch)
	w.mu.Unlock()
	return ch
}

// Signal wakes every subscriber that does not already have a pending wake.
func (w *Wake) Signal() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Timer schedules one worker's sweeps. Each sweep waits base ±20 % (jittered),
// and the first sweep starts at a random offset in [0, base) so workers that
// start together do not fire together.
type Timer struct {
	base  time.Duration
	wake  <-chan struct{}
	after func(time.Duration) (<-chan time.Time, func())
	rnd   func(time.Duration) time.Duration
	first bool
}

// New returns a Timer for a worker that sweeps every base and wakes on wake
// (nil for sweep-only workers).
func New(base time.Duration, wake <-chan struct{}) *Timer {
	return &Timer{base: base, wake: wake, after: realAfter, rnd: rand.N[time.Duration], first: true}
}

// Jitter returns base ±20 %, uniformly distributed in [0.8·base, 1.2·base).
func Jitter(base time.Duration) time.Duration {
	return base*4/5 + rand.N(base*2/5) //nolint:gosec // scheduling jitter needs no cryptographic randomness
}

func realAfter(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// next returns the delay before the next sweep.
func (t *Timer) next() time.Duration {
	if t.first {
		t.first = false
		return t.rnd(t.base)
	}
	return t.base*4/5 + t.rnd(t.base*2/5)
}

// Wait blocks until a wake arrives or the next sweep is due. It reports false
// when ctx ends first.
func (t *Timer) Wait(ctx context.Context) bool {
	ch, stop := t.after(t.next())
	defer stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.wake:
		return true
	case <-ch:
		return true
	}
}

// Loop runs pass once per wake or sweep until ctx ends. pass reports whether
// its batch was full; a full batch runs pass again at once so a backlog drains
// without waiting for the next sweep.
func Loop(ctx context.Context, t *Timer, pass func(context.Context) bool) {
	for t.Wait(ctx) {
		for full := pass(ctx); full && ctx.Err() == nil; {
			full = pass(ctx)
		}
	}
}
