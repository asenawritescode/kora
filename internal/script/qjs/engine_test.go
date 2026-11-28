package qjs

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testLimits is the cap shared by every test: 8 MiB JS heap, 768 KiB JS stack (below the
// 1 MiB wasm stack, so QuickJS throws before the wasm stack overflows), 128 MiB wasm memory.
var testLimits = Limits{HeapBytes: 8 << 20, StackBytes: 768 << 10, MaxPages: 2048}

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
	inst, err := eng.NewInstance(context.Background())
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	inst.SetHandler(h)
	return inst
}

func TestHostCallHandlerBinding(t *testing.T) {
	const hostCall = `__kora_host("{}")`
	tests := []struct {
		name       string
		setHandler bool
		want       string
		reached    bool
	}{
		{name: "host call before SetHandler throws", setHandler: false,
			want: `{"throw":"kora host calls are not available"}`},
		{name: "host call after SetHandler reaches the handler", setHandler: true,
			want: `{"ok":1}`, reached: true},
	}
	eng := newTestEngine(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := eng.NewInstance(context.Background())
			if err != nil {
				t.Fatalf("NewInstance: %v", err)
			}
			t.Cleanup(func() { _ = inst.Close(context.Background()) })
			reached := false
			if tc.setHandler {
				inst.SetHandler(func(context.Context, []byte) []byte {
					reached = true
					return []byte(`{"ok":1}`)
				})
			}
			res, err := inst.Eval(context.Background(), hostCall, 0)
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if res.Output != tc.want {
				t.Fatalf("output = %q, want %q", res.Output, tc.want)
			}
			if reached != tc.reached {
				t.Fatalf("handler reached = %v, want %v", reached, tc.reached)
			}
		})
	}
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

// A 3 MiB source, larger than the old fixed input buffer, grows the guest buffer and runs.
func TestEvalGrowsBufferForLargeSource(t *testing.T) {
	inst := newTestInstance(t, newTestEngine(t), noHostCall(t))
	src := "var s='" + strings.Repeat("a", 3<<20) + "'; s.length"
	got, err := inst.Eval(context.Background(), src, ModeJSONResult)
	if err != nil || got.Thrown || got.Output != "3145728" {
		t.Fatalf("result = %+v, err %v; want 3145728", got, err)
	}
}

// snapshotOf runs init on a template instance and captures it.
func snapshotOf(t *testing.T, eng *Engine) *Snapshot {
	t.Helper()
	snap, err := newTestInstance(t, eng, noHostCall(t)).Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snap
}

// restoredInstance returns an instance restored from snap, closed at test end.
func restoredInstance(t *testing.T, eng *Engine, snap *Snapshot) *Instance {
	t.Helper()
	inst, err := eng.Restore(context.Background(), snap)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	inst.SetHandler(noHostCall(t))
	return inst
}

func TestRestoredInstanceMatchesFresh(t *testing.T) {
	eng := newTestEngine(t)
	tests := []struct {
		name, src, want string
	}{
		{"arithmetic", `var x = 0; for (var i = 0; i < 1000; i++) x += i; x;`, "499500"},
		{"json", `JSON.stringify({a: [1, 2], s: "x"})`, `"{\"a\":[1,2],\"s\":\"x\"}"`},
		{"stack overflow is catchable", `function f() { return f() + 1; } try { f(); } catch (e) { "caught"; }`, `"caught"`},
		{"deep JSON nesting is catchable", `try { JSON.parse("[".repeat(100000)); } catch (e) { e.name; }`, `"RangeError"`},
	}
	snap := snapshotOf(t, eng)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fresh := newTestInstance(t, eng, noHostCall(t))
			restored := restoredInstance(t, eng, snap)
			for who, inst := range map[string]*Instance{"fresh": fresh, "restored": restored} {
				got, err := inst.Eval(context.Background(), tc.src, ModeJSONResult)
				if err != nil || got.Thrown || got.Output != tc.want {
					t.Fatalf("%s: result = %+v, err %v; want %s", who, got, err, tc.want)
				}
			}
		})
	}
}

func TestRestoredInstancesAreIsolatedAndReseeded(t *testing.T) {
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	a := restoredInstance(t, eng, snap)
	b := restoredInstance(t, eng, snap)
	if _, err := a.Eval(context.Background(), `globalThis.leak = 1`, 0); err != nil {
		t.Fatalf("set in a: %v", err)
	}
	got, err := b.Eval(context.Background(), `typeof leak`, 0)
	if err != nil || got.Output != "undefined" {
		t.Fatalf("typeof leak in b = %+v, err %v; want undefined", got, err)
	}
	ra, err := a.Eval(context.Background(), `Math.random()`, ModeJSONResult)
	if err != nil {
		t.Fatalf("random in a: %v", err)
	}
	rb, err := b.Eval(context.Background(), `Math.random()`, ModeJSONResult)
	if err != nil {
		t.Fatalf("random in b: %v", err)
	}
	if ra.Output == rb.Output {
		t.Fatalf("restored instances share a Math.random sequence: both %q", ra.Output)
	}
}

// Strings over 2 KiB make QuickJS copy more than 2048 bytes at once (memory.copy). Under
// wazero v1.8.2 built with Go 1.24+ that copy clobbered a register the compiled code still
// used, so these trapped out of bounds or crashed the process (#879).
func TestLargeStringsDoNotTrap(t *testing.T) {
	big := strconv.Quote(strings.Repeat("x", 40<<10))
	tests := []struct {
		name, src, want string
		h               Handler
	}{
		{name: "JSON.parse just over 2 KiB", src: `JSON.parse('{"s":"' + "a".repeat(2049) + '"}').s.length`, want: "2049"},
		{name: "JSON.parse 40 KiB", src: `JSON.parse('{"s":"' + "a".repeat(40960) + '"}').s.length`, want: "40960"},
		{name: "JSON.parse 1 MiB", src: `JSON.parse('{"s":"' + "a".repeat(1 << 20) + '"}').s.length`, want: "1048576"},
		{name: "concat 40 KiB", src: `var s = ""; for (var i = 0; i < 40960; i++) s += "a"; s.length`, want: "40960"},
		{name: "stringify round trip 40 KiB", src: `var o = {s: "\u20ac".repeat(20480)}; JSON.parse(JSON.stringify(o)).s === o.s`, want: "true"},
		{name: "host response 40 KiB", src: `JSON.parse(__kora_host("{}")).s.length`, want: "40960",
			h: func(context.Context, []byte) []byte { return []byte(`{"s":` + big + `}`) }},
	}
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h
			if h == nil {
				h = noHostCall(t)
			}
			fresh := newTestInstance(t, eng, h)
			restored := restoredInstance(t, eng, snap)
			restored.SetHandler(h)
			for who, inst := range map[string]*Instance{"fresh": fresh, "restored": restored} {
				got, err := inst.Eval(context.Background(), tc.src, ModeJSONResult)
				if err != nil || got.Thrown || got.Output != tc.want {
					t.Fatalf("%s: result = %+v, err %v; want %s", who, got, err, tc.want)
				}
			}
		})
	}
}
