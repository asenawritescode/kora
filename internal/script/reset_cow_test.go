//go:build !gojascript

package script

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// withCopyReset runs fn with the copy-reset fallback forced, then restores the default.
// Tests that call it must not run in parallel.
func withCopyReset(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	forceCopyReset = true
	t.Cleanup(func() { forceCopyReset = false })
	fn(t)
}

func TestCopyResetFallbackIsUsedWhenForced(t *testing.T) {
	withCopyReset(t, func(t *testing.T) {
		r := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1, MaxRAM: 16 << 20, Timeout: 5 * time.Second})
		t.Cleanup(func() { _ = r.Close() })
		if r.snapshot.MemoryBytes() == 0 {
			t.Fatal("empty snapshot")
		}
		inst, err := r.newPrewarmed(context.Background())
		if err != nil {
			t.Fatalf("newPrewarmed: %v", err)
		}
		t.Cleanup(func() { _ = inst.Close(context.Background()) })
		if inst.MemoryBytes() != r.snapshot.MemoryBytes() {
			t.Fatalf("memory %d bytes, want the snapshot size %d", inst.MemoryBytes(), r.snapshot.MemoryBytes())
		}
	})
}

// TestCopyResetFallbackKeepsContracts runs the reset contract tests on the fallback path.
func TestCopyResetFallbackKeepsContracts(t *testing.T) {
	withCopyReset(t, func(t *testing.T) {
		t.Run("no script state leaks", TestResetLeavesNoScriptState)
		t.Run("random is reseeded", TestResetReseedsRandom)
		t.Run("timeout and heap cap after many runs", TestResetKeepsTimeoutAndHeapCapAfterManyRuns)
		t.Run("grown instance is discarded", TestResetDiscardsInstanceThatGrewPastBound)
		t.Run("concurrent executions stay isolated", TestResetConcurrentExecutionsStayIsolated)
	})
}

func TestCowResetLeavesNoGlobalsOrClosuresAfterManyRuns(t *testing.T) {
	r := onePoolRunner(t, DefaultEmbeddedConfig())
	for i := range 200 {
		runOK(t, r, ExecuteRequest{Script: fmt.Sprintf(
			`globalThis.g%d = {n: %d}; Array.prototype.leak%d = %d; globalThis.f%d = function () { return %d; }; return {result: 1};`,
			i%7, i, i%7, i, i%7, i)})
		awaitReset(t, r, i+1)
	}
	got := runOK(t, r, ExecuteRequest{Script: `
		var probe = [typeof g0, typeof g6, typeof f0, typeof Array.prototype.leak0];
		return {result: probe.join(",")};`})
	if got.Result != "undefined,undefined,undefined,undefined" {
		t.Fatalf("state survived 200 resets: %v", got.Result)
	}
}

// TestLargeStringsAcrossResets keeps large strings and JSON round trips correct on a
// reused instance, including JSON.parse of strings far past the 2 KiB copy that bug #879
// trapped on under wazero v1.8.2.
func TestLargeStringsAcrossResets(t *testing.T) {
	r := onePoolRunner(t, EmbeddedConfig{MaxRAM: 64 << 20, Timeout: 10 * time.Second})
	tests := []struct {
		name   string
		script string // %d is the string length
		n      int
		want   func(n int) string
	}{
		{name: "64 KiB string stringifies", n: 64 << 10,
			script: `var s = "x".repeat(%d); return {result: JSON.stringify(s).length};`,
			want:   func(n int) string { return strconv.Itoa(n + 2) }},
		{name: "1 MiB string stringifies", n: 1 << 20,
			script: `var s = "x".repeat(%d); return {result: JSON.stringify(s).length};`,
			want:   func(n int) string { return strconv.Itoa(n + 2) }},
		{name: "JSON.parse of a 2000 byte string", n: 2000,
			script: `var s = "x".repeat(%d); return {result: JSON.parse(JSON.stringify(s)).length};`,
			want:   strconv.Itoa},
		{name: "JSON.parse of a 4 KiB string", n: 4 << 10,
			script: `var s = "x".repeat(%d); return {result: JSON.parse(JSON.stringify(s)).length};`,
			want:   strconv.Itoa},
		{name: "JSON.parse of a 64 KiB string", n: 64 << 10,
			script: `var s = "x".repeat(%d); return {result: JSON.parse(JSON.stringify(s)).length};`,
			want:   strconv.Itoa},
	}
	// Large cases grow memory past twice the snapshot, so their instance is discarded
	// rather than reset; the second run then gets a fresh instance. Either way it must
	// see the right result, so no wait for a reset is needed.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(tc.script, tc.n)
			runOK(t, r, ExecuteRequest{Script: src})
			got := runOK(t, r, ExecuteRequest{Script: src})
			if fmt.Sprint(got.Result) != tc.want(tc.n) {
				t.Fatalf("result = %v, want %s", got.Result, tc.want(tc.n))
			}
		})
	}
}

// TestCowConcurrentRunsWithReset is the race-detector workload for mapped memory: many
// executions on a small pool, each dirtying memory and reading the previous run's state.
func TestCowConcurrentRunsWithReset(t *testing.T) {
	const workers, perWorker = 4, 8 // at most PoolSize workers: admission is PoolSize*2
	r := NewEmbeddedRunner(EmbeddedConfig{PoolSize: workers, MaxRAM: 64 << 20, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = r.Close() })
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				res, err := r.Execute(context.Background(), ExecuteRequest{Script: fmt.Sprintf(
					`var before = typeof globalThis.seen; globalThis.seen = %d; var a = []; for (var i = 0; i < 300; i++) a.push("y" + i); return {result: before === "undefined" ? a.length : -1};`, w*10+i)})
				if err != nil {
					errs <- err
					continue
				}
				if fmt.Sprint(res.Result) != "300" {
					errs <- fmt.Errorf("worker %d run %d saw state from another run: %v", w, i, res.Result)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
