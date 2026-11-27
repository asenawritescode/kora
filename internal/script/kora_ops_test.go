package script

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// fakeKoraProvider answers every KoraProvider method from fixed fields and records what it was asked.
type fakeKoraProvider struct {
	NoopProvider
	doc       map[string]any
	docs      []map[string]any
	readErr   error
	writeErr  error
	secret    string
	secretErr error
	httpResp  *HTTPResponse
	httpErr   error
	installed map[string]bool

	gotReq      *HTTPRequest
	gotFilters  map[string]any
	gotOrderBy  string
	gotLimit    int
	gotOffset   int
	saved       map[string]any
	savedBy     string
	created     map[string]any
	deletedName string
}

func (p *fakeKoraProvider) GetDoc(doctype, name string) (map[string]any, error) {
	return p.doc, p.readErr
}

func (p *fakeKoraProvider) GetList(doctype string, filters map[string]any, orderBy string, limit, offset int) ([]map[string]any, error) {
	p.gotFilters, p.gotOrderBy, p.gotLimit, p.gotOffset = filters, orderBy, limit, offset
	return p.docs, p.readErr
}

func (p *fakeKoraProvider) SaveDoc(doctype string, doc map[string]any, modifiedBy string) error {
	p.saved, p.savedBy = doc, modifiedBy
	return p.writeErr
}

func (p *fakeKoraProvider) CreateDoc(doctype string, doc map[string]any, owner, modifiedBy string) (map[string]any, error) {
	return p.created, p.writeErr
}

func (p *fakeKoraProvider) DeleteDoc(doctype, name string) error {
	p.deletedName = doctype + "/" + name
	return p.writeErr
}

func (p *fakeKoraProvider) GetSecret(key string) (string, error) {
	return p.secret, p.secretErr
}

func (p *fakeKoraProvider) DoHTTP(req *HTTPRequest) (*HTTPResponse, error) {
	p.gotReq = req
	return p.httpResp, p.httpErr
}

func (p *fakeKoraProvider) HasDocType(name string) bool { return p.installed[name] }

func TestKoraOpsNilProviderDefaults(t *testing.T) {
	ops := newKoraOps(context.Background(), ExecuteRequest{})
	tests := []struct {
		name    string
		run     func() (any, error)
		want    any
		wantErr string
	}{
		{name: "hasDocs", run: func() (any, error) { return ops.hasDocs(), nil }, want: false},
		{name: "hasSecrets", run: func() (any, error) { return ops.hasSecrets(), nil }, want: false},
		{name: "hasHTTP", run: func() (any, error) { return ops.hasHTTP(), nil }, want: false},
		{name: "hasDocType", run: func() (any, error) { return ops.hasDocType("Sale"), nil }, want: false},
		{name: "getDoc", run: func() (any, error) { return ops.getDoc("Sale", "S1") }, want: map[string]any(nil)},
		{name: "getList", run: func() (any, error) { return ops.getList("Sale", nil) }, want: []any{}},
		{name: "saveDoc", run: func() (any, error) { return ops.saveDoc("Sale", map[string]any{}) }, want: nil},
		{name: "createDoc", run: func() (any, error) { return ops.createDoc("Sale", map[string]any{}) }, want: map[string]any(nil)},
		{name: "deleteDoc", run: func() (any, error) { return nil, ops.deleteDoc("Sale", "S1") }, want: nil},
		{name: "getSecret", run: func() (any, error) { return ops.getSecret("k"), nil }, want: nil},
		{name: "fetch", run: func() (any, error) { return ops.fetch("https://example.test", nil) }, wantErr: "kora.http.fetch: provider not available"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.run()
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.name == "getList" {
				if list, ok := got.([]any); !ok || list == nil || len(list) != 0 {
					t.Fatalf("getList = %#v, want empty non-nil []any", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestKoraOpsErrors(t *testing.T) {
	boom := errors.New("boom")
	notFound := contract.NewError(contract.CodeNotFound, "missing")
	denied := contract.NewError(contract.CodePermissionDenied, "no")
	unauth := contract.NewError(contract.CodeUnauthenticated, "login")
	tests := []struct {
		name    string
		prov    *fakeKoraProvider
		run     func(*koraOps) (any, error)
		want    any
		wantErr string
	}{
		{name: "getDoc read error throws", prov: &fakeKoraProvider{readErr: boom},
			run: func(o *koraOps) (any, error) { return o.getDoc("Sale", "S1") }, wantErr: `kora.getDoc("Sale", "S1") failed: boom`},
		{name: "getDoc not found is null", prov: &fakeKoraProvider{readErr: notFound},
			run: func(o *koraOps) (any, error) { return o.getDoc("Sale", "S1") }, want: map[string]any(nil)},
		{name: "getDoc unauthenticated is null", prov: &fakeKoraProvider{readErr: unauth},
			run: func(o *koraOps) (any, error) { return o.getDoc("Sale", "S1") }, want: map[string]any(nil)},
		{name: "getList read error throws", prov: &fakeKoraProvider{readErr: boom},
			run: func(o *koraOps) (any, error) { return o.getList("Sale", nil) }, wantErr: `kora.getList("Sale") failed: boom`},
		{name: "getList permission denied is empty", prov: &fakeKoraProvider{readErr: denied},
			run: func(o *koraOps) (any, error) { return o.getList("Sale", nil) }, want: []any{}},
		{name: "saveDoc error", prov: &fakeKoraProvider{writeErr: boom},
			run: func(o *koraOps) (any, error) { return o.saveDoc("Sale", map[string]any{}) }, wantErr: `kora.saveDoc("Sale") error: boom`},
		{name: "createDoc error", prov: &fakeKoraProvider{writeErr: boom},
			run: func(o *koraOps) (any, error) { return o.createDoc("Sale", map[string]any{}) }, wantErr: `kora.createDoc("Sale") error: boom`},
		{name: "deleteDoc error", prov: &fakeKoraProvider{writeErr: boom},
			run: func(o *koraOps) (any, error) { return nil, o.deleteDoc("Sale", "S1") }, wantErr: `kora.deleteDoc("Sale", "S1") error: boom`},
		{name: "http error", prov: &fakeKoraProvider{httpErr: boom},
			run: func(o *koraOps) (any, error) { return o.fetch("https://example.test", nil) }, wantErr: "kora.http.fetch error: boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops := newKoraOps(context.Background(), ExecuteRequest{Provider: tc.prov})
			got, err := tc.run(ops)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestKoraOpsSecretErrorWarnsOnce(t *testing.T) {
	prov := &fakeKoraProvider{secretErr: errors.New("vault down")}
	ops := newKoraOps(context.Background(), ExecuteRequest{Provider: prov})
	if got := ops.getSecret("api_key"); got != nil {
		t.Fatalf("getSecret = %#v, want nil", got)
	}
	if len(ops.logs) != 1 || ops.logs[0].Level != "warn" || ops.logs[0].Message != "kora.secrets.get error: vault down" {
		t.Fatalf("logs = %+v, want one warn entry", ops.logs)
	}
}

func TestKoraOpsFetchDefaultsAndPreview(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		opts       map[string]any
		wantMethod string
		wantHdr    map[string]string
		wantBody   string
		wantErr    string
	}{
		{name: "defaults to GET", ctx: context.Background(), opts: map[string]any{},
			wantMethod: "GET", wantHdr: map[string]string{}},
		{name: "stringifies headers and keeps body", ctx: context.Background(),
			opts:       map[string]any{"method": "POST", "headers": map[string]any{"X-N": 3, "X-S": "a"}, "body": "{}"},
			wantMethod: "POST", wantHdr: map[string]string{"X-N": "3", "X-S": "a"}, wantBody: "{}"},
		{name: "preview refuses outbound", ctx: WithPreview(context.Background()), opts: map[string]any{},
			wantErr: "kora.http.fetch error: kora.http.fetch is not allowed while the change is being previewed; it runs only after the person confirms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prov := &fakeKoraProvider{httpResp: &HTTPResponse{Status: 200}}
			ops := newKoraOps(tc.ctx, ExecuteRequest{Provider: prov})
			_, err := ops.fetch("https://example.test", tc.opts)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prov.gotReq.Method != tc.wantMethod || prov.gotReq.Body != tc.wantBody || !reflect.DeepEqual(prov.gotReq.Headers, tc.wantHdr) {
				t.Fatalf("request = %+v, want method %q body %q headers %v", prov.gotReq, tc.wantMethod, tc.wantBody, tc.wantHdr)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int
	}{
		{name: "int", in: 7, want: 7},
		{name: "int64", in: int64(8), want: 8},
		{name: "float64", in: float64(9.9), want: 9},
		{name: "json number", in: json.Number("12"), want: 12},
		{name: "string falls back", in: "x", want: 50},
		{name: "nil falls back", in: nil, want: 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toInt(tc.in, 50); got != tc.want {
				t.Fatalf("toInt(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsEmptyReadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "not found", err: contract.NewError(contract.CodeNotFound, "x"), want: true},
		{name: "permission denied", err: contract.NewError(contract.CodePermissionDenied, "x"), want: true},
		{name: "unauthenticated", err: contract.NewError(contract.CodeUnauthenticated, "x"), want: true},
		{name: "infrastructure", err: errors.New("reset"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyReadError(tc.err); got != tc.want {
				t.Fatalf("isEmptyReadError = %v, want %v", got, tc.want)
			}
		})
	}
}
