package script

import (
	"fmt"
	"time"
)

// EmbeddedConfig configures the embedded runner.
type EmbeddedConfig struct {
	PoolSize int           // number of prewarmed runtimes (default: 15)
	MaxRAM   int64         // per-execution heap cap, enforced on QuickJS (default); advisory on the goja fallback
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

// freezePrototypesSource freezes the built-in prototypes and disables __proto__ so scripts cannot
// pollute them.
const freezePrototypesSource = `
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
	`

// deterministicSource removes the wall clock and random sources from a runtime.
const deterministicSource = "Date = undefined; Math.random = undefined; Object.defineProperty(globalThis, 'Date', {value: undefined, writable: false, configurable: false}); Object.freeze(Math)"

// wrapScript wraps the user's script in an IIFE that exposes the event as `event`.
func wrapScript(script string) string {
	return fmt.Sprintf(`
		(function() {
			var event = __kora_event__;
			%s
		})();
	`, script)
}

// applyReturn maps a script's return value onto the result: an object with a
// "doc" map replaces the document, an object's "result" key sets the result, and
// any other value is the result itself.
func applyReturn(res *ExecuteResult, v any) {
	obj, ok := v.(map[string]any)
	if !ok {
		res.Result = v
		return
	}
	if doc, ok := obj["doc"].(map[string]any); ok {
		res.Document = doc
		res.Modified = true
	}
	if returnVal, ok := obj["result"]; ok {
		res.Result = returnVal
	}
}
