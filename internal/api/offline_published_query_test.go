package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

func TestOfflinePublishedQueryHTTPPinsSnapshotAndPullsCommittedChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := sql.Open("sqlite3", t.TempDir()+"/offline-http.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	provider, err := plugin.NewSQLPublishedQueryProvider(database, db.Resolve("libsql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}

	scope := contract.BusinessScope{TenantID: "tenant-http", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	in := contract.SchemaRef{ID: "orders.input", Version: 1}
	out := contract.SchemaRef{ID: "orders.output", Version: 1}
	query := contract.PublishedQuery{
		OwnerModuleID: "sales.orders", Name: "mine", Capability: "sales.orders.read", Operation: "list",
		InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned,
		AllowedFields: []string{"id", "owner_id", "amount"}, OwnerField: "owner_id", PageLimit: 10,
		OfflineEligible: true, OfflineFactKey: "id",
	}
	offer := contract.CapabilityOffer{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, Queries: []contract.PublishedQuery{query}, Schemas: []contract.ModuleSchemaDocument{{Ref: in, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: out, Schema: json.RawMessage(`{"type":"array"}`)}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 9, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: scope.TenantID, ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleActive)}}}
	registration := plugin.ProviderRegistration{
		TenantID: scope.TenantID, ProviderID: "engine.published-facts.sales.orders", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal,
		Offers: []contract.CapabilityOffer{offer}, Queries: map[string]plugin.PublishedQueryExecutor{query.Name: provider},
		ValidateSchema: func(_ contract.SchemaRef, raw []byte) error {
			if !json.Valid(raw) {
				return fmt.Errorf("invalid JSON")
			}
			return nil
		},
	}
	resolve := func(got contract.BusinessScope) (plugin.BindingPlan, error) {
		return plugin.ResolveModules(got, []contract.ModuleDescriptor{module}, []plugin.ProviderRegistration{registration}, nil, snapshot.Revision)
	}
	input := json.RawMessage(`{"status":"open"}`)
	put := func(row string) {
		t.Helper()
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := contract.NewModuleExecution(scope, module.ID, module.ID, snapshot.Revision, query.Operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.PutFactTx(ctx, tx, plugin.FactStream{Execution: execution, Query: query, Input: input}, json.RawMessage(row)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put(`{"id":"o-1","owner_id":"alice","amount":1}`)
	put(`{"id":"o-2","owner_id":"alice","amount":2}`)
	put(`{"id":"private","owner_id":"bob","amount":99}`)

	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.mine", Role: "Sales", Read: true, IfOwner: true, Scope: scope})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "alice", SubjectUserID: "alice", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Sales"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace(string(scope.TenantID), SiteRuntimeService{LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil }, ResolveModuleBindings: resolve})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services

	invoke := func(path string, body any, handle func(*gin.Context)) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(recorder)
		requestContext.Request = httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
		requestContext.Request.Header.Set("X-Kora-Legal-Entity-ID", scope.LegalEntityID)
		requestContext.Set("site_name", string(scope.TenantID))
		requestContext.Set("policy_execution", execution)
		handle(requestContext)
		return recorder
	}
	source := contract.OfflineSourceRef{OwnerModuleID: module.ID, QueryName: query.Name, InputSchema: in, Input: input}
	capabilitiesRecorder := invoke("/api/v2/offline/capabilities", nil, handler.HandleOfflineCapabilitiesV2)
	if capabilitiesRecorder.Code != 200 {
		t.Fatalf("capabilities status=%d body=%s", capabilitiesRecorder.Code, capabilitiesRecorder.Body.String())
	}
	var capabilities struct {
		Data contract.OfflineCapabilities `json:"data"`
	}
	if err := json.Unmarshal(capabilitiesRecorder.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Data.Validate(); err != nil {
		t.Fatalf("capabilities failed contract validation: %v", err)
	}
	if len(capabilities.Data.OfflineQueries) != 1 || capabilities.Data.OfflineQueries[0].OwnerModuleID != module.ID || capabilities.Data.OfflineQueries[0].Name != query.Name || len(capabilities.Data.OfflineOperations) != 0 {
		t.Fatalf("offline capabilities do not match the actor-scoped replayable query: %+v", capabilities.Data)
	}
	firstRecorder := invoke("/api/v2/offline/bootstrap", contract.OfflineBootstrapRequest{Source: source, BusinessScope: scope, CatalogRevision: snapshot.Revision, Limit: 1}, handler.HandleOfflineBootstrapV2)
	if firstRecorder.Code != 200 {
		t.Fatalf("first bootstrap status=%d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	var first struct {
		Data contract.OfflineSnapshotPage `json:"data"`
	}
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Data.Complete || first.Data.NextPageCursor == "" || first.Data.SnapshotToken == "" || first.Data.ReplayPosition == "" || string(first.Data.Rows) != `[{"id":"o-1","owner_id":"alice","amount":1}]` {
		t.Fatalf("first bootstrap = %+v", first.Data)
	}

	put(`{"id":"o-3","owner_id":"alice","amount":3}`)
	secondRecorder := invoke("/api/v2/offline/bootstrap", contract.OfflineBootstrapRequest{Source: source, BusinessScope: scope, CatalogRevision: snapshot.Revision, Limit: 1, SnapshotToken: first.Data.SnapshotToken, PageCursor: first.Data.NextPageCursor, InputHash: first.Data.InputHash}, handler.HandleOfflineBootstrapV2)
	if secondRecorder.Code != 200 {
		t.Fatalf("second bootstrap status=%d body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	var second struct {
		Data contract.OfflineSnapshotPage `json:"data"`
	}
	if err := json.Unmarshal(secondRecorder.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if !second.Data.Complete || second.Data.SnapshotToken != first.Data.SnapshotToken || second.Data.ReplayPosition != first.Data.ReplayPosition || string(second.Data.Rows) != `[{"id":"o-2","owner_id":"alice","amount":2}]` {
		t.Fatalf("second bootstrap = %+v", second.Data)
	}

	pullRecorder := invoke("/api/v2/offline/pull", contract.OfflinePullRequest{Source: source, BusinessScope: scope, CatalogRevision: snapshot.Revision, SnapshotToken: first.Data.SnapshotToken, InputHash: first.Data.InputHash, ReplayPosition: first.Data.ReplayPosition, Limit: 10}, handler.HandleOfflinePullV2)
	if pullRecorder.Code != 200 {
		t.Fatalf("pull status=%d body=%s", pullRecorder.Code, pullRecorder.Body.String())
	}
	var pull struct {
		Data contract.OfflinePullResponse `json:"data"`
	}
	if err := json.Unmarshal(pullRecorder.Body.Bytes(), &pull); err != nil {
		t.Fatal(err)
	}
	if pull.Data.HasMore || len(pull.Data.Changes) != 1 || pull.Data.Changes[0].FactKey != "o-3" || strings.Contains(pullRecorder.Body.String(), "private") {
		t.Fatalf("pull = %+v body=%s", pull.Data, pullRecorder.Body.String())
	}
}
