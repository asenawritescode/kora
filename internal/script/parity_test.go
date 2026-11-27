package script

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This file is the parity contract between the embedded runtimes: each case runs through
// NewEmbeddedRunner, so it runs on QuickJS by default and on goja under -tags gojascript.

func TestParityDocArgumentsMustBePlainObjects(t *testing.T) {
	const msg = "second argument must be an object"
	tests := []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "saveDoc Date", script: `kora.saveDoc("X", new Date());`, wantErr: "kora.saveDoc: " + msg},
		{name: "createDoc array", script: `kora.createDoc("X", [1]);`, wantErr: "kora.createDoc: " + msg},
		{name: "saveDoc null", script: `kora.saveDoc("X", null);`, wantErr: "kora.saveDoc: " + msg},
		{name: "saveDoc function", script: `kora.saveDoc("X", function () {});`, wantErr: "kora.saveDoc: " + msg},
		{name: "createDoc Map", script: `kora.createDoc("X", new Map());`, wantErr: "kora.createDoc: " + msg},
	}
	if qjsParity {
		// intentional: stricter than goja (#843); goja accepts a RegExp as an empty map.
		tests = append(tests, struct {
			name    string
			script  string
			wantErr string
		}{name: "saveDoc RegExp", script: `kora.saveDoc("X", /x/);`, wantErr: "kora.saveDoc: " + msg})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runKora(t, context.Background(), ExecuteRequest{Provider: &fakeKoraProvider{}, Script: tc.script})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestParityClassInstanceIsAPlainDocument(t *testing.T) {
	p := &fakeKoraProvider{}
	_, err := runKora(t, context.Background(), ExecuteRequest{
		Provider: p,
		Script:   `class Row { constructor() { this.qty = 2; } } kora.saveDoc("X", new Row());`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := map[string]any{"qty": int64(2)}; !reflect.DeepEqual(p.saved, want) {
		t.Fatalf("saved = %#v, want %#v", p.saved, want)
	}
}

func TestParityNonPlainListAndFetchOptions(t *testing.T) {
	p := &fakeKoraProvider{}
	if _, err := runKora(t, context.Background(), ExecuteRequest{
		Provider: p,
		Script:   `kora.getList("X", new Date());`,
	}); err != nil {
		t.Fatalf("getList with a Date as options: %v", err)
	}
	if p.gotLimit != 50 {
		t.Fatalf("getList limit = %d, want the default 50", p.gotLimit)
	}
	_, err := runKora(t, context.Background(), ExecuteRequest{Provider: p, Script: `kora.http.fetch("https://x.invalid", new Date());`})
	if err == nil || !strings.Contains(err.Error(), "kora.http.fetch: second argument must be an options object") {
		t.Fatalf("fetch with a Date as options: err = %v", err)
	}
}

// TestParityNilDocuments records what event.doc and event.oldDoc are when the document is nil or empty.
func TestParityNilDocuments(t *testing.T) {
	const script = `return [typeof event.doc, event.doc === null, JSON.stringify(event.doc),
		typeof event.oldDoc, event.oldDoc === null, JSON.stringify(event.oldDoc)];`
	tests := []struct {
		name string
		req  ExecuteRequest
		want []any
	}{
		{name: "nil document", req: ExecuteRequest{Script: script},
			want: []any{"object", true, "null", "object", true, "null"}},
		{name: "empty document", req: ExecuteRequest{Script: script, Document: map[string]any{}, OldDocument: map[string]any{}},
			want: []any{"object", false, "{}", "object", false, "{}"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runKora(t, context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(res.Result, tc.want) {
				t.Fatalf("result = %#v, want %#v", res.Result, tc.want)
			}
		})
	}
}

// TestParityProviderTimeAndBytes records how a provider's time.Time value reaches a script. goja wraps
// the Go value (its String() is time.Time's format, an accident of the binding); QuickJS passes the
// JSON string, the same form the REST API returns. This is an intentional difference (#843).
func TestParityProviderTimeAndBytes(t *testing.T) {
	p := &fakeKoraProvider{doc: map[string]any{
		"at":  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		"raw": []byte("ab"),
	}}
	res, err := runKora(t, context.Background(), ExecuteRequest{
		Provider: p,
		Script:   `var d = kora.getDoc("X", "a"); return [typeof d.at, String(d.at), typeof d.raw];`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []any{"object", "2026-01-02 03:04:05 +0000 UTC", "object"}
	if qjsParity {
		want = []any{"string", "2026-01-02T03:04:05Z", "string"}
	}
	if !reflect.DeepEqual(res.Result, want) {
		t.Fatalf("result = %#v, want %#v", res.Result, want)
	}
}

// TestParityCreateDocDecimalType pins an intentional difference: a decimal returned by createDoc is a
// JS number under QuickJS, as every other read through jsValue. goja wraps json.Number as a Go value
// (typeof "object"), so goja keeps that answer and only QuickJS is asserted to be "number".
func TestParityCreateDocDecimalType(t *testing.T) {
	want := "object"
	if qjsParity {
		want = "number"
	}
	p := &fakeKoraProvider{created: map[string]any{"amount": json.Number("12.50")}}
	res, err := runKora(t, context.Background(), ExecuteRequest{
		Provider: p,
		Script:   `return {result: typeof kora.createDoc("X", {}).amount};`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Result != want {
		t.Fatalf("typeof amount = %v, want %q", res.Result, want)
	}
}
