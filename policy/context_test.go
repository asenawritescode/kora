package policy

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
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
