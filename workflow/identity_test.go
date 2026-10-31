package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

func TestTenantServiceIdentityProviderBindsServiceIdentityAndTenant(t *testing.T) {
	provider, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{{Resource: "Invoice", Operation: "submit"}})
	if err != nil {
		t.Fatal("construct provider:", err)
	}
	exec, err := provider.ForWorkflow(context.Background(), "tenant-a", "01K7INSTANCE00000000000000000")
	if err != nil {
		t.Fatal("resolve workflow execution:", err)
	}
	if !exec.Valid() || exec.Tenant() != "tenant-a" || exec.Actor().PrincipalType != contract.PrincipalService || !exec.Allows("Invoice", "submit") {
		t.Fatalf("unexpected workflow execution identity: tenant=%q actor=%+v", exec.Tenant(), exec.Actor())
	}
	if _, err := provider.ForWorkflow(context.Background(), "tenant-b", "01K7INSTANCE00000000000000000"); err == nil {
		t.Fatal("provider returned an identity for another tenant")
	}
	if _, err := provider.ForWorkflow(context.Background(), "tenant-a", ""); err == nil {
		t.Fatal("provider accepted an invalid instance ID")
	}
}

func TestTenantServiceIdentityProviderRejectsHumanAndDelegatedActors(t *testing.T) {
	base := contract.ActorContext{PrincipalID: "runtime", PrincipalType: contract.PrincipalHuman, Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()}
	if _, err := NewTenantServiceIdentityProvider("tenant-a", base, nil); err == nil {
		t.Fatal("accepted human workflow execution identity")
	}
	base.PrincipalType = contract.PrincipalService
	base.SubjectUserID = "user-1"
	if _, err := NewTenantServiceIdentityProvider("tenant-a", base, nil); err == nil {
		t.Fatal("accepted delegated workflow execution identity")
	}
}

func TestTenantServiceIdentityProviderHonorsCancellation(t *testing.T) {
	provider, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.ForWorkflow(ctx, "tenant-a", "01K7INSTANCE00000000000000000"); err == nil {
		t.Fatal("identity lookup ignored cancellation")
	}
}

func TestTenantServiceIdentityProviderRequiresEveryRegisteredActionGrant(t *testing.T) {
	grant := testActionGrant()
	provider, err := NewTenantServiceIdentityProvider("tenant-a", contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService,
		Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{grant})
	if err != nil {
		t.Fatal("construct provider:", err)
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-a", []policy.Grant{grant}); err != nil {
		t.Fatalf("required registered grant was rejected: %v", err)
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-a", []policy.Grant{grant, {Resource: "workflow.action.missing", Operation: "submit"}}); err == nil {
		t.Fatal("provider accepted an action grant it does not hold")
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-b", []policy.Grant{grant}); err == nil {
		t.Fatal("provider accepted grants for another tenant")
	}
}
