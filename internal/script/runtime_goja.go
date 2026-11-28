//go:build gojascript

// Runtime on goja (pure Go ES5.1+): the fallback selected by -tags gojascript.

package script

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// EmbeddedRunner executes scripts in-process using goja.
// It maintains a pool of prewarmed runtimes for concurrent execution.
type EmbeddedRunner struct {
	pool      chan *goja.Runtime
	admission chan struct{}
	config    EmbeddedConfig
	mu        sync.Mutex
	closed    bool
	stop      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
}

// NewEmbeddedRunner creates a runner with a pool of prewarmed goja runtimes.
func NewEmbeddedRunner(cfg EmbeddedConfig) *EmbeddedRunner {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 15
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}

	r := &EmbeddedRunner{
		pool:      make(chan *goja.Runtime, cfg.PoolSize),
		admission: make(chan struct{}, cfg.PoolSize*2),
		config:    cfg,
		stop:      make(chan struct{}),
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())

	// Prewarm the pool.
	for i := 0; i < cfg.PoolSize; i++ {
		r.pool <- r.newRuntime()
	}
	return r
}

// newRuntime creates a fresh goja runtime with Kigae API and security hardening.
func (r *EmbeddedRunner) newRuntime() *goja.Runtime {
	vm := goja.New()

	// Freeze prototypes to prevent prototype pollution.
	r.freezePrototypes(vm)

	// Goja has no hard heap limit. Execution time is bounded below, and the
	// configured MaxRAM value remains advisory rather than an enforced cap.
	return vm
}

// freezePrototypesProgram freezes the built-in prototypes and disables __proto__ so scripts cannot
// pollute them. It is compiled once at startup, so a syntax error fails fast instead of silently
// leaving runtimes unhardened.
var freezePrototypesProgram = goja.MustCompile("freeze-prototypes.js", freezePrototypesSource, false)

// freezePrototypes prevents scripts from modifying built-in prototypes. Failing to harden a runtime
// is an invariant violation, so it fails closed rather than running scripts in an open sandbox.
func (r *EmbeddedRunner) freezePrototypes(vm *goja.Runtime) {
	if _, err := vm.RunProgram(freezePrototypesProgram); err != nil {
		panic(fmt.Errorf("script sandbox hardening failed: %w", err))
	}
}

// Execute runs a script using a runtime from the pool.
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

	// Acquire a runtime from the pool.
	var vm *goja.Runtime
	select {
	case vm = <-r.pool:
	case <-ctx.Done():
		return nil, fmt.Errorf("script: no runtime available: %w", ctx.Err())
	case <-r.stop:
		return nil, fmt.Errorf("script runner is closed")
	}
	defer func() {
		// Reset the runtime and return it to the pool.
		// Create a fresh runtime to avoid state leakage between executions.
		newVM := r.newRuntime()
		r.mu.Lock()
		if !r.closed {
			r.pool <- newVM
		}
		r.mu.Unlock()
	}()

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("script runner is closed")
	}

	// Apply timeout.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.config.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopRunnerCancellation := context.AfterFunc(r.ctx, cancel)
	defer stopRunnerCancellation()

	// Set up an interrupt via a timer.
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt("script execution timed out")
	})
	defer timer.Stop()
	stopExecutionInterrupt := context.AfterFunc(execCtx, func() {
		vm.Interrupt("script execution canceled")
	})
	defer stopExecutionInterrupt()

	// Inject the kora API into the runtime.
	ops := newKoraOps(ctx, req)
	api := &koraAPI{req: req, ops: ops}
	vm.Set("kora", api.buildObject(vm))

	vm.Set("__kora_event__", map[string]any{
		"doc":    jsValue(req.Document),
		"oldDoc": jsValue(req.OldDocument),
	})
	if req.Deterministic {
		_ = vm.Set("Date", goja.Undefined())
		if mathValue := vm.Get("Math"); mathValue != nil && !goja.IsUndefined(mathValue) {
			_ = mathValue.ToObject(vm).Set("random", goja.Undefined())
		}
		_, _ = vm.RunString(deterministicSource)
	}

	start := time.Now()

	// Execute the script. goja's throw() can cause a Go panic, so recover.
	var val goja.Value
	var err error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("script: runtime panic: %v", rec)
			}
		}()
		val, err = runWithLibraries(vm, req.Libraries, wrapScript(req.Script))
	}()
	duration := time.Since(start)

	// Check for context cancellation first.
	if execCtx.Err() != nil {
		return nil, fmt.Errorf("script: execution timed out after %v", timeout)
	}

	result := &ExecuteResult{
		Duration: duration,
		Logs:     ops.logs,
	}

	if err != nil {
		return nil, fmt.Errorf("script error: %w", err)
	}

	// If the script returned an object with a 'doc' property, use it as the modified document.
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		applyReturn(result, val.Export())
	}

	return result, nil
}

// runWithLibraries evaluates the libraries, then the wrapped script, in one VM.
func runWithLibraries(vm *goja.Runtime, libraries []Library, script string) (goja.Value, error) {
	if err := runLibraries(vm, libraries); err != nil {
		return nil, err
	}
	return vm.RunString(script)
}

// runLibraries evaluates each library at global scope, in order, so the
// declarations it makes are visible to the script that follows. A library that
// throws fails the run with its name in the error.
func runLibraries(vm *goja.Runtime, libraries []Library) error {
	for _, lib := range libraries {
		if err := loadGojaLibrary(vm, lib); err != nil {
			return err
		}
	}
	return nil
}

// loadGojaLibrary evaluates one library, then locks the globals it defined.
func loadGojaLibrary(vm *goja.Runtime, lib Library) error {
	before, err := gojaGlobalNames(vm)
	if err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	if _, err = vm.RunString(lib.Source); err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	after, err := gojaGlobalNames(vm)
	if err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	lock, err := lockGlobalsSource(newGlobalNames(before, after))
	if err != nil {
		return err
	}
	if _, err = vm.RunString(lock); err != nil {
		return fmt.Errorf("script: library %q failed to load: %w", lib.Name, err)
	}
	return nil
}

// gojaGlobalNames returns the names of the global object's own properties.
func gojaGlobalNames(vm *goja.Runtime) ([]string, error) {
	value, err := vm.RunString(globalNamesExpr)
	if err != nil {
		return nil, err
	}
	items, ok := value.Export().([]any)
	if !ok {
		return nil, fmt.Errorf("global names are not a list")
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("global name is not a string")
		}
		names = append(names, name)
	}
	return names, nil
}

// Validate checks that a script compiles without running it.
func (r *EmbeddedRunner) Validate(script string) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("script runner is closed")
	}
	r.mu.Unlock()

	vm := goja.New()
	r.freezePrototypes(vm)
	// Set stub globals so the script can reference event.doc and kora.* without error.
	vm.Set("__kora_event__", map[string]any{"doc": map[string]any{}, "oldDoc": nil})
	stubKora := vm.NewObject()
	stubLog := vm.NewObject()
	stubLog.Set("info", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	stubLog.Set("warn", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	stubLog.Set("error", func(call goja.FunctionCall) goja.Value { return goja.Undefined() })
	stubKora.Set("log", stubLog)
	stubKora.Set("hasDocType", func(call goja.FunctionCall) goja.Value { return vm.ToValue(false) })
	vm.Set("kora", stubKora)

	// First, try to compile without running to catch syntax errors.
	_, err := goja.Compile("script", wrapScript(script), false)
	if err != nil {
		return fmt.Errorf("script: syntax error: %w", err)
	}

	// Then run to catch reference errors (undefined variables, etc.).
	// Runtime errors (throw, TypeError, etc.) are expected — they happen during real execution too.
	_, runErr := vm.RunString(wrapScript(script))
	if runErr != nil {
		// Only fail on compilation-level errors (ReferenceError, SyntaxError).
		// Runtime throw/new Error is valid — scripts throw to reject operations.
		errStr := runErr.Error()
		if strings.Contains(errStr, "ReferenceError") || strings.Contains(errStr, "SyntaxError") {
			return fmt.Errorf("script: compile error: %w", runErr)
		}
	}
	return nil
}

// Close shuts down all runtimes in the pool.
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
	// Execute registers with active while holding mu, so after closed is set no
	// new Add can race this Wait. Running scripts are interrupted through ctx.
	r.active.Wait()
	close(r.pool)
	for vm := range r.pool {
		vm.Interrupt("runner closed")
	}
	return nil
}

// PoolSize returns the number of runtimes in the pool.
func (r *EmbeddedRunner) PoolSize() int {
	return r.config.PoolSize
}
