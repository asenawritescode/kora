// Package script provides the JavaScript runtime using goja (pure Go ES5.1+).
// A build tag switches to QJS+Wazero when available.
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
	pool   chan *goja.Runtime
	config EmbeddedConfig
	mu     sync.Mutex
	closed bool
}

// EmbeddedConfig configures the embedded runner.
type EmbeddedConfig struct {
	PoolSize int           // number of prewarmed runtimes (default: 15)
	MaxRAM   int64         // soft memory target in bytes
	Timeout  time.Duration // default execution timeout
}

// DefaultEmbeddedConfig returns sensible defaults.
func DefaultEmbeddedConfig() EmbeddedConfig {
	return EmbeddedConfig{
		PoolSize: 15,
		MaxRAM:   128 * 1024 * 1024, // 128 MB
		Timeout:  5 * time.Second,
	}
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
		pool:   make(chan *goja.Runtime, cfg.PoolSize),
		config: cfg,
	}

	// Prewarm the pool.
	for i := 0; i < cfg.PoolSize; i++ {
		r.pool <- r.newRuntime()
	}
	return r
}

// newRuntime creates a fresh goja runtime with Kora API and security hardening.
func (r *EmbeddedRunner) newRuntime() *goja.Runtime {
	vm := goja.New()

	// Freeze prototypes to prevent prototype pollution.
	r.freezePrototypes(vm)

	// Set memory limits via the runtime's internal limits.
	// goja doesn't expose memory limits directly, but we can set
	// a timer-based interrupt for long-running scripts.
	return vm
}

// freezePrototypes prevents scripts from modifying built-in prototypes.
func (r *EmbeddedRunner) freezePrototypes(vm *goja.Runtime) {
	// Object.freeze(Object.prototype), Object.freeze(Array.prototype), etc.
	vm.RunString(`
		(function() {
			var frozen = [Object.prototype, Array.prototype, String.prototype,
				Number.prototype, Boolean.prototype, Function.prototype,
				Error.prototype, RegExp.prototype, Date.prototype];
			for (var i = 0; i < frozen.length; i++) {
				try { Object.freeze(frozen[i]); } catch(e) {}
			}
			// Block access to __proto__
			try {
				Object.defineProperty(Object.prototype, '__proto__', {
					get: function() { return undefined; },
					set: function() { throw new Error('__proto__ is disabled'); }
				});
			} catch(e) {}
		})();
	`)
}

// Execute runs a script using a runtime from the pool.
func (r *EmbeddedRunner) Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, fmt.Errorf("script runner is closed")
	}
	r.mu.Unlock()

	// Acquire a runtime from the pool.
	var vm *goja.Runtime
	select {
	case vm = <-r.pool:
	case <-ctx.Done():
		return nil, fmt.Errorf("script: no runtime available: %w", ctx.Err())
	}
	defer func() {
		// Reset the runtime and return it to the pool.
		// Create a fresh runtime to avoid state leakage between executions.
		newVM := r.newRuntime()
		r.pool <- newVM
	}()

	// Apply timeout.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.config.Timeout
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Set up an interrupt via a timer.
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt("script execution timed out")
	})
	defer timer.Stop()

	// Inject the kora API into the runtime.
	api := &koraAPI{
		req:    req,
		runner: r,
		logs:   make([]LogEntry, 0),
	}
	vm.Set("kora", api.buildObject(vm))

	// Set the event parameter.
	vm.Set("__kora_event__", map[string]any{
		"doc":    req.Document,
		"oldDoc": req.OldDocument,
	})

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
		script := r.wrapScript(req)
		val, err = vm.RunString(script)
	}()
	duration := time.Since(start)

	// Check for context cancellation first.
	if execCtx.Err() != nil {
		return nil, fmt.Errorf("script: execution timed out after %v", timeout)
	}

	result := &ExecuteResult{
		Duration: duration,
		Logs:     api.logs,
	}

	if err != nil {
		return nil, fmt.Errorf("script error: %w", err)
	}

	// If the script returned an object with a 'doc' property, use it as the modified document.
	if val != nil && !goja.IsUndefined(val) && !goja.IsNull(val) {
		if obj, ok := val.Export().(map[string]any); ok {
			if modifiedDoc, ok := obj["doc"]; ok {
				if doc, ok := modifiedDoc.(map[string]any); ok {
					result.Document = doc
					result.Modified = true
				}
			}
			if returnVal, ok := obj["result"]; ok {
				result.Result = returnVal
			}
		} else {
			result.Result = val.Export()
		}
	}

	return result, nil
}

// wrapScript wraps the user's script in an async IIFE with the kora API injected.
func (r *EmbeddedRunner) wrapScript(req ExecuteRequest) string {
	return fmt.Sprintf(`
		(function() {
			var event = __kora_event__;
			%s
		})();
	`, req.Script)
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
	vm.Set("kora", stubKora)

	// First, try to compile without running to catch syntax errors.
	_, err := goja.Compile("script", r.wrapScript(ExecuteRequest{Script: script}), false)
	if err != nil {
		return fmt.Errorf("script: syntax error: %w", err)
	}

	// Then run to catch reference errors (undefined variables, etc.).
	// Runtime errors (throw, TypeError, etc.) are expected — they happen during real execution too.
	_, runErr := vm.RunString(r.wrapScript(ExecuteRequest{Script: script}))
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
	defer r.mu.Unlock()
	r.closed = true
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
