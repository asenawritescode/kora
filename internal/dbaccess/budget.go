// Package dbaccess bounds physical database connections shared by tenant and
// platform sql.DB pools in one Engine process.
package dbaccess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrExhausted = errors.New("database connection budget exhausted")
)

// Config bounds concurrent physical connections and connector waiters. A
// waiter is rejected when MaxWaiters is reached and times out after Wait.
type Config struct {
	MaxConnections int
	MaxWaiters     int
	Wait           time.Duration
	MaxActiveWork  int
	MaxWorkWaiters int
	WorkWait       time.Duration
	MaxHeavyJobs   int
}

// Snapshot contains bounded-cardinality process-local diagnostics.
type Snapshot struct {
	Capacity          int
	InUse             int
	Waiters           int
	WaitCount         uint64
	WaitDuration      time.Duration
	Rejected          uint64
	OperationCount    uint64
	OperationDuration time.Duration
	LockContention    uint64
	Work              AdmissionSnapshot
}

// Budget is shared by all sql.DB pools that target the same physical server.
type Budget struct {
	capacity       int
	maxQueued      int
	permits        chan struct{}
	wait           time.Duration
	work           *Admission
	mu             sync.Mutex
	waiters        int
	waitCount      atomic.Uint64
	waitNanos      atomic.Uint64
	rejected       atomic.Uint64
	operationCount atomic.Uint64
	operationNanos atomic.Uint64
	lockContention atomic.Uint64
}

func New(cfg Config) (*Budget, error) {
	if cfg.MaxConnections <= 0 {
		return nil, fmt.Errorf("database connection capacity must be positive")
	}
	if cfg.MaxWaiters < 0 {
		return nil, fmt.Errorf("database waiter capacity cannot be negative")
	}
	if cfg.Wait <= 0 {
		return nil, fmt.Errorf("database connection wait must be positive")
	}
	if cfg.MaxActiveWork <= 0 {
		cfg.MaxActiveWork = cfg.MaxConnections
	}
	if cfg.MaxWorkWaiters < 0 {
		return nil, fmt.Errorf("active work waiter capacity cannot be negative")
	}
	if cfg.WorkWait <= 0 {
		cfg.WorkWait = cfg.Wait
	}
	work, err := NewAdmissionWithMaintenance(cfg.MaxActiveWork, cfg.MaxWorkWaiters, cfg.WorkWait, cfg.MaxHeavyJobs)
	if err != nil {
		return nil, err
	}
	return &Budget{capacity: cfg.MaxConnections, maxQueued: cfg.MaxWaiters, permits: make(chan struct{}, cfg.MaxConnections), wait: cfg.Wait, work: work}, nil
}

// Acquire reserves one physical connection slot until the returned release
// function is called. Wait count and duration are both bounded.
func (b *Budget) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case b.permits <- struct{}{}:
		release := b.releaseFunc()
		if err := ctx.Err(); err != nil {
			release()
			b.rejected.Add(1)
			return nil, err
		}
		return release, nil
	default:
	}

	b.mu.Lock()
	if b.waiters >= b.maxQueued {
		b.mu.Unlock()
		b.rejected.Add(1)
		return nil, ErrExhausted
	}
	b.waiters++
	b.mu.Unlock()
	waitStarted := time.Now()
	defer func() {
		b.waitCount.Add(1)
		b.waitNanos.Add(uint64(time.Since(waitStarted).Nanoseconds()))
		b.mu.Lock()
		b.waiters--
		b.mu.Unlock()
	}()

	timer := time.NewTimer(b.wait)
	defer timer.Stop()
	select {
	case b.permits <- struct{}{}:
		release := b.releaseFunc()
		if err := ctx.Err(); err != nil {
			release()
			b.rejected.Add(1)
			return nil, err
		}
		return release, nil
	case <-ctx.Done():
		b.rejected.Add(1)
		return nil, ctx.Err()
	case <-timer.C:
		b.rejected.Add(1)
		return nil, ErrExhausted
	}
}

func (b *Budget) releaseFunc() func() {
	var once sync.Once
	return func() { once.Do(func() { <-b.permits }) }
}

func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	waiters := b.waiters
	b.mu.Unlock()
	return Snapshot{
		Capacity: b.capacity, InUse: len(b.permits), Waiters: waiters,
		WaitCount: b.waitCount.Load(), WaitDuration: time.Duration(b.waitNanos.Load()), Rejected: b.rejected.Load(),
		OperationCount: b.operationCount.Load(), OperationDuration: time.Duration(b.operationNanos.Load()), LockContention: b.lockContention.Load(),
		Work: b.work.Snapshot(),
	}
}

func (b *Budget) recordOperation(started time.Time, err error) {
	if b == nil {
		return
	}
	b.operationCount.Add(1)
	b.operationNanos.Add(uint64(time.Since(started).Nanoseconds()))
	if isLockContention(err) {
		b.lockContention.Add(1)
	}
}

// WorkAdmission returns the active-work limiter associated with this physical
// endpoint's connection allocation.
func (b *Budget) WorkAdmission() *Admission { return b.work }

// ConfigFromEnv derives a per-process share of an optional physical-server
// connection budget. When KORA_DB_SERVER_MAX_CONNECTIONS is configured, the
// reserved connections are removed first and the remainder is divided by the
// declared replica count. Every Engine replica must use the same values.
func ConfigFromEnv() (Config, error) {
	capacity, err := envInt("KORA_DB_CONNECTION_BUDGET", 64)
	if err != nil || capacity <= 0 {
		return Config{}, fmt.Errorf("KORA_DB_CONNECTION_BUDGET must be a positive integer")
	}
	serverMax, err := envInt("KORA_DB_SERVER_MAX_CONNECTIONS", 0)
	if err != nil || serverMax < 0 {
		return Config{}, fmt.Errorf("KORA_DB_SERVER_MAX_CONNECTIONS must be a non-negative integer")
	}
	reserved, err := envInt("KORA_DB_SERVER_RESERVED_CONNECTIONS", 0)
	if err != nil || reserved < 0 {
		return Config{}, fmt.Errorf("KORA_DB_SERVER_RESERVED_CONNECTIONS must be a non-negative integer")
	}
	replicas, err := envInt("KORA_DB_ENGINE_REPLICAS", 1)
	if err != nil || replicas <= 0 {
		return Config{}, fmt.Errorf("KORA_DB_ENGINE_REPLICAS must be a positive integer")
	}
	if serverMax > 0 {
		if reserved >= serverMax {
			return Config{}, fmt.Errorf("KORA_DB_SERVER_RESERVED_CONNECTIONS must be less than KORA_DB_SERVER_MAX_CONNECTIONS")
		}
		allocation := (serverMax - reserved) / replicas
		if allocation <= 0 {
			return Config{}, fmt.Errorf("physical database allocation leaves no connections per Engine replica")
		}
		capacity = min(capacity, allocation)
	}
	waiters, err := envInt("KORA_DB_CONNECTION_WAITERS", capacity*2)
	if err != nil || waiters < 0 {
		return Config{}, fmt.Errorf("KORA_DB_CONNECTION_WAITERS must be a non-negative integer")
	}
	waitMS, err := envInt("KORA_DB_CONNECTION_WAIT_MS", 1500)
	if err != nil || waitMS <= 0 {
		return Config{}, fmt.Errorf("KORA_DB_CONNECTION_WAIT_MS must be a positive integer")
	}
	activeWork, err := envInt("KORA_DB_ACTIVE_WORK_BUDGET", min(capacity, 16))
	if err != nil || activeWork <= 0 {
		return Config{}, fmt.Errorf("KORA_DB_ACTIVE_WORK_BUDGET must be a positive integer")
	}
	workWaiters, err := envInt("KORA_DB_ACTIVE_WORK_WAITERS", activeWork*2)
	if err != nil || workWaiters < 0 {
		return Config{}, fmt.Errorf("KORA_DB_ACTIVE_WORK_WAITERS must be a non-negative integer")
	}
	workWaitMS, err := envInt("KORA_DB_ACTIVE_WORK_WAIT_MS", 1000)
	if err != nil || workWaitMS <= 0 {
		return Config{}, fmt.Errorf("KORA_DB_ACTIVE_WORK_WAIT_MS must be a positive integer")
	}
	heavyDefault := 0
	if activeWork > 1 {
		heavyDefault = replicas
	}
	heavyJobs, err := envInt("KORA_DB_SERVER_MAX_HEAVY_JOBS", heavyDefault)
	if err != nil || heavyJobs < 0 {
		return Config{}, fmt.Errorf("KORA_DB_SERVER_MAX_HEAVY_JOBS must be a non-negative integer")
	}
	perReplicaHeavyJobs := heavyJobs / replicas
	if heavyJobs > 0 && perReplicaHeavyJobs == 0 {
		return Config{}, fmt.Errorf("KORA_DB_SERVER_MAX_HEAVY_JOBS leaves no maintenance capacity per Engine replica")
	}
	if perReplicaHeavyJobs >= activeWork {
		return Config{}, fmt.Errorf("KORA_DB_SERVER_MAX_HEAVY_JOBS must leave ordinary active-work capacity on each replica")
	}
	return Config{
		MaxConnections: capacity, MaxWaiters: waiters, Wait: time.Duration(waitMS) * time.Millisecond,
		MaxActiveWork: activeWork, MaxWorkWaiters: workWaiters, WorkWait: time.Duration(workWaitMS) * time.Millisecond,
		MaxHeavyJobs: perReplicaHeavyJobs,
	}, nil
}

func envInt(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	return strconv.Atoi(raw)
}
