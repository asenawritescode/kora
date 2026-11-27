//go:build qjsscript

package qjs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLimits is the cap shared by every test: 8 MiB JS heap, 1 MiB JS stack, 128 MiB wasm memory.
var testLimits = Limits{HeapBytes: 8 << 20, StackBytes: 1 << 20, MaxPages: 2048}

// newTestEngine returns an engine closed at test end.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	eng, err := NewEngine(context.Background(), testLimits)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	return eng
}

// newTestInstance returns an instance closed at test end.
func newTestInstance(t *testing.T, eng *Engine, h Handler) *Instance {
	t.Helper()
	inst, err := eng.NewInstance(context.Background(), h)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

// noHostCall fails the test if the guest makes a host call.
func noHostCall(t *testing.T) Handler {
	return func(context.Context, []byte) []byte {
		t.Errorf("unexpected host call")
		return nil
	}
}

func TestHostRoundTrip(t *testing.T) {
	var got string
	inst := newTestInstance(t, newTestEngine(t), func(_ context.Context, req []byte) []byte {
		got = string(req)
		return []byte(`{"ok":true}`)
	})
	res, err := inst.Eval(context.Background(),
		`JSON.parse(__kora_host(JSON.stringify({op:"x",args:[1]}))).ok`, ModeJSONResult)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if res.Thrown || res.Output != "true" {
		t.Fatalf("result = %+v, want true", res)
	}
	if want := `{"op":"x","args":[1]}`; got != want {
		t.Fatalf("handler request = %q, want %q", got, want)
	}
}

func TestEvalResults(t *testing.T) {
	cases := []struct {
		name string
		src  string
		mode Mode
		want Result
	}{
		{name: "string completion", src: `"ok"`, want: Result{Output: "ok"}},
		{name: "thrown error", src: `throw new Error("bad table")`, want: Result{Thrown: true, Output: "Error: bad table"}},
		{name: "json object", src: `({a:1})`, mode: ModeJSONResult, want: Result{Output: `{"a":1}`}},
		{name: "undefined has no json", src: `undefined`, mode: ModeJSONResult, want: Result{}},
		{name: "compile-only valid source", src: `var x = 1;`, mode: ModeCompileOnly, want: Result{}},
		{name: "compile-only does not run a host call", src: `__kora_host("{}")`, mode: ModeCompileOnly, want: Result{}},
	}
	eng := newTestEngine(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstance(t, eng, noHostCall(t))
			got, err := inst.Eval(context.Background(), tc.src, tc.mode)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if got != tc.want {
				t.Fatalf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCompileOnlySyntaxError(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	got, err := inst.Eval(context.Background(), `var = ;`, ModeCompileOnly)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !got.Thrown || !strings.Contains(got.Output, "SyntaxError") {
		t.Fatalf("result = %+v, want a thrown SyntaxError", got)
	}
}

func TestShortEvalAfterLongEval(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	long := `var s = "` + strings.Repeat("a", 5000) + `"; s.length`
	if _, err := inst.Eval(context.Background(), long, 0); err != nil {
		t.Fatalf("long Eval: %v", err)
	}
	got, err := inst.Eval(context.Background(), `1+1`, ModeJSONResult)
	if err != nil {
		t.Fatalf("short Eval: %v", err)
	}
	if got.Thrown || got.Output != "2" {
		t.Fatalf("short result = %+v, want 2", got)
	}
}

func TestInstanceIsolation(t *testing.T) {
	eng := newTestEngine(t)
	a := newTestInstance(t, eng, noHostCall(t))
	b := newTestInstance(t, eng, noHostCall(t))
	if _, err := a.Eval(context.Background(), `globalThis.x = 1`, 0); err != nil {
		t.Fatalf("set in a: %v", err)
	}
	got, err := b.Eval(context.Background(), `typeof x`, 0)
	if err != nil {
		t.Fatalf("read in b: %v", err)
	}
	if got.Output != "undefined" {
		t.Fatalf("typeof x in b = %q, want undefined", got.Output)
	}
}

func TestConcurrentInstancesKeepTheirHandlers(t *testing.T) {
	eng := newTestEngine(t)
	const rounds = 20
	answers := map[string]Handler{
		"A": func(context.Context, []byte) []byte { return []byte(`"A"`) },
		"B": func(context.Context, []byte) []byte { return []byte(`"B"`) },
	}
	var wg sync.WaitGroup
	for who, h := range answers {
		inst := newTestInstance(t, eng, h)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				got, err := inst.Eval(context.Background(), `JSON.parse(__kora_host("{}"))`, ModeJSONResult)
				if err != nil || got.Output != `"`+who+`"` {
					t.Errorf("instance %s: got %+v, err %v", who, got, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestMemoryCap(t *testing.T) {
	eng := newTestEngine(t)
	t.Run("a trivial program runs under the cap", func(t *testing.T) {
		inst := newTestInstance(t, eng, noHostCall(t))
		got, err := inst.Eval(context.Background(), `var x = 0; for (var i = 0; i < 1000; i++) x += i; x;`, ModeJSONResult)
		if err != nil || got.Thrown || got.Output != "499500" {
			t.Fatalf("result = %+v, err %v; want 499500", got, err)
		}
	})
	t.Run("an unbounded allocation is refused, not OOM-killed", func(t *testing.T) {
		inst := newTestInstance(t, eng, noHostCall(t))
		got, err := inst.Eval(context.Background(), `var a = []; for (;;) { a.push(new Array(100000).fill(7)); }`, 0)
		if err != nil {
			t.Fatalf("unexpected error (a real OOM would surface here): %v", err)
		}
		if !got.Thrown {
			t.Fatalf("result = %+v, want a thrown memory error", got)
		}
	})
}

func TestTimeoutStopsRunawayGuest(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := inst.Eval(ctx, `for (;;) {}`, 0)
	if err == nil {
		t.Fatal("Eval of a runaway loop returned no error")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v; the host did not stop the guest promptly", elapsed)
	}
}

func TestRealClock(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	got, err := inst.Eval(context.Background(), `String(new Date().getFullYear() >= 2026)`, 0)
	if err != nil || got.Output != "true" {
		t.Fatalf("result = %+v, err %v; want true", got, err)
	}
}

func TestEvalRejectsOversizedSource(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	_, err := inst.Eval(context.Background(), strings.Repeat("a", 4*1024*1024), 0)
	if !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("err = %v, want ErrSourceTooLarge", err)
	}
}
