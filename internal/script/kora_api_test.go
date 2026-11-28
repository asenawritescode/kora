package script

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file is the parity contract for kora.* behaviour: it runs scripts through Execute and
// asserts only observable results, so a different JS engine must pass it unchanged.

func runKora(t *testing.T, ctx context.Context, req ExecuteRequest) (*ExecuteResult, error) {
	t.Helper()
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	return runner.Execute(ctx, req)
}

// largeText is a 40 KiB document text field.
var largeText = strings.Repeat("x", 40<<10)

func TestKoraAPIBehaviour(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		prov    *fakeKoraProvider
		script  string
		want    any
		wantErr string
	}{
		{name: "getDoc found", prov: &fakeKoraProvider{doc: map[string]any{"name": "S1"}},
			script: `return {result: kora.getDoc("Sale", "S1").name};`, want: "S1"},
		// #879: a document string over 2 KiB trapped the QuickJS runtime while the shim parsed it.
		{name: "getDoc large text field", prov: &fakeKoraProvider{doc: map[string]any{"notes": largeText}},
			script: `return {result: kora.getDoc("Sale", "S1").notes === "x".repeat(40960)};`, want: true},
		{name: "getList large text fields", prov: &fakeKoraProvider{docs: []map[string]any{{"notes": largeText}, {"notes": largeText}}},
			script: `return {result: kora.getList("Sale").map(d => d.notes.length).join()};`, want: "40960,40960"},
		{name: "getDoc arity", prov: &fakeKoraProvider{},
			script: `kora.getDoc("Sale");`, wantErr: "kora.getDoc requires 2 arguments: doctype, name"},
		{name: "getDoc provider error", prov: &fakeKoraProvider{readErr: errors.New("boom")},
			script: `kora.getDoc("Sale", "S1");`, wantErr: `kora.getDoc("Sale", "S1") failed: boom`},
		{name: "getList length", prov: &fakeKoraProvider{docs: []map[string]any{{"name": "A"}, {"name": "B"}}},
			script: `return {result: kora.getList("Sale").length};`, want: int64(2)},
		{name: "getList arity", prov: &fakeKoraProvider{},
			script: `kora.getList();`, wantErr: "kora.getList requires at least 1 argument: doctype"},
		{name: "getList provider error", prov: &fakeKoraProvider{readErr: boom},
			script: `kora.getList("Sale");`, wantErr: `kora.getList("Sale") failed: boom`},
		{name: "hasDocType installed", prov: &fakeKoraProvider{installed: map[string]bool{"Sale": true}},
			script: `return {result: kora.hasDocType("Sale")};`, want: true},
		{name: "hasDocType arity", prov: &fakeKoraProvider{},
			script: `kora.hasDocType();`, wantErr: "kora.hasDocType requires 1 argument: doctype"},
		{name: "saveDoc returns doc", prov: &fakeKoraProvider{},
			script: `return {result: kora.saveDoc("Sale", {name: "S1", total: 2}).name};`, want: "S1"},
		{name: "saveDoc non-object", prov: &fakeKoraProvider{},
			script: `kora.saveDoc("Sale", 1);`, wantErr: "kora.saveDoc: second argument must be an object"},
		{name: "saveDoc arity", prov: &fakeKoraProvider{},
			script: `kora.saveDoc("Sale");`, wantErr: "kora.saveDoc requires 2 arguments: doctype, doc"},
		{name: "saveDoc provider error", prov: &fakeKoraProvider{writeErr: boom},
			script: `kora.saveDoc("Sale", {name: "S1"});`, wantErr: `kora.saveDoc("Sale") error: boom`},
		{name: "createDoc returns created", prov: &fakeKoraProvider{created: map[string]any{"name": "S2"}},
			script: `return {result: kora.createDoc("Sale", {total: 1}).name};`, want: "S2"},
		{name: "createDoc arity", prov: &fakeKoraProvider{},
			script: `kora.createDoc("Sale");`, wantErr: "kora.createDoc requires 2 arguments: doctype, doc"},
		{name: "createDoc non-object", prov: &fakeKoraProvider{},
			script: `kora.createDoc("Sale", "x");`, wantErr: "kora.createDoc: second argument must be an object"},
		{name: "createDoc provider error", prov: &fakeKoraProvider{writeErr: boom},
			script: `kora.createDoc("Sale", {});`, wantErr: `kora.createDoc("Sale") error: boom`},
		{name: "deleteDoc ok", prov: &fakeKoraProvider{},
			script: `kora.deleteDoc("Sale", "S1"); return {result: "done"};`, want: "done"},
		{name: "deleteDoc arity", prov: &fakeKoraProvider{},
			script: `kora.deleteDoc("Sale");`, wantErr: "kora.deleteDoc requires 2 arguments: doctype, name"},
		{name: "deleteDoc provider error", prov: &fakeKoraProvider{writeErr: boom},
			script: `kora.deleteDoc("Sale", "S1");`, wantErr: `kora.deleteDoc("Sale", "S1") error: boom`},
		{name: "secrets.get value", prov: &fakeKoraProvider{secret: "sk-1"},
			script: `return {result: kora.secrets.get("api_key")};`, want: "sk-1"},
		{name: "secrets.get error is null", prov: &fakeKoraProvider{secretErr: boom},
			script: `return {result: kora.secrets.get("api_key") === null};`, want: true},
		{name: "secrets.get arity", prov: &fakeKoraProvider{},
			script: `kora.secrets.get();`, wantErr: "kora.secrets.get requires 1 argument: key"},
		{name: "http.fetch status and ok", prov: &fakeKoraProvider{httpResp: &HTTPResponse{Status: 201, StatusText: "Created", Body: []byte(`{"a":1}`)}},
			script: `var r = kora.http.fetch("https://example.test", {}); return {result: [r.status, r.ok, r.statusText, r.body].join("|")};`,
			want:   "201|true|Created|{\"a\":1}"},
		{name: "http.fetch json", prov: &fakeKoraProvider{httpResp: &HTTPResponse{Status: 200, Body: []byte(`{"a":1}`)}},
			script: `return {result: kora.http.fetch("https://example.test", {}).json().a};`, want: int64(1)},
		{name: "http.fetch not ok", prov: &fakeKoraProvider{httpResp: &HTTPResponse{Status: 404}},
			script: `return {result: kora.http.fetch("https://example.test", {}).ok};`, want: false},
		{name: "http.fetch provider error", prov: &fakeKoraProvider{httpErr: boom},
			script: `kora.http.fetch("https://example.test", {});`, wantErr: "kora.http.fetch error: boom"},
		{name: "http.fetch arity", prov: &fakeKoraProvider{},
			script: `kora.http.fetch("https://example.test");`, wantErr: "kora.http.fetch requires 2 arguments: url, options"},
		{name: "http.fetch options not object", prov: &fakeKoraProvider{},
			script: `kora.http.fetch("https://example.test", 1);`, wantErr: "kora.http.fetch: second argument must be an options object"},
		{name: "context fields", prov: &fakeKoraProvider{},
			script: `return {result: [kora.context.user, kora.context.site, kora.context.doctype, kora.context.event, kora.context.userRoles.length].join("|")};`,
			want:   "alice@example.test|acme.local|Sale|before_save|2"},
		{name: "now is a Date constructor", prov: &fakeKoraProvider{},
			script: `return {result: typeof kora.now()};`, want: "function"},
		{name: "event oldDoc", prov: &fakeKoraProvider{},
			script: `return {result: event.oldDoc.name};`, want: "S0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runKora(t, context.Background(), ExecuteRequest{
				Script:      tc.script,
				DocType:     "Sale",
				Event:       EventBeforeSave,
				User:        "alice@example.test",
				UserRoles:   []string{"Sales", "Accounts"},
				Site:        "acme.local",
				Document:    map[string]any{"name": "S1"},
				OldDocument: map[string]any{"name": "S0"},
				Provider:    tc.prov,
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if !reflect.DeepEqual(result.Result, tc.want) {
				t.Fatalf("Result = %#v, want %#v", result.Result, tc.want)
			}
		})
	}
}

func TestKoraAPILogLevels(t *testing.T) {
	result, err := runKora(t, context.Background(), ExecuteRequest{
		Script: `kora.log.debug("d"); kora.log.info("i"); kora.log.warn("w"); kora.log.error("e"); kora.log.info();`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := []LogEntry{{Level: "debug", Message: "d"}, {Level: "info", Message: "i"}, {Level: "warn", Message: "w"}, {Level: "error", Message: "e"}, {Level: "info", Message: ""}}
	if !reflect.DeepEqual(result.Logs, want) {
		t.Fatalf("Logs = %+v, want %+v", result.Logs, want)
	}
}

func TestKoraAPIReturnedDocIsModification(t *testing.T) {
	result, err := runKora(t, context.Background(), ExecuteRequest{
		Script: `return {doc: {name: "S1", total: 5}, result: "ok"};`,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Modified || result.Document["name"] != "S1" || result.Result != "ok" {
		t.Fatalf("result = %+v, want modified doc S1 with result ok", result)
	}
}

func TestKoraAPIListArgumentsReachProvider(t *testing.T) {
	prov := &fakeKoraProvider{}
	_, err := runKora(t, context.Background(), ExecuteRequest{
		Script:   `kora.getList("Sale", {filters: {status: "open"}, orderBy: "name", limit: 5, offset: 2});`,
		Provider: prov,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !reflect.DeepEqual(prov.gotFilters, map[string]any{"status": "open"}) || prov.gotOrderBy != "name" || prov.gotLimit != 5 || prov.gotOffset != 2 {
		t.Fatalf("provider got filters=%v orderBy=%q limit=%d offset=%d", prov.gotFilters, prov.gotOrderBy, prov.gotLimit, prov.gotOffset)
	}
}

func TestKoraAPIWritesUseRunUser(t *testing.T) {
	prov := &fakeKoraProvider{}
	_, err := runKora(t, context.Background(), ExecuteRequest{
		Script:   `kora.saveDoc("Sale", {name: "S1"}); kora.deleteDoc("Sale", "S2");`,
		User:     "alice@example.test",
		Provider: prov,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if prov.savedBy != "alice@example.test" || prov.deletedName != "Sale/S2" {
		t.Fatalf("savedBy=%q deleted=%q", prov.savedBy, prov.deletedName)
	}
}

func TestKoraAPIPreviewRefusesFetch(t *testing.T) {
	_, err := runKora(t, WithPreview(context.Background()), ExecuteRequest{
		Script:   `kora.http.fetch("https://example.test", {});`,
		Provider: &fakeKoraProvider{httpResp: &HTTPResponse{Status: 200}},
	})
	if err == nil || !strings.Contains(err.Error(), "kora.http.fetch error: kora.http.fetch is not allowed while the change is being previewed") {
		t.Fatalf("error = %v, want preview refusal", err)
	}
}
