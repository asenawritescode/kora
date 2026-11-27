//go:build !gojascript

package script

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/asenawritescode/kora/internal/script/qjs"
)

// QuickJS under WASI reports UTC; production containers run UTC too, so goja matches (no change).
const (
	qjsStackBytes    = 768 << 10 // JS stack cap per instance; below the 1 MiB wasm stack so QuickJS throws first
	qjsHeadroomBytes = 32 << 20  // wasm pages beyond the JS heap: input buffer, wasm stack, static data
	qjsPageBytes     = 64 << 10  // wasm linear-memory page size
	qjsDefaultHeap   = 128 << 20

	// validateStubSource gives a script under Validate the globals it may reference.
	validateStubSource = `var __kora_event__ = {doc: {}, oldDoc: null}; var kora = {log: {info: function(){}, warn: function(){}, error: function(){}}, hasDocType: function(){ return false; }};`
)

// EmbeddedRunner executes scripts in-process on QuickJS (wazero). Instances are
// prewarmed off the request path, and each serves exactly one execution: no state
// leaks between executions, and a used instance is always closed.
type EmbeddedRunner struct {
	engine    *qjs.Engine
	snapshot  *qjs.Snapshot      // the hardened template every prewarmed instance restores
	ready     chan *qjs.Instance // prewarmed instances, PoolSize at most
	slots     chan struct{}      // PoolSize concurrent executions
	admission chan struct{}      // PoolSize*2, same as goja
	config    EmbeddedConfig
	mu        sync.Mutex
	closed    bool
	stop      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
}

// NewEmbeddedRunner creates a runner on one QuickJS engine. A failing engine is an
// invariant violation, so it panics rather than running scripts unsandboxed.
func NewEmbeddedRunner(cfg EmbeddedConfig) *EmbeddedRunner {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 15
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	engine, err := qjs.NewEngine(context.Background(), qjsLimits(cfg.MaxRAM))
	if err != nil {
		panic(fmt.Errorf("script: qjs engine: %w", err))
	}
	snapshot, err := buildTemplate(context.Background(), engine)
	if err != nil {
		_ = engine.Close(context.Background()) // best effort: the template error is the one to report
		panic(fmt.Errorf("script: qjs template: %w", err))
	}
	r := &EmbeddedRunner{
		engine:    engine,
		snapshot:  snapshot,
		ready:     make(chan *qjs.Instance, cfg.PoolSize),
		slots:     make(chan struct{}, cfg.PoolSize),
		admission: make(chan struct{}, cfg.PoolSize*2),
		config:    cfg,
		stop:      make(chan struct{}),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	for range cfg.PoolSize {
		inst, err := r.newPrewarmed(r.ctx)
		if err != nil {
			panic(fmt.Errorf("script: prewarm qjs instance: %w", err))
		}
		r.ready <- inst
	}
	return r
}

// buildTemplate runs QuickJS init and the sandbox hardening once, then captures the
// result. Every prewarmed instance restores from it, so the hardened prototypes are
// frozen in the snapshot rather than rebuilt per instance.
func buildTemplate(ctx context.Context, engine *qjs.Engine) (*qjs.Snapshot, error) {
	inst, err := engine.NewInstance(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close(ctx) }() // best effort: the snapshot is already taken or the error is reported
	if err = evalSetup(ctx, inst, freezePrototypesSource); err != nil {
		return nil, fmt.Errorf("script sandbox hardening failed: %w", err)
	}
	if err = evalSetup(ctx, inst, koraDefineSource); err != nil {
		return nil, fmt.Errorf("script: kora bridge define failed: %w", err)
	}
	return inst.Snapshot()
}

// newPrewarmed restores an instance from the hardened template, so an execution starts
// from frozen prototypes.
func (r *EmbeddedRunner) newPrewarmed(ctx context.Context) (*qjs.Instance, error) {
	return r.engine.Restore(ctx, r.snapshot)
}

// takeInstance returns the instance for one execution: a prewarmed one when the pool
// has one, else one built inline. It never waits for a refill. Each call starts one
// refill, so the pool recovers even when it was empty.
func (r *EmbeddedRunner) takeInstance(ctx context.Context) (*qjs.Instance, error) {
	r.refillAsync() //nolint:contextcheck // the refill is bound to r.ctx, not the request, so Close cancels and waits for it
	select {
	case inst := <-r.ready:
		return inst, nil
	default:
		inst, err := r.newPrewarmed(ctx)
		if err != nil {
			return nil, fmt.Errorf("script: new instance: %w", err)
		}
		return inst, nil
	}
}

// refillAsync prewarms one replacement in the background, bounded by the runner
// context. It is tracked by active, so it starts only inside an Execute (which holds
// active), and Close waits for it.
func (r *EmbeddedRunner) refillAsync() {
	r.active.Add(1)
	go func() {
		defer r.active.Done()
		inst, err := r.newPrewarmed(r.ctx)
		if err != nil {
			return // no replacement: the next execution builds inline
		}
		select {
		case r.ready <- inst:
		default:
			_ = inst.Close(context.Background()) // the pool is full: drop the surplus instance
		}
	}()
}

// drainReady closes the prewarmed instances that no execution will take. Call it only
// after active.Wait, so no refill can still send to ready.
func (r *EmbeddedRunner) drainReady() {
	for {
		select {
		case inst := <-r.ready:
			_ = inst.Close(context.Background()) // best effort at shutdown
		default:
			return
		}
	}
}

// qjsLimits derives the engine caps from the heap budget. The uint32 ABI bounds the heap,
// and the page cap adds headroom beyond it.
func qjsLimits(maxRAM int64) qjs.Limits {
	heap := min(maxRAM, math.MaxUint32-qjsHeadroomBytes)
	if heap <= 0 {
		heap = qjsDefaultHeap
	}
	return qjs.Limits{
		HeapBytes:  uint32(heap),
		StackBytes: qjsStackBytes,
		MaxPages:   uint32((heap + qjsHeadroomBytes) / qjsPageBytes),
	}
}

// refuseHostCall answers the guest's host calls during Validate. Validate runs no kora
// code (its stub replaces kora), so any host call is refused.
func refuseHostCall(context.Context, []byte) []byte {
	return []byte(`{"error":"kora host calls are not available"}`)
}

// Execute runs a script in a fresh QuickJS instance.
func (r *EmbeddedRunner) Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, fmt.Errorf("script runner is closed")
	}
	select {
	case r.admission <- struct{}{}:
	default:
		r.mu.Unlock()
		return nil, fmt.Errorf("script runner is at capacity")
	}
	r.active.Add(1)
	r.mu.Unlock()
	defer func() {
		<-r.admission
		r.active.Done()
	}()

	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("script: no runtime available: %w", ctx.Err())
	case <-r.stop:
		return nil, fmt.Errorf("script runner is closed")
	}
	defer func() { <-r.slots }()

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("script runner is closed")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.config.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopRunnerCancellation := context.AfterFunc(r.ctx, cancel) //nolint:contextcheck // runner-wide context: Close cancels every execution
	defer stopRunnerCancellation()

	ops := newKoraOps(ctx, req)
	inst, err := r.takeInstance(execCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }() // best effort: the outcome is already decided
	inst.SetHandler(koraBridge{ops: ops}.handle)

	start := time.Now()
	out, err := r.run(execCtx, inst, req, ops)
	duration := time.Since(start)

	// Check for context cancellation first, as the goja runner does.
	if execCtx.Err() != nil {
		return nil, fmt.Errorf("script: execution timed out after %v", timeout)
	}
	if err != nil {
		return nil, err
	}

	result := &ExecuteResult{Duration: duration, Logs: ops.logs}
	if out == "" { // the script has no completion value
		return result, nil
	}
	value, err := decodeJSValue(out)
	if err != nil {
		return nil, fmt.Errorf("script error: %w", err)
	}
	applyReturn(result, value)
	return result, nil
}

// run evaluates the setup steps, the libraries and then the script, returning the
// JSON of the script's completion value. The instance arrives already hardened.
func (r *EmbeddedRunner) run(ctx context.Context, inst *qjs.Instance, req ExecuteRequest, ops *koraOps) (string, error) {
	setup, err := setupSource(req, ops)
	if err != nil {
		return "", err
	}
	if err = evalSetup(ctx, inst, setup); err != nil {
		return "", fmt.Errorf("script: setup failed: %w", err)
	}
	for _, lib := range req.Libraries {
		res, err := r.evalCached(ctx, inst, lib.Source, 0)
		if err != nil {
			return "", fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
		}
		if res.Thrown {
			return "", fmt.Errorf("script: library %q failed to load: %w", lib.Name, errors.New(res.Output))
		}
	}
	res, err := r.evalCached(ctx, inst, wrapScript(req.Script), qjs.ModeJSONResult)
	if err != nil {
		return "", fmt.Errorf("script error: %w", err)
	}
	if res.Thrown {
		return "", fmt.Errorf("script error: %w", errors.New(res.Output))
	}
	return res.Output, nil
}

// evalSetup runs one source for its effect; a throw is returned as an error.
func evalSetup(ctx context.Context, inst *qjs.Instance, src string) error {
	res, err := inst.Eval(ctx, src, 0)
	if err != nil {
		return err
	}
	if res.Thrown {
		return errors.New(res.Output)
	}
	return nil
}

// setupSource is the single setup evaluation of an execution: it builds kora, binds the
// event and, when the request is deterministic, removes the clock and random sources.
// The steps run in that order, before the libraries, as they did when evaluated apart.
func setupSource(req ExecuteRequest, ops *koraOps) (string, error) {
	install, err := koraInstallSource(ops, req)
	if err != nil {
		return "", err
	}
	event, err := eventSource(req)
	if err != nil {
		return "", err
	}
	parts := []string{install, event}
	if req.Deterministic {
		parts = append(parts, deterministicSource+";")
	}
	return strings.Join(parts, "\n"), nil
}

// eventSource binds __kora_event__ to the document and the old document, as JSON.
func eventSource(req ExecuteRequest) (string, error) {
	doc, err := json.Marshal(req.Document)
	if err != nil {
		return "", fmt.Errorf("script: encode document: %w", err)
	}
	oldDoc, err := json.Marshal(req.OldDocument)
	if err != nil {
		return "", fmt.Errorf("script: encode old document: %w", err)
	}
	return fmt.Sprintf(`var __kora_event__ = {"doc": %s, "oldDoc": %s};`, doc, oldDoc), nil
}

// Validate checks that a script compiles without running it.
func (r *EmbeddedRunner) Validate(script string) error {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return fmt.Errorf("script runner is closed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), r.config.Timeout)
	defer cancel()
	// Restore from the template: it already froze the prototypes and deleted __kora_host
	// from the global object. A fresh engine instance would expose the host function.
	inst, err := r.newPrewarmed(ctx)
	if err != nil {
		return fmt.Errorf("script: new instance: %w", err)
	}
	defer func() { _ = inst.Close(ctx) }() // best effort: the check is already decided
	inst.SetHandler(refuseHostCall)

	if err = evalSetup(ctx, inst, validateStubSource); err != nil {
		return fmt.Errorf("script: validate setup failed: %w", err)
	}

	// Compile without running to catch syntax errors.
	res, err := inst.Eval(ctx, wrapScript(script), qjs.ModeCompileOnly)
	if err != nil {
		return fmt.Errorf("script: validate: %w", err)
	}
	if res.Thrown {
		return fmt.Errorf("script: syntax error: %s", res.Output)
	}

	// Then run to catch reference errors. Other throws are valid at runtime, so they pass.
	res, err = inst.Eval(ctx, wrapScript(script), 0)
	if err != nil || !res.Thrown {
		return nil
	}
	if strings.Contains(res.Output, "ReferenceError") || strings.Contains(res.Output, "SyntaxError") {
		return fmt.Errorf("script: compile error: %s", res.Output)
	}
	return nil
}

// Close stops active executions, waits for them, and releases the engine. It is idempotent.
func (r *EmbeddedRunner) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.active.Wait()
		return nil
	}
	r.closed = true
	close(r.stop)
	r.cancel()
	r.mu.Unlock()
	// Execute registers with active while holding mu, so no Add can race this Wait.
	r.active.Wait()
	r.drainReady()
	return r.engine.Close(context.Background())
}

// PoolSize returns the number of concurrent executions the runner admits.
func (r *EmbeddedRunner) PoolSize() int {
	return r.config.PoolSize
}
