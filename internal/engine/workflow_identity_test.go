package engine

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
)

type workflowIdentityStub struct {
	execution policy.Execution
	err       error
	checked   []policy.Grant
}

func (s *workflowIdentityStub) ForWorkflow(context.Context, contract.TenantID, contract.InstanceID) (policy.Execution, error) {
	return s.execution, s.err
}
func (s *workflowIdentityStub) RequireActionGrants(_ context.Context, _ contract.TenantID, grants []policy.Grant) error {
	s.checked = append([]policy.Grant(nil), grants...)
	return s.err
}

func TestEngineWorkflowIdentityBindsTenantAndTrustedGrants(t *testing.T) {
	grant := policy.Grant{Resource: "Invoice", Operation: "submit"}
	provider, err := newEngineWorkflowIdentity("tenant-a", "tenant-a.example.test", []policy.Grant{grant})
	if err != nil {
		t.Fatal("mint workflow identity:", err)
	}
	execution, err := provider.ForWorkflow(context.Background(), "tenant-a", contract.InstanceID(contract.NewID()))
	if err != nil {
		t.Fatal("resolve workflow identity:", err)
	}
	if !execution.Valid() || execution.Tenant() != "tenant-a" || execution.Actor().PrincipalType != contract.PrincipalService || execution.Actor().PrincipalID != "kora-workflow-runtime" || execution.Actor().Site != "tenant-a.example.test" || !execution.Allows(grant.Resource, grant.Operation) {
		t.Fatalf("unexpected Engine workflow identity: tenant=%q actor=%+v", execution.Tenant(), execution.Actor())
	}
	if _, err := provider.ForWorkflow(context.Background(), "tenant-b", contract.InstanceID(contract.NewID())); err == nil {
		t.Fatal("Engine identity crossed tenant boundary")
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-a", []policy.Grant{grant}); err != nil {
		t.Fatalf("registered action grant was not accepted: %v", err)
	}
}

func TestScopedCompatibilityWorkflowIdentityReplacesInjectedGrants(t *testing.T) {
	allowed := policy.Grant{Resource: "Invoice", Operation: "submit"}
	forbidden := policy.Grant{Resource: "Payroll", Operation: "read"}
	actor := contract.ActorContext{PrincipalID: "legacy-workflow", PrincipalType: contract.PrincipalService, Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecutionWithGrants("tenant-a", actor, []policy.Grant{allowed, forbidden})
	if err != nil {
		t.Fatal("create compatibility identity:", err)
	}
	delegate := &workflowIdentityStub{execution: execution}
	provider, err := newScopedWorkflowIdentity(delegate, "tenant-a", actor.Site, []policy.Grant{allowed})
	if err != nil {
		t.Fatal("scope compatibility identity:", err)
	}
	got, err := provider.ForWorkflow(context.Background(), "tenant-a", contract.InstanceID(contract.NewID()))
	if err != nil {
		t.Fatal("resolve scoped compatibility identity:", err)
	}
	if !got.Allows(allowed.Resource, allowed.Operation) || got.Allows(forbidden.Resource, forbidden.Operation) {
		t.Fatalf("compatibility identity grants were not narrowed: actor=%+v", got.Actor())
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-a", []policy.Grant{forbidden}); err == nil {
		t.Fatal("compatibility provider accepted a grant outside trusted registration")
	}
	if err := provider.RequireActionGrants(context.Background(), "tenant-a", []policy.Grant{allowed}); err != nil {
		t.Fatalf("compatibility provider rejected trusted grant: %v", err)
	}
}

func TestScopedCompatibilityWorkflowIdentityRejectsHumanAndRoleIdentities(t *testing.T) {
	for _, actor := range []contract.ActorContext{
		{PrincipalID: "user-a", PrincipalType: contract.PrincipalHuman, SubjectUserID: "user-a", Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()},
		{PrincipalID: "service-a", PrincipalType: contract.PrincipalService, Roles: []string{"Administrator"}, Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()},
		{PrincipalID: "service-a", PrincipalType: contract.PrincipalService, DelegationID: "delegated", Site: "tenant-a.example.test", AuthenticatedAt: time.Now().UTC()},
	} {
		execution, err := policy.NewVerifiedExecution("tenant-a", actor)
		if err != nil {
			t.Fatal("create test identity:", err)
		}
		provider, err := newScopedWorkflowIdentity(&workflowIdentityStub{execution: execution}, "tenant-a", "tenant-a.example.test", nil)
		if err != nil {
			t.Fatal("construct scoped identity:", err)
		}
		if _, err := provider.ForWorkflow(context.Background(), "tenant-a", contract.InstanceID(contract.NewID())); err == nil {
			t.Fatalf("accepted unsafe workflow actor: %+v", actor)
		}
	}
}

var _ workflow.ExecutionIdentityProvider = (*scopedWorkflowIdentity)(nil)
