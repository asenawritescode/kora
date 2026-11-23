package dbaccess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBackgroundBusy marks a background pass skipped because no background slot
// was free by its deadline. It is not a failure; the next tick retries.
var ErrBackgroundBusy = errors.New("background database lane is busy")

// IsBackgroundBusy reports whether err is a skipped background pass.
func IsBackgroundBusy(err error) bool { return errors.Is(err, ErrBackgroundBusy) }

type workAdmissionContextKey struct{}

type workAdmissionLease struct{ active atomic.Bool }

// AttachWorkAdmission records an already-acquired unit-of-work permit on its
// context. The returned release is idempotent and marks the context inactive
// before returning the permit.
func AttachWorkAdmission(ctx context.Context, release func()) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	lease := &workAdmissionLease{}
	lease.active.Store(true)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			lease.active.Store(false)
			if release != nil {
				release()
			}
		})
	}
	return context.WithValue(ctx, workAdmissionContextKey{}, lease), finish
}

// HasWorkAdmission reports whether the context is currently covered by an
// active admission permit. A released stream-authentication permit is false.
func HasWorkAdmission(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	lease, ok := ctx.Value(workAdmissionContextKey{}).(*workAdmissionLease)
	return ok && lease != nil && lease.active.Load()
}

// AcquireWorkAdmission reuses a live context permit or acquires exactly one
// permit and attaches it to the returned context.
func AcquireWorkAdmission(ctx context.Context, admission interface {
	Acquire(context.Context) (func(), error)
}) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if HasWorkAdmission(ctx) {
		return ctx, func() {}, nil
	}
	if admission == nil {
		return nil, nil, errors.New("tenant work admission is required")
	}
	release, err := admission.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	admitted, finish := AttachWorkAdmission(ctx, release)
	return admitted, finish, nil
}

// BeginAdmittedTx acquires or reuses one tenant work permit and holds it until
// the caller releases the returned callback after transaction completion.
func BeginAdmittedTx(ctx context.Context, database *sql.DB, admission interface {
	Acquire(context.Context) (func(), error)
}) (*sql.Tx, func(), error) {
	if database == nil {
		return nil, nil, errors.New("tenant database is required")
	}
	admittedContext, release, err := AcquireWorkAdmission(ctx, admission)
	if err != nil {
		return nil, nil, err
	}
	tx, err := database.BeginTx(admittedContext, nil)
	if err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}

// AdmissionSnapshot reports active and queued unit-of-work counts.
type AdmissionSnapshot struct {
	Capacity            int
	Active              int
	MaintenanceActive   int
	MaintenanceCapacity int
	Waiters             int
	WaitCount           uint64
	WaitDuration        time.Duration
	Rejected            uint64
	BackgroundActive    int
	BackgroundCapacity  int
	BackgroundWaiters   int
	BackgroundSkipped   uint64
}

// Admission independently limits active work. It is intentionally separate
// from physical connection permits: one admitted operation can perform nested
// queries or reuse a transaction without acquiring another permit.
type Admission struct {
	capacity    int
	maxQueued   int
	wait        time.Duration
	ordinary    chan struct{}
	maintenance chan struct{}
	background  chan struct{}
	lane        *BackgroundLane
	mu          sync.Mutex
	waiters     int
	bgWaiters   int
	waitCount   atomic.Uint64
	waitNanos   atomic.Uint64
	rejected    atomic.Uint64
	bgSkipped   atomic.Uint64
}

// BackgroundLane is the admission view for background workers; its slots never
// block request admission.
type BackgroundLane struct{ admission *Admission }

// Background returns the background lane; nil for a nil admission.
func (a *Admission) Background() *BackgroundLane {
	if a == nil {
		return nil
	}
	return a.lane
}

// Acquire waits for a slot until the context deadline (or the admission wait
// when there is none) and returns ErrBackgroundBusy when the pass is skipped.
func (l *BackgroundLane) Acquire(ctx context.Context) (func(), error) {
	if l == nil || l.admission == nil {
		return nil, errors.New("background database lane is required")
	}
	return l.admission.acquireBackground(ctx)
}

// backgroundLaneCapacity is max(2, budget/4), leaving at least one ordinary slot.
func backgroundLaneCapacity(capacity, maintenanceCapacity int) int {
	return max(0, min(max(2, capacity/4), capacity-maintenanceCapacity-1))
}

func NewAdmission(capacity, maxWaiters int, wait time.Duration) (*Admission, error) {
	return NewAdmissionWithMaintenance(capacity, maxWaiters, wait, 0)
}

// NewAdmissionWithMaintenance reserves a separate bounded lane for heavy
// maintenance work. Ordinary work cannot consume the reserved maintenance
// slot, and maintenance concurrency cannot exceed maintenanceCapacity.
func NewAdmissionWithMaintenance(capacity, maxWaiters int, wait time.Duration, maintenanceCapacity int) (*Admission, error) {
	if capacity <= 0 {
		return nil, errors.New("active work capacity must be positive")
	}
	if maxWaiters < 0 {
		return nil, errors.New("active work waiter capacity cannot be negative")
	}
	if wait <= 0 {
		return nil, errors.New("active work wait must be positive")
	}
	if maintenanceCapacity < 0 || maintenanceCapacity >= capacity {
		return nil, errors.New("maintenance work capacity must be non-negative and leave ordinary active work capacity")
	}
	backgroundCapacity := backgroundLaneCapacity(capacity, maintenanceCapacity)
	ordinaryCapacity := capacity - maintenanceCapacity - backgroundCapacity
	a := &Admission{
		capacity: capacity, maxQueued: maxWaiters, wait: wait,
		ordinary: make(chan struct{}, ordinaryCapacity), maintenance: make(chan struct{}, maintenanceCapacity),
		background: make(chan struct{}, backgroundCapacity),
	}
	a.lane = &BackgroundLane{admission: a}
	return a, nil
}

// Acquire reserves one work slot. Always release after the complete unit of
// work, including its transaction commit or rollback.
func (a *Admission) Acquire(ctx context.Context) (func(), error) {
	return a.acquire(ctx, a.ordinary)
}

// AcquireMaintenance admits a heavy maintenance unit in the separately
// bounded lane. A zero-capacity lane rejects maintenance rather than allowing
// unbounded fan-out through the ordinary lane.
func (a *Admission) AcquireMaintenance(ctx context.Context) (func(), error) {
	if cap(a.maintenance) == 0 {
		a.rejected.Add(1)
		return nil, ErrExhausted
	}
	return a.acquire(ctx, a.maintenance)
}

func (a *Admission) acquireBackground(ctx context.Context) (func(), error) {
	select {
	case a.background <- struct{}{}:
		return a.releaseFunc(a.background), nil
	default:
	}
	a.mu.Lock()
	if a.bgWaiters >= a.maxQueued {
		a.mu.Unlock()
		return nil, a.skipBackground(nil)
	}
	a.bgWaiters++
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.bgWaiters--
		a.mu.Unlock()
	}()
	var expired <-chan time.Time
	if _, bounded := ctx.Deadline(); !bounded {
		timer := time.NewTimer(a.wait)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case a.background <- struct{}{}:
		release := a.releaseFunc(a.background)
		if err := ctx.Err(); err != nil {
			release()
			return nil, a.skipBackground(err)
		}
		return release, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, a.skipBackground(ctx.Err())
		}
		return nil, ctx.Err()
	case <-expired:
		return nil, a.skipBackground(nil)
	}
}

// skipBackground counts a skipped pass and wraps ErrBackgroundBusy with cause.
func (a *Admission) skipBackground(cause error) error {
	a.bgSkipped.Add(1)
	slog.Debug("background database lane busy; pass skipped")
	if cause == nil {
		return ErrBackgroundBusy
	}
	return fmt.Errorf("%w: %w", ErrBackgroundBusy, cause)
}

func (a *Admission) acquire(ctx context.Context, lane chan struct{}) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case lane <- struct{}{}:
		release := a.releaseFunc(lane)
		if err := ctx.Err(); err != nil {
			release()
			a.rejected.Add(1)
			return nil, err
		}
		return release, nil
	default:
	}
	a.mu.Lock()
	if a.waiters >= a.maxQueued {
		a.mu.Unlock()
		a.rejected.Add(1)
		return nil, ErrExhausted
	}
	a.waiters++
	a.mu.Unlock()
	started := time.Now()
	defer func() {
		a.waitCount.Add(1)
		a.waitNanos.Add(uint64(time.Since(started).Nanoseconds()))
		a.mu.Lock()
		a.waiters--
		a.mu.Unlock()
	}()
	timer := time.NewTimer(a.wait)
	defer timer.Stop()
	select {
	case lane <- struct{}{}:
		release := a.releaseFunc(lane)
		if err := ctx.Err(); err != nil {
			release()
			a.rejected.Add(1)
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		a.rejected.Add(1)
		return nil, ctx.Err()
	case <-timer.C:
		a.rejected.Add(1)
		return nil, ErrExhausted
	}
}

func (a *Admission) releaseFunc(lane chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { <-lane }) }
}

func (a *Admission) Snapshot() AdmissionSnapshot {
	a.mu.Lock()
	waiters, bgWaiters := a.waiters, a.bgWaiters
	a.mu.Unlock()
	return AdmissionSnapshot{
		Capacity: a.capacity, Active: len(a.ordinary) + len(a.maintenance) + len(a.background),
		MaintenanceActive: len(a.maintenance), MaintenanceCapacity: cap(a.maintenance), Waiters: waiters,
		WaitCount: a.waitCount.Load(), WaitDuration: time.Duration(a.waitNanos.Load()), Rejected: a.rejected.Load(),
		BackgroundActive: len(a.background), BackgroundCapacity: cap(a.background),
		BackgroundWaiters: bgWaiters, BackgroundSkipped: a.bgSkipped.Load(),
	}
}
