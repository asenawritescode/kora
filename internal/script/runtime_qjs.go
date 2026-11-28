//go:build qjsscript

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

const (
	qjsStackBytes    = 1 << 20  // JS stack cap per instance
	qjsHeadroomBytes = 32 << 20 // wasm pages beyond the JS heap: input buffer, wasm stack, static data
	qjsPageBytes     = 64 << 10 // wasm linear-memory page size
	qjsDefaultHeap   = 128 << 20

	// validateStubSource gives a script under Validate the globals it may reference.
	validateStubSource = `var __kora_event__ = {doc: {}, oldDoc: null}; var kora = {log: {info: function(){}, warn: function(){}, error: function(){}}, hasDocType: function(){ return false; }};`
)

// EmbeddedRunner executes scripts in-process on QuickJS (wazero). Each execution
// runs in a fresh instance, so no state leaks between executions.
type EmbeddedRunner struct {
	engine    *qjs.Engine
	slots     chan struct{} // PoolSize concurrent executions
	admission chan struct{} // PoolSize*2, same as goja
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
	r := &EmbeddedRunner{
		engine:    engine,
		slots:     make(chan struct{}, cfg.PoolSize),
		admission: make(chan struct{}, cfg.PoolSize*2),
		config:    cfg,
		stop:      make(chan struct{}),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
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
	inst, err := r.engine.NewInstance(execCtx, koraBridge{ops: ops}.handle)
	if err != nil {
		return nil, fmt.Errorf("script: new instance: %w", err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }() // best effort: the outcome is already decided

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
// JSON of the script's completion value.
func (r *EmbeddedRunner) run(ctx context.Context, inst *qjs.Instance, req ExecuteRequest, ops *koraOps) (string, error) {
	if err := evalSetup(ctx, inst, freezePrototypesSource); err != nil {
		return "", fmt.Errorf("script sandbox hardening failed: %w", err)
	}
	if err := installKora(ctx, inst, ops, req); err != nil {
		return "", fmt.Errorf("script: kora setup failed: %w", err)
	}
	event, err := eventSource(req)
	if err != nil {
		return "", err
	}
	if err = evalSetup(ctx, inst, event); err != nil {
		return "", fmt.Errorf("script: setup failed: %w", err)
	}
	if req.Deterministic {
		if err = evalSetup(ctx, inst, deterministicSource); err != nil {
			return "", fmt.Errorf("script: setup failed: %w", err)
		}
	}
	for _, lib := range req.Libraries {
		if err = loadQJSLibrary(ctx, inst, lib); err != nil {
			return "", err
		}
	}
	res, err := inst.Eval(ctx, wrapScript(req.Script), qjs.ModeJSONResult)
	if err != nil {
		return "", fmt.Errorf("script error: %w", err)
	}
	if res.Thrown {
		return "", fmt.Errorf("script error: %w", errors.New(res.Output))
	}
	return res.Output, nil
}

// loadQJSLibrary evaluates one library, then locks the globals it defined.
func loadQJSLibrary(ctx context.Context, inst *qjs.Instance, lib Library) error {
	before, err := qjsGlobalNames(ctx, inst)
	if err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	if err = evalSetup(ctx, inst, lib.Source); err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	after, err := qjsGlobalNames(ctx, inst)
	if err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	lock, err := lockGlobalsSource(newGlobalNames(before, after))
	if err != nil {
		return err
	}
	if err = evalSetup(ctx, inst, lock); err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	return nil
}

// qjsGlobalNames returns the names of the global object's own properties.
func qjsGlobalNames(ctx context.Context, inst *qjs.Instance) ([]string, error) {
	res, err := inst.Eval(ctx, globalNamesExpr, qjs.ModeJSONResult)
	if err != nil {
		return nil, err
	}
	if res.Thrown {
		return nil, errors.New(res.Output)
	}
	var names []string
	if err := json.Unmarshal([]byte(res.Output), &names); err != nil {
		return nil, fmt.Errorf("decode global names: %w", err)
	}
	return names, nil
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
	inst, err := r.engine.NewInstance(ctx, refuseHostCall)
	if err != nil {
		return fmt.Errorf("script: new instance: %w", err)
	}
	defer func() { _ = inst.Close(ctx) }() // best effort: the check is already decided

	if err = evalSetup(ctx, inst, freezePrototypesSource); err != nil {
		return fmt.Errorf("script sandbox hardening failed: %w", err)
	}
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
	return r.engine.Close(context.Background())
}

// PoolSize returns the number of concurrent executions the runner admits.
func (r *EmbeddedRunner) PoolSize() int {
	return r.config.PoolSize
}
