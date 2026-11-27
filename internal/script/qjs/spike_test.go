//go:build qjsscript

package qjs

import (
	"context"
	"testing"
	"time"
)

// TestSpikeMemoryCap proves the thing goja cannot do (F2): a script that tries to
// allocate without bound is refused by QuickJS's JS_SetMemoryLimit as a catchable
// JS exception, in a bounded time, without taking the host down — while a trivial
// script under the same cap runs to completion.
func TestSpikeMemoryCap(t *testing.T) {
	const heap = 8 * 1024 * 1024 // 8 MiB JS heap

	t.Run("a trivial program runs under the cap", func(t *testing.T) {
		got, err := evalSpike(context.Background(), `var x = 0; for (var i = 0; i < 1000; i++) x += i; x;`,
			spikeLimits{heapBytes: heap, stackSize: 1 << 20, maxPages: 1024, timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != evalOK {
			t.Fatalf("outcome = %s, want ok", got)
		}
	})

	t.Run("an unbounded allocation is refused, not OOM-killed", func(t *testing.T) {
		got, err := evalSpike(context.Background(), `var a = []; for (;;) { a.push(new Array(100000).fill(7)); }`,
			spikeLimits{heapBytes: heap, stackSize: 1 << 20, maxPages: 2048, timeout: 10 * time.Second})
		if err != nil {
			t.Fatalf("unexpected error (a real OOM/crash would surface here): %v", err)
		}
		if got != evalException {
			t.Fatalf("outcome = %s, want js-exception (the heap cap should trip)", got)
		}
	})
}

// TestSpikeTimeout proves the second wall: a runaway loop is stopped by the host's
// deadline instead of hanging the process.
func TestSpikeTimeout(t *testing.T) {
	start := time.Now()
	got, err := evalSpike(context.Background(), `for (;;) {}`,
		spikeLimits{heapBytes: 8 * 1024 * 1024, stackSize: 1 << 20, maxPages: 512, timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != evalStopped {
		t.Fatalf("outcome = %s, want host-stopped", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v; the host did not stop the guest promptly", elapsed)
	}
}
