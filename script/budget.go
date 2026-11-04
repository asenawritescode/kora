package script

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrTenantCapacity = errors.New("script tenant concurrency capacity exhausted")

type BudgetConfig struct {
	PerTenant      int
	MaxWaiters     int
	MaxTenants     int
	Wait           time.Duration
	DefaultTimeout time.Duration
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{PerTenant: 2, MaxWaiters: 4, MaxTenants: 256, Wait: time.Second, DefaultTimeout: 5 * time.Second}
}

type tenantScriptGate struct {
	permits chan struct{}
	waiters int
	refs    int
}

type BudgetSnapshot struct {
	Active           int
	Waiters          int
	ActiveTenants    int
	MaxActiveTenants int
}

// BudgetedRunner applies tenant-scoped execution admission before delegating
// to a process-wide runner. The active/waiting gate map retains entries only
// while executions reference them and is itself capped.
type BudgetedRunner struct {
	base      Runner
	cfg       BudgetConfig
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	gates     map[string]*tenantScriptGate
	active    sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func NewBudgetedRunner(base Runner, cfg BudgetConfig) (*BudgetedRunner, error) {
	if base == nil {
		return nil, errors.New("script runner is required")
	}
	if cfg.PerTenant <= 0 || cfg.MaxWaiters < 0 || cfg.MaxTenants <= 0 || cfg.Wait <= 0 || cfg.DefaultTimeout <= 0 {
		return nil, errors.New("script budget limits must be positive and waiter count cannot be negative")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &BudgetedRunner{base: base, cfg: cfg, ctx: ctx, cancel: cancel, gates: make(map[string]*tenantScriptGate), closeDone: make(chan struct{})}, nil
}

func (r *BudgetedRunner) Execute(ctx context.Context, request ExecuteRequest) (*ExecuteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := strings.TrimSpace(request.Site)
	if key == "" {
		key = "_unspecified"
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("script runner is closed")
	}
	gate := r.gates[key]
	if gate == nil {
		if len(r.gates) >= r.cfg.MaxTenants {
			r.mu.Unlock()
			return nil, ErrTenantCapacity
		}
		gate = &tenantScriptGate{permits: make(chan struct{}, r.cfg.PerTenant)}
		r.gates[key] = gate
	}
	r.active.Add(1)
	gate.refs++
	select {
	case gate.permits <- struct{}{}:
		r.mu.Unlock()
	default:
		if gate.waiters >= r.cfg.MaxWaiters {
			gate.refs--
			r.cleanupGateLocked(key, gate)
			r.active.Done()
			r.mu.Unlock()
			return nil, ErrTenantCapacity
		}
		gate.waiters++
		r.mu.Unlock()
		timer := time.NewTimer(r.cfg.Wait)
		select {
		case gate.permits <- struct{}{}:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			r.releaseWaiter(key, gate, false)
			return nil, ctx.Err()
		case <-r.ctx.Done():
			timer.Stop()
			r.releaseWaiter(key, gate, false)
			return nil, errors.New("script runner is closed")
		case <-timer.C:
			r.releaseWaiter(key, gate, false)
			return nil, ErrTenantCapacity
		}
		r.mu.Lock()
		gate.waiters--
		r.mu.Unlock()
	}
	if request.Timeout <= 0 || request.Timeout > r.cfg.DefaultTimeout {
		request.Timeout = r.cfg.DefaultTimeout
	}
	defer func() {
		r.mu.Lock()
		<-gate.permits
		gate.refs--
		r.cleanupGateLocked(key, gate)
		r.mu.Unlock()
		r.active.Done()
	}()
	execCtx, cancel := context.WithTimeout(ctx, request.Timeout)
	stopClose := context.AfterFunc(r.ctx, cancel)
	defer func() { stopClose(); cancel() }()
	return r.base.Execute(execCtx, request)
}

func (r *BudgetedRunner) releaseWaiter(key string, gate *tenantScriptGate, acquired bool) {
	r.mu.Lock()
	gate.waiters--
	if acquired {
		<-gate.permits
	}
	gate.refs--
	r.cleanupGateLocked(key, gate)
	r.mu.Unlock()
	r.active.Done()
}

func (r *BudgetedRunner) cleanupGateLocked(key string, gate *tenantScriptGate) {
	if gate.refs == 0 && r.gates[key] == gate {
		delete(r.gates, key)
	}
}

func (r *BudgetedRunner) Validate(source string) error { return r.base.Validate(source) }

func (r *BudgetedRunner) Snapshot() BudgetSnapshot {
	if r == nil {
		return BudgetSnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot := BudgetSnapshot{ActiveTenants: len(r.gates), MaxActiveTenants: r.cfg.MaxTenants}
	for _, gate := range r.gates {
		snapshot.Active += len(gate.permits)
		snapshot.Waiters += gate.waiters
	}
	return snapshot
}

func (r *BudgetedRunner) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.cancel()
		r.mu.Unlock()
		r.active.Wait()
		r.closeErr = r.base.Close()
		close(r.closeDone)
	})
	<-r.closeDone
	return r.closeErr
}
