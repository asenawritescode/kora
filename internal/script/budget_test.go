package script

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type budgetTestRunner struct {
	started  chan string
	release  chan struct{}
	timeouts chan time.Duration
	mu       sync.Mutex
	closed   bool
}

func (r *budgetTestRunner) Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	r.started <- req.Site
	if r.timeouts != nil {
		r.timeouts <- req.Timeout
	}
	select {
	case <-r.release:
		return &ExecuteResult{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (*budgetTestRunner) Validate(string) error { return nil }
func (r *budgetTestRunner) Close() error        { r.mu.Lock(); r.closed = true; r.mu.Unlock(); return nil }

func TestBudgetedRunnerBoundsPerTenantAndWaiters(t *testing.T) {
	base := &budgetTestRunner{started: make(chan string, 8), release: make(chan struct{}, 8)}
	budget, err := NewBudgetedRunner(base, BudgetConfig{PerTenant: 1, MaxWaiters: 1, MaxTenants: 4, Wait: time.Second, DefaultTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer budget.Close()
	first := make(chan error, 1)
	go func() { _, err := budget.Execute(context.Background(), ExecuteRequest{Site: "alpha"}); first <- err }()
	if got := <-base.started; got != "alpha" {
		t.Fatalf("first tenant = %q", got)
	}
	second := make(chan error, 1)
	go func() { _, err := budget.Execute(context.Background(), ExecuteRequest{Site: "alpha"}); second <- err }()
	waitForScriptWaiter(t, budget, "alpha", 1)
	if _, err := budget.Execute(context.Background(), ExecuteRequest{Site: "alpha"}); !errors.Is(err, ErrTenantCapacity) {
		t.Fatalf("excess same-tenant execution = %v", err)
	}
	base.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal("first execution:", err)
	}
	if got := <-base.started; got != "alpha" {
		t.Fatalf("second tenant = %q", got)
	}
	base.release <- struct{}{}
	if err := <-second; err != nil {
		t.Fatal("second execution:", err)
	}
}

func TestBudgetedRunnerAllowsDifferentTenantsAndClosesActiveExecution(t *testing.T) {
	base := &budgetTestRunner{started: make(chan string, 8), release: make(chan struct{}, 8)}
	budget, err := NewBudgetedRunner(base, BudgetConfig{PerTenant: 1, MaxWaiters: 1, MaxTenants: 2, Wait: time.Second, DefaultTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, site := range []string{"alpha", "beta"} {
		go func(site string) {
			_, err := budget.Execute(context.Background(), ExecuteRequest{Site: site})
			results <- err
		}(site)
	}
	seen := map[string]bool{<-base.started: true, <-base.started: true}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("concurrent tenants started: %v", seen)
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("execution after close = %v", err)
		}
	}
	base.mu.Lock()
	closed := base.closed
	base.mu.Unlock()
	if !closed {
		t.Fatal("underlying runner was not closed")
	}
}

func TestBudgetedRunnerClampsExecutionTimeout(t *testing.T) {
	base := &budgetTestRunner{started: make(chan string, 1), release: make(chan struct{}, 1), timeouts: make(chan time.Duration, 1)}
	budget, err := NewBudgetedRunner(base, BudgetConfig{PerTenant: 1, MaxWaiters: 0, MaxTenants: 1, Wait: time.Second, DefaultTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := budget.Execute(context.Background(), ExecuteRequest{Site: "alpha", Timeout: time.Hour})
		done <- err
	}()
	if got := <-base.timeouts; got != 25*time.Millisecond {
		t.Fatalf("script timeout = %s, want 25ms", got)
	}
	base.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetedRunnerEnforcesExecutionDeadline(t *testing.T) {
	base := &budgetTestRunner{started: make(chan string, 1), release: make(chan struct{}, 1)}
	budget, err := NewBudgetedRunner(base, BudgetConfig{PerTenant: 1, MaxWaiters: 0, MaxTenants: 1, Wait: time.Second, DefaultTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = budget.Execute(context.Background(), ExecuteRequest{Site: "alpha"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("script result error = %v, want deadline exceeded", err)
	}
	if err := budget.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForScriptWaiter(t *testing.T, runner *BudgetedRunner, tenant string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		gate := runner.gates[tenant]
		got := 0
		if gate != nil {
			got = gate.waiters
		}
		runner.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("tenant %s did not reach %d waiters", tenant, want)
}
