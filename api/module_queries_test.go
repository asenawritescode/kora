package api

import (
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

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
