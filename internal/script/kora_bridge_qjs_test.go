//go:build !gojascript

package script

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestKoraBridgeRefusesBadRequests(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantThrow string
	}{
		{name: "malformed JSON", raw: `{"op":`, wantThrow: "malformed request"},
		{name: "not an object", raw: `[1]`, wantThrow: "want an object"},
		{name: "unknown op", raw: `{"op":"nope","args":[]}`, wantThrow: `unknown operation "nope"`},
		{name: "missing op", raw: `{"args":[]}`, wantThrow: `unknown operation ""`},
		{name: "wrong argument type", raw: `{"op":"log","args":[1,"x"]}`, wantThrow: "argument 1 must be a string"},
		{name: "missing argument", raw: `{"op":"getDoc","args":["Item"]}`, wantThrow: "argument 2 must be a string"},
		{name: "object argument not an object", raw: `{"op":"saveDoc","args":["Item",[]]}`, wantThrow: "argument 2 must be an object"},
	}
	ctx := context.Background()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bridge := koraBridge{ops: newKoraOps(ctx, ExecuteRequest{})}
			var resp struct {
				Throw string `json:"throw"`
			}
			if err := json.Unmarshal(bridge.handle(ctx, []byte(tc.raw)), &resp); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			if !strings.Contains(resp.Throw, tc.wantThrow) {
				t.Fatalf("throw = %q, want it to contain %q", resp.Throw, tc.wantThrow)
			}
		})
	}
}

func TestKoraBridgeDocNumbersReachProviderAsGoja(t *testing.T) {
	p := &fakeKoraProvider{}
	result, err := runKora(t, context.Background(), ExecuteRequest{
		Provider: p,
		Script:   `kora.saveDoc("Item", {qty: 3, price: 1.5, big: 9007199254740992, nested: [2, 0.25]});`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("Execute returned no result")
	}
	want := map[string]any{"qty": int64(3), "price": 1.5, "big": int64(9007199254740992), "nested": []any{int64(2), 0.25}}
	if !reflect.DeepEqual(p.saved, want) {
		t.Fatalf("saved doc = %#v, want %#v", p.saved, want)
	}
}

func TestKoraBridgeHidesHostFunction(t *testing.T) {
	result, err := runKora(t, context.Background(), ExecuteRequest{
		Script: `return {result: typeof __kora_host};`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Result != "undefined" {
		t.Fatalf("typeof __kora_host = %v, want undefined", result.Result)
	}
}

func TestKoraBridgeCollectsLogsInOrder(t *testing.T) {
	result, err := runKora(t, context.Background(), ExecuteRequest{
		Script: `kora.log.warn("first"); kora.log.error("second"); kora.log.info();`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []LogEntry{{Level: "warn", Message: "first"}, {Level: "error", Message: "second"}, {Level: "info", Message: ""}}
	if !reflect.DeepEqual(result.Logs, want) {
		t.Fatalf("Logs = %+v, want %+v", result.Logs, want)
	}
}

func TestKoraBridgeHidesSetupGlobals(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		want    any
		wantErr string
	}{
		{name: "install function removed after setup", script: `return {result: typeof __kora_install};`, want: "undefined"},
		{name: "host function removed", script: `return {result: typeof __kora_host};`, want: "undefined"},
		{name: "install function cannot be called", script: `__kora_install({}); return {result: 1};`, wantErr: "__kora_install"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runKora(t, context.Background(), ExecuteRequest{Script: tc.script})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Execute error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Result != tc.want {
				t.Fatalf("result = %v, want %v", result.Result, tc.want)
			}
		})
	}
}
