//go:build qjsscript

package script

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newQJSTestRunner returns a runner closed at test end.
func newQJSTestRunner(t *testing.T, cfg EmbeddedConfig) *EmbeddedRunner {
	t.Helper()
	runner := NewEmbeddedRunner(cfg)
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}

func TestQJSExecuteReturnValues(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		want    any
		wantErr string
	}{
		{name: "integer is int64", script: `return {result: 42};`, want: int64(42)},
		{name: "fraction is float64", script: `return {result: 1.5};`, want: 1.5},
		{name: "string", script: `return {result: "plain"};`, want: "plain"},
		{name: "no completion value leaves result nil", script: `var a = 1;`, want: nil},
		{name: "throw is a script error", script: `throw new Error("boom");`, wantErr: "script error"},
		{name: "throw names the exception", script: `throw new Error("boom");`, wantErr: "boom"},
	}
	runner := newQJSTestRunner(t, EmbeddedConfig{PoolSize: 1, Timeout: 5 * time.Second})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runner.Execute(context.Background(), ExecuteRequest{Script: tc.script})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Execute error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(res.Result, tc.want) {
				t.Fatalf("result = %#v, want %#v", res.Result, tc.want)
			}
		})
	}
}

func TestQJSTimeoutNamesLimit(t *testing.T) {
	runner := newQJSTestRunner(t, EmbeddedConfig{PoolSize: 1, Timeout: time.Minute})
	_, err := runner.Execute(context.Background(), ExecuteRequest{
		Script:  `for (;;) {}`,
		Timeout: 100 * time.Millisecond,
	})
	const want = "script: execution timed out after 100ms"
	if err == nil || err.Error() != want {
		t.Fatalf("Execute error = %v, want %q", err, want)
	}
}

func TestQJSHeapCapStopsAllocationAndRunnerStaysUsable(t *testing.T) {
	const timeout = 10 * time.Second
	runner := newQJSTestRunner(t, EmbeddedConfig{MaxRAM: 16 << 20, PoolSize: 1, Timeout: timeout})

	start := time.Now()
	_, err := runner.Execute(context.Background(), ExecuteRequest{
		Script: `var a = []; for (;;) { a.push(new Array(1000000).fill(1)); }`,
	})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("unbounded allocation error = %v, want out of memory", err)
	}
	if elapsed := time.Since(start); elapsed >= timeout {
		t.Fatalf("heap cap took %v, want it to stop before the %v timeout", elapsed, timeout)
	}

	res, err := runner.Execute(context.Background(), ExecuteRequest{Script: `return {result: 1};`})
	if err != nil {
		t.Fatalf("Execute after heap cap: %v", err)
	}
	if res.Result != int64(1) {
		t.Fatalf("result after heap cap = %#v, want int64(1)", res.Result)
	}
}

func TestQJSDocumentRoundTrip(t *testing.T) {
	runner := newQJSTestRunner(t, EmbeddedConfig{PoolSize: 1, Timeout: 5 * time.Second})
	doc := map[string]any{
		"name":  "order",
		"qty":   int64(3),
		"price": json.Number("1.5"),
		"lines": []any{map[string]any{"n": int64(1)}},
	}
	res, err := runner.Execute(context.Background(), ExecuteRequest{
		Script:   `return {doc: event.doc};`,
		Document: doc,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := map[string]any{
		"name":  "order",
		"qty":   int64(3),
		"price": 1.5,
		"lines": []any{map[string]any{"n": int64(1)}},
	}
	if !res.Modified || !reflect.DeepEqual(res.Document, want) {
		t.Fatalf("document = (%v, %#v), want (true, %#v)", res.Modified, res.Document, want)
	}
}

func TestQJSValidate(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "valid script", script: `return {result: 1};`},
		{name: "syntax error", script: `var x = ;`, wantErr: "syntax error"},
		{name: "reference error", script: `return {result: missingName};`, wantErr: "compile error"},
		{name: "runtime throw is valid", script: `throw new Error("reject");`},
	}
	runner := newQJSTestRunner(t, EmbeddedConfig{PoolSize: 1, Timeout: 5 * time.Second})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runner.Validate(tc.script)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
