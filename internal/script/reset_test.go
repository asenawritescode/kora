//go:build !gojascript

package script

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// onePoolRunner returns a QuickJS runner with a single pooled instance, so every
// execution after the first can only be served by a reset of that instance.
func onePoolRunner(t *testing.T, cfg EmbeddedConfig) *EmbeddedRunner {
	t.Helper()
	cfg.PoolSize = 1
	r := NewEmbeddedRunner(cfg)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// awaitReset waits until the pool holds the instance that ran before, reset at least n
// times, and returns it to the pool. A reset runs off the request path, so the test waits.
func awaitReset(t *testing.T, r *EmbeddedRunner, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case inst := <-r.ready:
			reused := inst.Reused()
			r.ready <- inst
			if reused >= n {
				return
			}
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pooled instance not reset %d times in time", n)
}

// runOK runs req and fails the test on an error.
func runOK(t *testing.T, r *EmbeddedRunner, req ExecuteRequest) *ExecuteResult {
	t.Helper()
	res, err := r.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

func TestResetLeavesNoScriptState(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
		want   any
	}{
		{name: "global from previous run is gone",
			first:  `globalThis.leak = "x"; return {result: 1};`,
			second: `return {result: String(globalThis.leak)};`,
			want:   "undefined"},
		{name: "function from previous run is gone",
			first:  `globalThis.helper = function () { return 1; }; return {result: 1};`,
			second: `return {result: typeof globalThis.helper};`,
			want:   "undefined"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := onePoolRunner(t, DefaultEmbeddedConfig())
			runOK(t, r, ExecuteRequest{Script: tc.first})
			awaitReset(t, r, 1)
			got := runOK(t, r, ExecuteRequest{Script: tc.second})
			if got.Result != tc.want {
				t.Fatalf("second run result = %v, want %v", got.Result, tc.want)
			}
		})
	}
}

func TestResetReseedsRandom(t *testing.T) {
	r := onePoolRunner(t, DefaultEmbeddedConfig())
	const script = `return {result: Math.random()};`
	first := runOK(t, r, ExecuteRequest{Script: script})
	awaitReset(t, r, 1)
	second := runOK(t, r, ExecuteRequest{Script: script})
	if first.Result == second.Result {
		t.Fatalf("Math.random repeated %v across a reset; the reset must reseed", first.Result)
	}
}

func TestResetKeepsTimeoutAndHeapCapAfterManyRuns(t *testing.T) {
	const runs = 50
	cfg := EmbeddedConfig{MaxRAM: 16 << 20, Timeout: 10 * time.Second}
	r := onePoolRunner(t, cfg)
	for i := range runs {
		runOK(t, r, ExecuteRequest{Script: `return {result: 1};`})
		awaitReset(t, r, i+1)
	}
	tests := []struct {
		name    string
		script  string
		timeout time.Duration
		errs    string
	}{
		{name: "timeout still stops a runaway script", script: `while (true) {}`,
			timeout: 300 * time.Millisecond, errs: "timed out"},
		{name: "heap cap still stops a huge allocation",
			script:  `var a = []; for (var i = 0; i < 1000; i++) { a.push("x".repeat(1 << 20)); } return {result: a.length};`,
			timeout: 10 * time.Second, errs: "script error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Execute(context.Background(), ExecuteRequest{Script: tc.script, Timeout: tc.timeout})
			if err == nil || !strings.Contains(err.Error(), tc.errs) {
				t.Fatalf("Execute = %v, want an error containing %q", err, tc.errs)
			}
			// The abnormal run's instance is discarded, so the next run gets a fresh one.
			got := runOK(t, r, ExecuteRequest{Script: `return {result: 2};`})
			if fmt.Sprint(got.Result) != "2" {
				t.Fatalf("run after %s = %v, want 2", tc.name, got.Result)
			}
		})
	}
}

func TestResetDiscardsInstanceThatGrewPastBound(t *testing.T) {
	r := onePoolRunner(t, EmbeddedConfig{MaxRAM: 16 << 20, Timeout: 5 * time.Second})
	runOK(t, r, ExecuteRequest{Script: `globalThis.big = "x".repeat(4 << 20); return {result: 1};`})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case inst := <-r.ready:
			if inst.Reused() != 0 {
				t.Fatalf("grown instance was reset (reused %d), want it closed", inst.Reused())
			}
			if inst.MemoryBytes() > 2*r.snapshot.MemoryBytes() {
				t.Fatalf("replacement memory %d over the bound", inst.MemoryBytes())
			}
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("no replacement instance refilled the pool")
}

func TestResetConcurrentExecutionsStayIsolated(t *testing.T) {
	const workers, perWorker = 8, 10
	r := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 4, MaxRAM: 128 << 20, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = r.Close() })
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				want := w*100 + i
				res, err := r.Execute(context.Background(), ExecuteRequest{
					Script:   `globalThis.seen = event.doc.n; return {result: event.doc.n};`,
					Document: map[string]any{"n": want},
				})
				if err != nil {
					errs <- err
					continue
				}
				if fmt.Sprint(res.Result) != fmt.Sprint(want) {
					errs <- fmt.Errorf("execution got %v, want %d", res.Result, want)
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
