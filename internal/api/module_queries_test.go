package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

type historicalPublishedQueryExecutor struct{ result contract.QueryResult }

func (e historicalPublishedQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, plugin.QueryPageRequest) (contract.QueryResult, error) {
	return e.result, nil
}

func TestAuthorizePublishedQueryRequiresActorGrantAndExactScope(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "totals", ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"assigned_to"}}
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
	actorScope, authErr := authorizePublishedQueryActorScope(assignedIdentity.(policy.Execution), registry, query, scope)
	if authErr != nil || actorScope == nil || actorScope.SubjectID == "" || len(actorScope.AssignedFields) != 1 || actorScope.AssignedFields[0] != "assigned_to" {
		t.Fatalf("assignment-scoped role did not produce trusted provider scope: scope=%#v err=%v", actorScope, authErr)
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
	if !(projectionAccess{Execution: execution, Roles: registry, Definition: definition, Scope: scope, ScopePolicy: contract.ScopeEntityOwned}).allows("read") {
		t.Fatal("exact projection scope grant was denied")
	}
	other := scope
	other.LegalEntityID = "entity-b"
	if (projectionAccess{Execution: execution, Roles: registry, Definition: definition, Scope: other, ScopePolicy: contract.ScopeEntityOwned}).allows("read") {
		t.Fatal("projection scope grant crossed legal entity boundary")
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.projection.ar_balance", Role: "Reader", Read: true, IfOwner: true, Scope: scope})
	roleExecution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	if (projectionAccess{Execution: roleExecution, Roles: registry, Definition: definition, Scope: scope, ScopePolicy: contract.ScopeEntityOwned}).allows("read") {
		t.Fatal("owner-scoped permission authorized aggregate projection access")
	}
}

func TestPublishedProjectionActorScopeFiltersRawRows(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	definition := contract.ProjectionDefinition{ID: "ar_balance", OwnerModuleID: "finance.ar", Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "balances", OutputSchema: contract.SchemaRef{ID: "ar.output", Version: 1}}}, ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"assigned_to", "id", "amount"}, OutputSchema: contract.SchemaRef{ID: "ar.output", Version: 1}, RebuildStrategy: "consistent_snapshot"}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.finance.ar.projection.ar_balance", Role: "Reader", Read: true, IfAssigned: []string{"assigned_to"}, Scope: scope})
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "alice", SubjectUserID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("verified actor:", err)
	}
	actorScope, authErr := projectionAccess{Execution: execution, Roles: registry, Definition: definition, Scope: scope, ScopePolicy: contract.ScopeEntityOwned}.actorScope("read")
	if authErr != nil || actorScope == nil || len(actorScope.AssignedFields) != 1 || actorScope.AssignedFields[0] != "assigned_to" {
		t.Fatalf("authorized projection actor scope = %#v, %v", actorScope, authErr)
	}
	data, err := filterPublishedProjectionRows(json.RawMessage(`[{"id":"mine","assigned_to":"alice","amount":12},{"id":"theirs","assigned_to":"bob","amount":99}]`), *actorScope)
	if err != nil || string(data) != `[{"amount":12,"assigned_to":"alice","id":"mine"}]` {
		t.Fatalf("filtered projection rows = %s, error=%v", data, err)
	}
}

func TestApplicationCatalogFiltersSurfacesByActorGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	scope := contract.BusinessScope{TenantID: "catalog-tenant", Kind: contract.ScopeKindTenant}
	in, out := contract.SchemaRef{ID: "catalog.input", Version: 1}, contract.SchemaRef{ID: "catalog.output", Version: 1}
	checkoutIn, checkoutOut := contract.SchemaRef{ID: "checkout.input", Version: 1}, contract.SchemaRef{ID: "checkout.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "list", Capability: "sales.orders", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 10}
	query.ReferenceLookup = &contract.PublishedReferenceLookup{ResourceType: "sales.order", SearchField: "search", ValueField: "business_ref", LabelField: "number"}
	query.AllowedFields = []string{"id", "business_ref", "number"}
	projection := contract.ProjectionDefinition{ID: "history", OwnerModuleID: "sales.orders", Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "list", OutputSchema: out}}, ScopePolicy: contract.ScopeTenantShared, FactKey: "id", AllowedFields: []string{"id"}, OutputSchema: out, RebuildStrategy: "consistent_snapshot"}
	checkoutContract := &contract.CheckoutOperationContract{Input: contract.CheckoutInputBinding{CartItems: "lines", RequestedTotal: "amount", Currency: "currency", PaymentMethod: "method"}, Outcomes: []contract.CheckoutOutcomeBinding{{Kind: contract.CheckoutPaymentSettled, StatusField: "payment_status", SuccessValue: "Settled"}, {Kind: contract.CheckoutInvoiceIssued, StatusField: "invoice_status", SuccessValue: "Issued", ReferenceField: "invoice_ref"}}}
	refSchema := `{"type":"object","properties":{"provider_id":{"type":"string"},"resource_type":{"type":"string"},"id":{"type":"string"},"scope":{"type":"object"}},"required":["provider_id","resource_type","id","scope"]}`
	checkoutInputSchema := json.RawMessage(`{"type":"object","properties":{"lines":{"type":"array"},"amount":{"type":"string"},"currency":{"type":"string"},"method":{"type":"string"}}}`)
	checkoutOutputSchema := json.RawMessage(`{"type":"object","properties":{"payment_status":{"type":"string","enum":["Settled","Pending"]},"invoice_status":{"type":"string","enum":["Issued","Pending"]},"invoice_ref":` + refSchema + `}}`)
	checkoutOperation := contract.PublishedOperation{Name: "checkout", Capability: "sales.checkout", Operation: "submit", ScopePolicy: contract.ScopeTenantShared, AllowedCallers: []contract.ModuleID{"sales.orders"}, Checkout: checkoutContract}
	module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Schemas: []contract.ModuleSchemaDocument{{Ref: in, Schema: json.RawMessage(`{"type":"object"}`)}, {Ref: out, Schema: json.RawMessage(`{"type":"array"}`)}, {Ref: checkoutIn, Schema: checkoutInputSchema}, {Ref: checkoutOut, Schema: checkoutOutputSchema}}, Queries: []contract.PublishedQuery{query}, PublishedOperations: []contract.PublishedOperation{checkoutOperation}, Projections: []contract.ProjectionDefinition{projection}, Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: "list", Input: in, Output: out, Effect: contract.EffectQuery}}}, {Name: checkoutOperation.Capability, Contract: checkoutOut, Operations: []contract.OperationSpec{{Name: checkoutOperation.Operation, Input: checkoutIn, Output: checkoutOut, Effect: contract.EffectExternal}}}}}
	snapshot := contract.ModuleSnapshot{TenantID: scope.TenantID, Revision: 7, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: scope.TenantID, ModuleID: module.ID, Version: module.Version, State: string(contract.ModuleActive)}}}
	plan, err := plugin.ResolveModules(scope, []contract.ModuleDescriptor{module}, []plugin.ProviderRegistration{{TenantID: scope.TenantID, ProviderID: "sales.internal", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: module.Provides, Factories: map[contract.CapabilityName]plugin.ProviderFactory{checkoutOperation.Capability: plugin.NewStaticFactory(checkoutOperation.Capability, nil)}, Queries: map[string]plugin.PublishedQueryExecutor{"list": historicalPublishedQueryExecutor{}}}}, nil, snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	registry := doctype.NewRegistry()
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.list", Role: "Reader", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.projection.history", Role: "Reader", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.operation.checkout", Role: "Reader", Write: true})
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Reader"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace(string(scope.TenantID), SiteRuntimeService{LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) { return snapshot, nil }, ResolveModuleBindings: func(got contract.BusinessScope) (plugin.BindingPlan, error) {
		if got != scope {
			t.Fatalf("scope=%+v", got)
		}
		return plan, nil
	}})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/v2/modules/catalog", nil)
	ctx.Set("site_name", string(scope.TenantID))
	ctx.Set("policy_execution", execution)
	handler.HandleApplicationCatalog(ctx)
	if recorder.Code != 200 {
		t.Fatalf("catalog status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data contract.ApplicationCatalog `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.PublishedQueries) != 1 || response.Data.PublishedQueries[0].Route != "/api/v1/modules/sales.orders/queries/list" || response.Data.PublishedQueries[0].Capability != query.Capability || response.Data.PublishedQueries[0].ReferenceLookup == nil || *response.Data.PublishedQueries[0].ReferenceLookup != *query.ReferenceLookup || len(response.Data.PublishedProjections) != 1 || response.Data.PublishedProjections[0].Route != "/api/v1/modules/sales.orders/projections/history" || response.Data.Revision != 7 || len(response.Data.Schemas) != 4 || len(response.Data.PublishedOperations) != 1 || response.Data.PublishedOperations[0].Checkout == nil || !reflect.DeepEqual(response.Data.PublishedOperations[0].Checkout, checkoutContract) {
		t.Fatalf("unexpected actor catalog: %+v", response.Data)
	}
}

func TestApplicationCatalogRejectsUnauthorizedLegalEntity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	execution, err := policy.NewVerifiedExecution("catalog-tenant", contract.ActorContext{
		PrincipalID: "extension:scoped", PrincipalType: contract.PrincipalService, Site: "catalog-tenant",
		AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("verified actor:", err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("catalog-tenant", SiteRuntimeService{
		LoadModuleSnapshot: func() (contract.ModuleSnapshot, error) {
			return contract.ModuleSnapshot{TenantID: "catalog-tenant", Revision: 3}, nil
		},
		ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) {
			return plugin.BindingPlan{}, nil
		},
	})
	handler := NewHandler(registry, nil)
	handler.RuntimeServices = services
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/api/v2/modules/catalog", nil)
	ctx.Request.Header.Set("X-Kora-Legal-Entity-ID", "entity-not-granted")
	ctx.Set("site_name", "catalog-tenant")
	ctx.Set("policy_execution", execution)

	handler.HandleApplicationCatalog(ctx)
	if recorder.Code != 403 {
		t.Fatalf("catalog status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"code":"permission.denied"`) {
		t.Fatalf("catalog body=%s", recorder.Body.String())
	}
}

func TestAuthorizePublishedQueryRequiresSourceDocTypeRead(t *testing.T) {
	registry := doctype.NewRegistry()
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "totals", ScopePolicy: contract.ScopeTenantShared}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.totals", Role: "Reader", Read: true})
	registry.SetQuerySource("module.sales.orders.query.totals", "Sale")
	authErr := authorizePublishedQuery(execution, registry, query, scope)
	if authErr == nil {
		t.Fatalf("query without source read allowed: %v", authErr)
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Sale", Role: "Reader", Read: true})
	if authErr := authorizePublishedQuery(execution, registry, query, scope); authErr != nil {
		t.Fatalf("query with source read denied: %v", authErr)
	}
}

// A child-source query is authorized on its parent DocType (Sale), not the child.
func TestAuthorizePublishedQueryChildSourceChecksParent(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	q := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "z", ScopePolicy: contract.ScopeTenantShared}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		perms []*doctype.Permission
		deny  bool
	}{
		{"parent read only", []*doctype.Permission{{Doctype: "Sale", Role: "Reader", Read: true}}, false},
		{"no parent read", []*doctype.Permission{{Doctype: "Sale Payment", Role: "Reader", Read: true}}, true},
		{"owner-only parent", []*doctype.Permission{{Doctype: "Sale", Role: "Reader", Read: true, IfOwner: true}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := doctype.NewRegistry()
			registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.z", Role: "Reader", Read: true})
			for _, p := range tt.perms {
				registry.Permissions.SetPermission(p)
			}
			registry.SetQuerySource("module.sales.orders.query.z", "Sale")
			if got := authorizePublishedQuery(execution, registry, q, scope); (got != nil) != tt.deny {
				t.Fatalf("deny=%v err=%v", tt.deny, got)
			}
		})
	}
}

// D7: a query over its own module's DocType needs only the query grant.
func TestAuthorizePublishedQueryOwnerSourceSkipsSourceRead(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	q := contract.PublishedQuery{OwnerModuleID: "inventory.core", Name: "item_search", ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"name", "price"}}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "c", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Cashier"}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		owner string
		grant bool
		deny  bool
	}{
		{"owned by query owner", "inventory.core", true, false},
		{"unowned source", "", true, true},
		{"other module source", "pos.core", true, true},
		{"no query grant", "inventory.core", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := doctype.NewRegistry()
			registry.Register(&doctype.DocType{Name: "Item", ModuleOwnerID: tt.owner})
			if tt.grant {
				registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.inventory.core.query.item_search", Role: "Cashier", Read: true})
			}
			registry.SetQuerySource("module.inventory.core.query.item_search", "Item")
			if got := authorizePublishedQuery(execution, registry, q, scope); (got != nil) != tt.deny {
				t.Fatalf("deny=%v err=%v", tt.deny, got)
			}
		})
	}
}

type failingPublishedQueryExecutor struct{ err error }

func (e failingPublishedQueryExecutor) ExecutePublishedQuery(context.Context, contract.ModuleExecution, contract.PublishedQuery, plugin.QueryPageRequest) (contract.QueryResult, error) {
	return contract.QueryResult{}, e.err
}

// TestPublishedQueryCallerMistakesAre4xx covers #317: a caller's mistake is a
// bounded 400, a provider fault stays a 503 that leaks nothing.
func TestPublishedQueryCallerMistakesAre4xx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		body       string
		execErr    error
		inputErr   error
		wantStatus int
		wantCode   string
		wantField  string
		wantMsg    string
		wantLog    string // underlying error the 503 path must log; empty means nothing is logged
	}{
		{"executor input error", `{"input":{}}`, &plugin.QueryInputError{Field: "since", Message: "input since is required"}, nil, 400, "validation.invalid_query_request", "since", "input since is required", ""},
		{"schema failure hides validator text", `{"input":{}}`, nil, errors.New("jsonschema secret-internal-detail"), 400, "validation.invalid_query_request", "", "query input is invalid", ""},
		{"limit beyond page bound", `{"input":{},"limit":11}`, nil, nil, 400, "validation.invalid_query_request", "limit", "limit", ""},
		{"provider fault stays 503", `{"input":{}}`, errors.New("dial tcp 10.0.0.1: secret-host"), nil, 503, "module.query_failed", "", "Published query could not be completed", "dial tcp 10.0.0.1: secret-host"},
		{"contract violation by the provider is logged", `{"input":{}}`, errors.New("published query result data must be a bounded row array"), nil, 503, "module.query_failed", "", "Published query could not be completed", "bounded row array"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs strings.Builder
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			scope := contract.BusinessScope{TenantID: "tenant-inputs", Kind: contract.ScopeKindTenant}
			inputSchema := contract.SchemaRef{ID: "sales.inputs.input", Version: 1}
			outputSchema := contract.SchemaRef{ID: "sales.inputs.output", Version: 1}
			query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "totals", Capability: "sales.totals", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 10}
			operation := contract.OperationSpec{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
			module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
			provider := plugin.ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sales.internal", OwnerModuleID: module.ID, Kind: plugin.ProviderInternal, Offers: module.Provides, Queries: map[string]plugin.PublishedQueryExecutor{"totals": failingPublishedQueryExecutor{err: tc.execErr}}, ValidateSchema: func(ref contract.SchemaRef, _ []byte) error {
				if ref == inputSchema {
					return tc.inputErr
				}
				return nil
			}}
			selection := plugin.ProviderSelection{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
			services := SiteRuntimeService{ResolveModuleBindings: func(requested contract.BusinessScope) (plugin.BindingPlan, error) {
				return plugin.ResolveModules(requested, []contract.ModuleDescriptor{module}, []plugin.ProviderRegistration{provider}, []plugin.ProviderSelection{selection}, 1)
			}}
			serviceRegistry := NewSiteRuntimeServices()
			serviceRegistry.Replace(string(scope.TenantID), services)
			registry := doctype.NewRegistry()
			registry.Permissions.SetPermission(&doctype.Permission{Doctype: "module.sales.orders.query.totals", Role: "Reader", Read: true})
			handler := NewHandler(registry, nil)
			handler.RuntimeServices = serviceRegistry
			execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), Roles: []string{"Reader"}, AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("POST", "/api/modules/sales.orders/queries/totals", strings.NewReader(tc.body))
			ctx.Params = gin.Params{{Key: "module", Value: "sales.orders"}, {Key: "query", Value: "totals"}}
			ctx.Set("site_name", string(scope.TenantID))
			ctx.Set("policy_execution", execution)
			handler.HandlePublishedModuleQuery(ctx)
			var body struct {
				Error ErrorBody `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", recorder.Body.String(), err)
			}
			if recorder.Code != tc.wantStatus || body.Error.Code != tc.wantCode || !strings.Contains(body.Error.Message, tc.wantMsg) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if field, _ := body.Error.Details["field"].(string); field != tc.wantField {
				t.Fatalf("field = %q, want %q: %s", field, tc.wantField, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "secret") {
				t.Fatalf("internal detail leaked: %s", recorder.Body.String())
			}
			if tc.wantLog == "" && strings.Contains(logs.String(), "published query failed") || !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("log = %q, want it to contain %q", logs.String(), tc.wantLog)
			}
		})
	}
}

func TestApplicationCatalogForTenantWithoutInstalledModules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name         string
		snapshot     contract.ModuleSnapshot
		loadErr      error
		wantStatus   int
		wantRevision uint64
	}{
		{name: "no snapshot row yet is an empty catalog", snapshot: contract.ModuleSnapshot{TenantID: "fresh-tenant"}, wantStatus: 200},
		{name: "snapshot with no installations is an empty catalog", snapshot: contract.ModuleSnapshot{TenantID: "fresh-tenant", Revision: 4}, wantStatus: 200, wantRevision: 4},
		{name: "snapshot of another tenant stays unavailable", snapshot: contract.ModuleSnapshot{TenantID: "other-tenant", Revision: 4}, wantStatus: 503},
		{name: "snapshot read failure stays unavailable", loadErr: errors.New("db down"), wantStatus: 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execution, err := policy.NewVerifiedExecution("fresh-tenant", contract.ActorContext{PrincipalID: "owner", PrincipalType: contract.PrincipalHuman, Site: "fresh-tenant", Roles: []string{"Administrator"}, AuthenticatedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			services := NewSiteRuntimeServices()
			services.Replace("fresh-tenant", SiteRuntimeService{
				LoadModuleSnapshot:    func() (contract.ModuleSnapshot, error) { return tt.snapshot, tt.loadErr },
				ResolveModuleBindings: func(contract.BusinessScope) (plugin.BindingPlan, error) { return plugin.BindingPlan{}, nil },
			})
			handler := NewHandler(doctype.NewRegistry(), nil)
			handler.RuntimeServices = services
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/api/v2/modules/catalog", nil)
			ctx.Set("site_name", "fresh-tenant")
			ctx.Set("policy_execution", execution)
			handler.HandleApplicationCatalog(ctx)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("catalog status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if tt.wantStatus != 200 {
				return
			}
			var response struct {
				Data contract.ApplicationCatalog `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Data.Revision != tt.wantRevision || len(response.Data.Modules) != 0 || len(response.Data.PublishedOperations) != 0 || len(response.Data.PublishedQueries) != 0 || response.Data.BusinessScope.Kind != contract.ScopeKindTenant {
				t.Fatalf("unexpected empty catalog: %+v", response.Data)
			}
		})
	}
}
