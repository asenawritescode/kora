package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

type historicalPublishedQueryExecutor struct{ result contract.QueryResult }

func (e historicalPublishedQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, json.RawMessage, string, uint16) (contract.QueryResult, error) {
	return e.result, nil
}

func TestAuthorizePublishedQueryRequiresActorGrantAndExactScope(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "totals", ScopePolicy: contract.ScopeEntityOwned}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	if authErr := authorizePublishedQuery(execution, registry, query, scope); authErr == nil {
		t.Fatal("query without module query permission was allowed")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.totals", Role: "Reader", Read: true})
	if authErr := authorizePublishedQuery(execution, registry, query, scope); authErr == nil {
		t.Fatal("tenant role permission authorized entity-scoped query")
	}
	query.ScopePolicy = contract.ScopeTenantShared
	if authErr := authorizePublishedQuery(execution, registry, query, scope); authErr != nil {
		t.Fatalf("authorized shared query denied: %v", authErr)
	}
	other := scope
	other.LegalEntityID = "entity-b"
	query.ScopePolicy = contract.ScopeEntityOwned
	entityExecution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalService, Site: "tenant-route"}, []policy.Grant{{Resource: "module.sales.orders.query.totals", Operation: "read", Scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	if authErr := authorizePublishedQuery(entityExecution, registry, query, scope); authErr != nil {
		t.Fatalf("exact entity grant denied: %v", authErr)
	}
	if authErr := authorizePublishedQuery(entityExecution, registry, query, other); authErr == nil {
		t.Fatal("entity grant crossed legal entity boundary")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.totals", Role: "Reader", Read: true, IfOwner: true, Scope: scope})
	ownerScoped := newAnalyticsAuthorizationContext(t, "tenant-a", "tenant-route", []string{"Reader"}, nil)
	ownerExecution, ok := ownerScoped.Get("policy_execution")
	if !ok {
		t.Fatal("verified execution was not installed")
	}
	if authErr := authorizePublishedQuery(ownerExecution.(policy.Execution), registry, query, scope); authErr == nil {
		t.Fatal("owner-scoped role permission allowed a provider result without row-level filtering")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.totals", Role: "AssignedReader", Read: true, IfAssigned: []string{"assigned_to"}, Scope: scope})
	assignedExecution := newAnalyticsAuthorizationContext(t, "tenant-a", "tenant-route", []string{"AssignedReader"}, nil)
	assignedIdentity, ok := assignedExecution.Get("policy_execution")
	if !ok {
		t.Fatal("verified assigned execution was not installed")
	}
	if authErr := authorizePublishedQuery(assignedIdentity.(policy.Execution), registry, query, scope); authErr == nil {
		t.Fatal("assignment-scoped role permission allowed a query without provider row filtering")
	}
}

func TestPublishedQueryRouteDeniesUnverifiedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, nil)
	request := httptest.NewRequest("POST", "/api/modules/sales.orders/queries/totals", nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "module", Value: "sales.orders"}, {Key: "query", Value: "totals"}}
	handler.HandlePublishedModuleQuery(ctx)
	if recorder.Code != 401 {
		t.Fatalf("unverified query status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPublishedQueryRemainsAvailableForDisabledModuleHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "tenant-history", Kind: contract.ScopeKindTenant}
	inputSchema := contract.SchemaRef{ID: "sales.history.input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "sales.history.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "history", Capability: "sales.history", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id", "amount"}, PageLimit: 10}
	operation := contract.OperationSpec{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 9, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: scope.TenantID, ModuleID: module.ID, Version: module.Version, ArtifactDigest: "retained-artifact", State: string(contract.ModuleDisabled), Revision: 4}}}
	result := contract.QueryResult{Data: json.RawMessage(`[{"id":"invoice-archived","amount":"12.50"}]`), SourceRevision: "history-r9", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	provider := plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sales.internal", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: module.Provides, Queries: map[string]plugin.PublishedQueryExecutor{"history": historicalPublishedQueryExecutor{result: result}}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
		if !json.Valid(data) {
			return errors.New("invalid query JSON")
		}
		return nil
	}}
	selection := plugin.ProviderSelection{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
	services := SiteRuntimeService{ResolveModuleBindings: func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
		if requested != scope || snapshot.Installations[0].State != string(contract.ModuleDisabled) {
			t.Fatalf("historical query resolved against unexpected lifecycle state or scope: %+v", requested)
		}
		return plugin.ResolveModules(requested, snapshot.Modules, []plugin.ProviderRegistration{provider}, []plugin.ProviderSelection{selection}, snapshot.Revision)
	}}
	serviceRegistry := NewSiteRuntimeServices()
	serviceRegistry.Replace(string(scope.TenantID), services)
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.history", Role: "Reader", Read: true})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = serviceRegistry
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Reader"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/api/modules/sales.orders/queries/history", strings.NewReader(`{"input":{}}`))
	ctx.Params = gin.Params{{Key: "module", Value: string(module.ID)}, {Key: "query", Value: query.Name}}
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandlePublishedModuleQuery(ctx)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "invoice-archived") {
		t.Fatalf("authorized historical query after module disable = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPublishedProjectionAuthorizationRequiresExactScopeAndNoRowScope(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	definition := contract.ProjectionDefinition{ID: "ar_balance", OwnerModuleID: "finance.ar", Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "balances", OutputSchema: contract.SchemaRef{ID: "ar.output", Version: 1}}}, ScopePolicy: contract.ScopeEntityOwned, FactKey: "amount", AllowedFields: []string{"amount"}, OutputSchema: contract.SchemaRef{ID: "ar.output", Version: 1}, RebuildStrategy: "consistent_snapshot"}
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalService, Site: "tenant-route"}, []policy.Grant{{Resource: "module.finance.ar.projection.ar_balance", Operation: "read", Scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	if !authorizePublishedProjection(execution, registry, definition, scope, contract.ScopeEntityOwned, "read") {
		t.Fatal("exact projection scope grant was denied")
	}
	other := scope
	other.LegalEntityID = "entity-b"
	if authorizePublishedProjection(execution, registry, definition, other, contract.ScopeEntityOwned, "read") {
		t.Fatal("projection scope grant crossed legal entity boundary")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.projection.ar_balance", Role: "Reader", Read: true, IfOwner: true, Scope: scope})
	roleExecution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	if authorizePublishedProjection(roleExecution, registry, definition, scope, contract.ScopeEntityOwned, "read") {
		t.Fatal("owner-scoped permission authorized aggregate projection access")
	}
}
