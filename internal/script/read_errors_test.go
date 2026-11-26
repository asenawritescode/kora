package script

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// readProvider answers GetDoc and GetList with fixed results, so a test can
// tell a provider error apart from a legitimate empty result.
type readProvider struct {
	NoopProvider
	doc  map[string]any
	docs []map[string]any
	err  error
}

func (p readProvider) GetDoc(doctype, name string) (map[string]any, error) {
	return p.doc, p.err
}

func (p readProvider) GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error) {
	return p.docs, p.err
}

func TestKoraReadThrowsOnError(t *testing.T) {
	boom := errors.New("connection reset")
	tests := []struct {
		name     string
		expr     string
		wantText string
	}{
		{name: "getList provider error", expr: `kora.getList("Account")`, wantText: `kora.getList("Account") failed: connection reset`},
		{name: "getDoc provider error", expr: `kora.getDoc("Account", "ACC-1")`, wantText: `kora.getDoc("Account", "ACC-1") failed: connection reset`},
	}
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runner.Execute(context.Background(), ExecuteRequest{
				Script:   "var x = " + tc.expr + "; return {result: 'continued'};",
				Provider: readProvider{err: boom},
			})
			if err == nil {
				t.Fatalf("%s returned no error; a read failure must fail the script", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantText)
			}
		})
	}
}

func TestKoraReadEmptyIsNotError(t *testing.T) {
	notFound := contract.NewError(contract.CodeNotFound, "document not found")
	denied := contract.NewError(contract.CodePermissionDenied, "not permitted")
	tests := []struct {
		name     string
		provider KoraProvider
		expr     string
		want     any
	}{
		{name: "getList empty result", provider: readProvider{}, expr: `Array.isArray(kora.getList("Account")) && kora.getList("Account").length === 0`, want: true},
		{name: "getDoc not found", provider: readProvider{}, expr: `kora.getDoc("Account", "ACC-404") === null`, want: true},
		{name: "getDoc record not found error", provider: readProvider{err: notFound}, expr: `kora.getDoc("Account", "ACC-404") === null`, want: true},
		{name: "getList permission denied", provider: readProvider{err: denied}, expr: `Array.isArray(kora.getList("Account")) && kora.getList("Account").length === 0`, want: true},
		{name: "getDoc permission denied", provider: readProvider{err: denied}, expr: `kora.getDoc("Account", "ACC-1") === null`, want: true},
		{name: "getList with rows", provider: readProvider{docs: []map[string]any{{"name": "ACC-1"}}}, expr: `kora.getList("Account").length`, want: int64(1)},
		{name: "getDoc found", provider: readProvider{doc: map[string]any{"name": "ACC-1"}}, expr: `kora.getDoc("Account", "ACC-1").name`, want: "ACC-1"},
	}
	runner := NewEmbeddedRunner(DefaultEmbeddedConfig())
	t.Cleanup(func() { _ = runner.Close() }) // test-only runner; Close errors cannot change the assertions
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := runner.Execute(context.Background(), ExecuteRequest{
				Script:   "return {result: " + tc.expr + "};",
				Provider: tc.provider,
			})
			if err != nil {
				t.Fatalf("Execute: %v; an empty read must not fail the script", err)
			}
			if result.Result != tc.want {
				t.Fatalf("%s = %v (%T), want %v", tc.expr, result.Result, result.Result, tc.want)
			}
		})
	}
}
