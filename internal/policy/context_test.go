package policy

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestExecutionRequiresTenantBoundAuthenticatedActor(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Reader"}}
	exec, err := NewVerifiedExecution("tenant-a", actor)
	if err != nil || !exec.Valid() {
		t.Fatalf("NewVerifiedExecution = %#v, %v", exec, err)
	}
	actorView := exec.Actor()
	actorView.Roles[0] = "Administrator"
	if exec.Actor().Roles[0] != "Reader" {
		t.Fatal("execution exposed mutable role storage")
	}
	stable, err := NewVerifiedExecution("stable-id-42", actor)
	if err != nil || stable.Tenant() != "stable-id-42" || stable.Actor().Site != "tenant-a" {
		t.Fatalf("stable tenant ID was conflated with tenant route name: %#v, %v", stable, err)
	}
	if _, err := NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user", PrincipalType: contract.PrincipalHuman}); err == nil {
		t.Fatal("execution accepted an unauthenticated actor")
	}
}

func TestPrivateModuleResourcesDenyGenericReads(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Private Invoice", ModuleOwnerID: "finance.ar", ModuleAccess: contract.ModulePrivate})
	execution, err := NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"read", "report", "export"} {
		if _, authErr := Decide(execution, registry, "Private Invoice", operation); authErr == nil {
			t.Errorf("private resource %s bypassed generic %s authorization", "Private Invoice", operation)
		}
		if _, authErr := DecideForBusinessScope(execution, registry, "Private Invoice", operation, contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, true); authErr == nil {
			t.Errorf("private resource %s bypassed scoped %s authorization", "Private Invoice", operation)
		}
	}
}

func TestExecutionCannotBeConstructedFromJSON(t *testing.T) {
	var exec Execution
	if err := json.Unmarshal([]byte(`{"tenant":"tenant-a","actor":{"principal_id":"user-1","principal_type":"human","site":"tenant-a"}}`), &exec); err != nil {
		t.Fatal(err)
	}
	if exec.Valid() {
		t.Fatal("JSON created a valid authenticated execution")
	}
}

func TestExecutionGrantsAreValidatedCopiedAndExact(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "extension:reports", PrincipalType: contract.PrincipalService, Site: "tenant-a"}
	grants := []Grant{{Resource: "Invoice", Operation: "read"}, {Resource: "Invoice", Operation: "read"}}
	exec, err := NewVerifiedExecutionWithGrants("tenant-a", actor, grants)
	if err != nil {
		t.Fatal(err)
	}
	grants[0].Resource = "Task"
	if !exec.Allows("Invoice", "read") || exec.Allows("Task", "read") || exec.Allows("Invoice", "delete") {
		t.Fatal("execution grant was mutable, wildcarded, or authorized a different operation")
	}
	grantSnapshot := exec.Grants()
	grantSnapshot[0].Resource = "Other"
	if !exec.Allows("Invoice", "read") || exec.Allows("Other", "read") {
		t.Fatal("execution exposed mutable grant storage")
	}
	for _, invalid := range []Grant{{Resource: "Invoice", Operation: "*"}, {Resource: " ", Operation: "read"}, {Resource: "Invoice", Operation: " read"}, {Resource: "Invoice\x00Secret", Operation: "read"}} {
		if _, err := NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{invalid}); err == nil {
			t.Fatalf("invalid delegated grant %#v accepted", invalid)
		}
	}
}

func TestExecutionGrantForPreservesRestrictiveOwnerScope(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "extension:reports", PrincipalType: contract.PrincipalService, Site: "tenant-a"}
	execution, err := NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{
		{Resource: "Invoice", Operation: "read"},
		{Resource: "Invoice", Operation: "read", OwnerOnly: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, ok := execution.GrantFor("Invoice", "read")
	if !ok || !grant.OwnerOnly {
		t.Fatalf("GrantFor returned (%#v, %v), want owner-scoped grant", grant, ok)
	}
	if _, ok := execution.GrantFor("Invoice", "write"); ok {
		t.Fatal("GrantFor inherited grant across operations")
	}
}

func TestEntityGrantIsExactAndTenantRoleDoesNotImplyEntityAccess(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"}}
	scopeA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	roles := testRolePolicy{"Administrator": {"Invoice:read": true, "Invoice:create": true, "Invoice:write": true}}
	execution, err := NewVerifiedExecution("tenant-a", actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecideForBusinessScope(execution, roles, "Invoice", "read", scopeA, true); err == nil {
		t.Fatal("tenant-wide role granted entity access without an entity grant")
	}
	execution, err = NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{{Resource: "Invoice", Operation: "read", Scope: scopeA}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecideForBusinessScope(execution, roles, "Invoice", "read", scopeA, true); err != nil {
		t.Fatalf("exact entity grant denied: %v", err)
	}
	if _, err := DecideForBusinessScope(execution, roles, "Invoice", "write", scopeA, true); err == nil {
		t.Fatal("read grant authorized write")
	}
	if _, err := DecideForBusinessScope(execution, roles, "Invoice", "read", scopeB, true); err == nil {
		t.Fatal("entity grant crossed to another entity")
	}
	crossTenant := contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	if _, err := NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{{Resource: "Invoice", Operation: "read", Scope: crossTenant}}); err == nil {
		t.Fatal("grant for another tenant's entity scope was accepted")
	}
}

func TestScopedGrantCannotAuthorizeTenantScope(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "service-1", PrincipalType: contract.PrincipalService, Site: "tenant-a"}
	entity := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{{Resource: "Invoice", Operation: "read", Scope: entity}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Allows("Invoice", "read") {
		t.Fatal("entity grant authorized unscoped tenant operation")
	}
	if _, ok := execution.GrantFor("Invoice", "read"); ok {
		t.Fatal("entity grant was returned as tenant grant")
	}
	if _, ok := execution.GrantForScope("Invoice", "read", entity); !ok {
		t.Fatal("exact entity grant was not available")
	}
}

func TestCrossEntityOperationRequiresBothExactEntityGrants(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "service-1", PrincipalType: contract.PrincipalService, Site: "tenant-a"}
	entityA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	entityB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	operation := contract.CrossEntityOperation{OperationID: "intercompany.transfer", TenantID: "tenant-a", SourceEntityID: "entity-a", TargetEntityID: "entity-b", Resource: "Intercompany Transfer", Operation: "write"}
	for _, grants := range [][]Grant{
		{{Resource: operation.Resource, Operation: operation.Operation, Scope: entityA}},
		{{Resource: operation.Resource, Operation: operation.Operation}},
	} {
		execution, err := NewVerifiedExecutionWithGrants("tenant-a", actor, grants)
		if err != nil {
			t.Fatal(err)
		}
		if AuthorizeCrossEntityOperation(execution, operation) == nil {
			t.Fatal("cross-entity command accepted without both scoped grants")
		}
	}
	execution, err := NewVerifiedExecutionWithGrants("tenant-a", actor, []Grant{
		{Resource: operation.Resource, Operation: operation.Operation, Scope: entityA},
		{Resource: operation.Resource, Operation: operation.Operation, Scope: entityB},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeCrossEntityOperation(execution, operation); err != nil {
		t.Fatalf("dual-authorized operation denied: %v", err)
	}
	operation.TargetEntityID = "entity-a"
	if AuthorizeCrossEntityOperation(execution, operation) == nil {
		t.Fatal("same-entity cross-entity command accepted")
	}
	operation.TargetEntityID = "entity-b"
	registry := doctype.NewRegistry()
	registry.Permissions.LoadPermissionsFromDB([]*doctype.Role{{Name: "Source"}, {Name: "Target"}}, []*doctype.Permission{
		{Role: "Source", Doctype: operation.Resource, Write: true, Scope: entityA},
		{Role: "Target", Doctype: operation.Resource, Write: true, Scope: entityB},
	})
	roleExecution, err := NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Source", "Target"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeCrossEntityOperation(roleExecution, operation, registry); err != nil {
		t.Fatalf("explicit dual-role operation denied: %v", err)
	}
	missingTarget, err := NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Source"}})
	if err != nil {
		t.Fatal(err)
	}
	if AuthorizeCrossEntityOperation(missingTarget, operation, registry) == nil {
		t.Fatal("one entity role authorized both sides")
	}
}

func TestEntityScopedRolePermissionIsNotTenantRoleInheritance(t *testing.T) {
	registry := doctype.NewRegistry()
	registry.Permissions.LoadPermissionsFromDB([]*doctype.Role{{Name: "Manager"}}, []*doctype.Permission{{Role: "Manager", Doctype: "Invoice", Read: true, Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}}})
	actor := contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Manager"}}
	execution, err := NewVerifiedExecution("tenant-a", actor)
	if err != nil {
		t.Fatal(err)
	}
	scopeA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	if _, err := DecideForBusinessScope(execution, registry, "Invoice", "read", scopeA, true); err != nil {
		t.Fatalf("explicit scoped role grant denied: %v", err)
	}
	if _, err := DecideForBusinessScope(execution, registry, "Invoice", "read", scopeB, true); err == nil {
		t.Fatal("entity A role permission authorized entity B")
	}
}

type testRolePolicy map[string]map[string]bool

func (p testRolePolicy) CanUser(roles []string, resource, operation string) (bool, bool) {
	for _, role := range roles {
		if p[role][resource+":"+operation] {
			return true, p[role][resource+":owner"]
		}
	}
	return false, false
}

func TestDecideUsesVerifiedIdentityAndCurrentRolePolicy(t *testing.T) {
	actor := contract.ActorContext{PrincipalID: "reader", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Reader"}}
	execution, err := NewVerifiedExecution("tenant-a", actor)
	if err != nil {
		t.Fatal(err)
	}
	roles := testRolePolicy{"Reader": {"Invoice:read": true, "Invoice:owner": true}}
	decision, decideErr := Decide(execution, roles, "Invoice", "read")
	if decideErr != nil || !decision.Allowed || !decision.OwnerOnly {
		t.Fatalf("Decide = (%#v, %v), want allowed owner-only decision", decision, decideErr)
	}
	decision, decideErr = Decide(execution, roles, "Invoice", "delete")
	if decideErr == nil || decision.Allowed {
		t.Fatalf("unauthorized decision = (%#v, %v), want denial", decision, decideErr)
	}
	if _, decideErr := Decide(Execution{}, roles, "Invoice", "read"); decideErr == nil || decideErr.Type != contract.CodeUnauthenticated {
		t.Fatalf("unverified decision error = %v, want UNAUTHENTICATED", decideErr)
	}
}

func TestIsPrivateModuleResourcePredicate(t *testing.T) {
	registry := doctype.NewRegistry()
	for name, dt := range map[string]*doctype.DocType{
		"Plain":     {Name: "Plain"},
		"Private":   {Name: "Private", ModuleOwnerID: "m.core", ModuleAccess: contract.ModulePrivate},
		"Unset":     {Name: "Unset", ModuleOwnerID: "m.core"},
		"Published": {Name: "Published", ModuleOwnerID: "m.core", ModuleAccess: contract.ModulePublished},
	} {
		registry.Register(dt)
		_ = name
	}
	for name, want := range map[string]bool{"Plain": false, "Private": true, "Unset": true, "Published": false, "Missing": false} {
		if got := registry.IsPrivateModuleResource(name); got != want {
			t.Errorf("IsPrivateModuleResource(%s) = %v, want %v", name, got, want)
		}
	}
	execution, err := NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "r", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Published", Role: "Administrator", Read: true})
	if _, authErr := Decide(execution, registry, "Published", "read"); authErr != nil {
		t.Errorf("published owned read denied: %v", authErr)
	}
}
