package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

func testCatalog(scope contract.BusinessScope) contract.ApplicationCatalog {
	in := contract.SchemaRef{ID: "sales.input", Version: 1}
	out := contract.SchemaRef{ID: "sales.output", Version: 1}
	return contract.ApplicationCatalog{
		Revision: 7, BusinessScope: scope,
		Modules:              []contract.ApplicationCatalogModule{{ID: "sales.orders", Version: "1.0.0", State: string(contract.ModuleActive)}},
		PublishedQueries:     []contract.ApplicationCatalogQuery{{OwnerModuleID: "sales.orders", Name: "list", Route: "/api/v1/modules/sales.orders/queries/list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 20}},
		PublishedProjections: []contract.ApplicationCatalogProjection{{OwnerModuleID: "sales.orders", ID: "history", Route: "/api/v1/modules/sales.orders/projections/history", OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}}},
		PublishedOperations:  []contract.ApplicationCatalogOperation{{OperationRef: contract.PublishedOperationRef("sales.orders", "sales.invoice", "issue"), OwnerModuleID: "sales.orders", Capability: "sales.invoice", Operation: "issue", InputSchema: in, OutputSchema: out, Availability: contract.OperationAvailable, Permissions: []string{"invoice.issue"}}},
		Schemas:              []contract.ModuleSchemaDocument{{Ref: in, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: out, Schema: json.RawMessage(`{"type":"object"}`)}},
		AllowedEntityChoices: []contract.BusinessScope{}, Blockers: []contract.ModuleBlocker{},
	}
}

func TestScopedClientUsesCatalogRoutesScopeAndReceiptRecovery(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	catalog := testCatalog(scope)
	operationRef := contract.PublishedOperationRef("sales.orders", "sales.invoice", "issue")
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer actor-token" || r.Header.Get("X-Kora-Legal-Entity-ID") != "entity-a" {
			t.Errorf("request auth/scope = %q / %q", r.Header.Get("Authorization"), r.Header.Get("X-Kora-Legal-Entity-ID"))
		}
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/modules/catalog":
			data = catalog
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v1/modules/sales.orders/queries/list":
			var request contract.PublishedQueryRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Limit != 10 {
				t.Errorf("query request=%+v error=%v", request, err)
			}
			data = contract.QueryResult{Data: json.RawMessage(`{"items":[]}`), AsOf: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryFresh, SnapshotToken: "snapshot-1", SourceCursor: "cursor-1", SourceRevision: "rev-1"}
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v1/modules/sales.orders/projections/history":
			data = contract.QueryResult{Data: json.RawMessage(`{"items":[]}`), AsOf: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC), Freshness: contract.QueryStale}
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/modules/operations/"+operationRef+"/execute":
			var command contract.ExecutePublishedOperation
			if err := json.NewDecoder(r.Body).Decode(&command); err != nil || command.Meta.IdempotencyKey != "intent-1" {
				t.Errorf("execute request=%+v error=%v", command, err)
			}
			data = contract.OperationReceipt{ExecutionID: "exec-1", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptAccepted, Revision: "7"}
		case r.Method == http.MethodPost && r.URL.Path == "/s/demo/api/v2/module-executions/lookup":
			var lookup contract.LookupPublishedOperation
			if err := json.NewDecoder(r.Body).Decode(&lookup); err != nil || lookup.IdempotencyKey != "intent-1" {
				t.Errorf("lookup=%+v error=%v", lookup, err)
			}
			data = contract.OperationReceipt{ExecutionID: "exec-1", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptOutcomeUnknown, Error: contract.NewError(contract.CodeDependencyUnavailable, "provider result is unknown"), Revision: "7"}
		case r.Method == http.MethodGet && r.URL.Path == "/s/demo/api/v2/module-executions/exec-1":
			data = contract.OperationReceipt{ExecutionID: "exec-1", OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptRunning, Revision: "7"}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor-token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := client.WithScope(scope)
	if err != nil {
		t.Fatal(err)
	}
	gotCatalog, err := scoped.Catalog(context.Background())
	if err != nil || gotCatalog.Revision != 7 {
		t.Fatalf("Catalog=%+v err=%v", gotCatalog, err)
	}
	query, err := scoped.PublishedQuery(context.Background(), gotCatalog, "sales.orders", "list", contract.PublishedQueryRequest{Input: json.RawMessage(`{}`), Limit: 10})
	if err != nil || query.Freshness != contract.QueryFresh || query.SnapshotToken != "snapshot-1" || query.SourceCursor != "cursor-1" {
		t.Fatalf("PublishedQuery=%+v err=%v", query, err)
	}
	projection, err := scoped.Projection(context.Background(), gotCatalog, "sales.orders", "history")
	if err != nil || projection.Freshness != contract.QueryStale {
		t.Fatalf("Projection=%+v err=%v", projection, err)
	}
	command := contract.ExecutePublishedOperation{Meta: contract.CommandMeta{IdempotencyKey: "intent-1"}, InputSchema: contract.SchemaRef{ID: "sales.input", Version: 1}, Input: json.RawMessage(`{"amount":1}`)}
	if _, err := scoped.ExecutePublishedOperation(context.Background(), gotCatalog, operationRef, command); err != nil {
		t.Fatal("ExecutePublishedOperation:", err)
	}
	recovered, err := scoped.LookupExecution(context.Background(), gotCatalog, contract.LookupPublishedOperation{OperationRef: operationRef, IdempotencyKey: "intent-1"})
	if err != nil || recovered.Status != contract.ReceiptOutcomeUnknown {
		t.Fatalf("LookupExecution=%+v err=%v", recovered, err)
	}
	byID, err := scoped.Execution(context.Background(), gotCatalog, "exec-1")
	if err != nil || byID.Status != contract.ReceiptRunning {
		t.Fatalf("Execution=%+v err=%v", byID, err)
	}
	if strings.Join(paths, "|") != "GET /s/demo/api/v2/modules/catalog|POST /s/demo/api/v1/modules/sales.orders/queries/list|GET /s/demo/api/v1/modules/sales.orders/projections/history|POST /s/demo/api/v2/modules/operations/"+operationRef+"/execute|POST /s/demo/api/v2/module-executions/lookup|GET /s/demo/api/v2/module-executions/exec-1" {
		t.Fatalf("unexpected request paths: %v", paths)
	}
}

func TestScopedClientRejectsCatalogScopeMismatchAndUnsafeCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": testCatalog(scope)})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Catalog(context.Background()); err == nil {
		t.Fatal("catalog from another scope accepted")
	}
	catalog := testCatalog(scoped.Scope())
	if _, err := scoped.ExecutePublishedOperation(context.Background(), catalog, "sales.orders~sales.invoice~issue", contract.ExecutePublishedOperation{Meta: contract.CommandMeta{IdempotencyKey: "x"}, InputSchema: contract.SchemaRef{ID: "other", Version: 1}, Input: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("operation with forged input schema was sent")
	}
}

func TestScopedClientEscapesSitePathExactlyOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/s/tenant%20east/api/v2/modules/catalog"; got != want {
			t.Errorf("escaped request path = %q, want %q", got, want)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": testCatalog(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, SiteName: "tenant east", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor-token", nil })})
	if err != nil {
		t.Fatal("NewClient:", err)
	}
	scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
	if err != nil {
		t.Fatal("WithScope:", err)
	}
	if _, err := scoped.Catalog(context.Background()); err != nil {
		t.Fatal("Catalog:", err)
	}
}

func TestTenantRemoteErrorsRetainHTTPStatusAndEngineCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"permission.denied","message":"scope denied"}}`))
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "token", nil })})
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoped.Catalog(context.Background())
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Status != http.StatusForbidden || remote.Body.Type != "permission.denied" {
		t.Fatalf("error=%#v", err)
	}
}

func TestScopedClientKeepsConcurrentEntityScopesIsolated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entity := r.Header.Get("X-Kora-Legal-Entity-ID")
		if entity != "entity-a" && entity != "entity-b" {
			t.Errorf("missing entity header: %q", entity)
		}
		scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: entity}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": testCatalog(scope)})
	}))
	defer server.Close()
	client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor", nil })})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, entity := range []string{"entity-a", "entity-b"} {
		entity := entity
		wg.Add(1)
		go func() {
			defer wg.Done()
			scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: entity})
			if err != nil {
				t.Error(err)
				return
			}
			catalog, err := scoped.Catalog(context.Background())
			if err != nil || catalog.BusinessScope.LegalEntityID != entity {
				t.Errorf("entity %s got catalog scope %+v, err %v", entity, catalog.BusinessScope, err)
			}
		}()
	}
	wg.Wait()
}

func TestScopedClientHonorsCancellationBodyLimitAndStrictReceiptDecode(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor", nil })})
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := scoped.Catalog(ctx); done <- err }()
		<-started
		cancel()
		if err := <-done; err == nil {
			t.Fatal("cancelled request succeeded")
		}
	})

	t.Run("body limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":` + strings.Repeat(" ", maxResponseBytes+1) + `}`))
		}))
		defer server.Close()
		client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor", nil })})
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := scoped.Catalog(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized response error = %v", err)
		}
	})

	t.Run("strict receipt decoding", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"execution_id":"exec-1","operation_ref":"sales~sales.invoice~issue","business_scope":{"tenant_id":"tenant-a","kind":"tenant"},"status":"accepted","revision":"1","unexpected":true}}`))
		}))
		defer server.Close()
		client, err := NewClient(Config{BaseURL: server.URL, SiteName: "demo", Tokens: TokenSourceFunc(func(context.Context) (string, error) { return "actor", nil })})
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := client.WithScope(contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant})
		if err != nil {
			t.Fatal(err)
		}
		var receipt contract.OperationReceipt
		if err := scoped.do(context.Background(), http.MethodGet, "/api/v2/module-executions/exec-1", nil, http.StatusOK, &receipt); err == nil {
			t.Fatal("receipt with unknown field accepted")
		}
	})
}
