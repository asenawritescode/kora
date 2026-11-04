package dbaccess

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

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
	mu          sync.Mutex
	waiters     int
	waitCount   atomic.Uint64
	waitNanos   atomic.Uint64
	rejected    atomic.Uint64
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
	ordinaryCapacity := capacity - maintenanceCapacity
	return &Admission{capacity: capacity, maxQueued: maxWaiters, wait: wait, ordinary: make(chan struct{}, ordinaryCapacity), maintenance: make(chan struct{}, maintenanceCapacity)}, nil
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
	waiters := a.waiters
	a.mu.Unlock()
	return AdmissionSnapshot{
		Capacity: a.capacity, Active: len(a.ordinary) + len(a.maintenance),
		MaintenanceActive: len(a.maintenance), MaintenanceCapacity: cap(a.maintenance), Waiters: waiters,
		WaitCount: a.waitCount.Load(), WaitDuration: time.Duration(a.waitNanos.Load()), Rejected: a.rejected.Load(),
	}
}
