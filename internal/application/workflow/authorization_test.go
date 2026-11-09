package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	processworkflow "github.com/asenawritescode/kora/internal/workflow"
)

type authorizationTestStore struct {
	roles       []string
	active      bool
	owner       contract.PrincipalID
	userFields  map[string]string
	revision    contract.Revision
	starter     contract.PrincipalID
	roleErr     error
	subjectErr  error
	starterErr  error
	roleCalls   int
	subjectCall int
}

func (s *authorizationTestStore) UserRoles(context.Context, string, contract.PrincipalID) ([]string, bool, error) {
	s.roleCalls++
	return append([]string(nil), s.roles...), s.active, s.roleErr
}
func (s *authorizationTestStore) Subject(context.Context, contract.SubjectRef) (processworkflow.SubjectAuthorization, error) {
	s.subjectCall++
	return processworkflow.SubjectAuthorization{Owner: s.owner, Revision: s.revision, UserFields: s.userFields}, s.subjectErr
}
func (s *authorizationTestStore) WorkflowStarter(context.Context, contract.TenantID, contract.InstanceID) (contract.PrincipalID, error) {
	return s.starter, s.starterErr
}

func workflowPolicyRegistry() *doctype.Registry {
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "assigned_to", Fieldtype: "User"}}}}, []*doctype.Role{{Name: "Publisher"}, {Name: "Reader"}, {Name: "Owner"}, {Name: "Assigned"}, {Name: "Operator"}}, []*doctype.Permission{
		{Doctype: DefinitionResource, Role: "Publisher", Read: true, Write: true},
		{Doctype: InstanceResource, Role: "Publisher", Create: true},
		{Doctype: InstanceResource, Role: "Operator", Write: true, Cancel: true},
		{Doctype: "Invoice", Role: "Reader", Read: true},
		{Doctype: "Invoice", Role: "Owner", Read: true, IfOwner: true},
		{Doctype: "Invoice", Role: "Assigned", Read: true, IfAssigned: []string{"assigned_to"}},
		{Doctype: "Invoice", Role: "Operator", Read: true},
	})
	return registry
}

func TestWorkflowAuthorizerHonorsAssignedSubjectVisibility(t *testing.T) {
	store := &authorizationTestStore{roles: []string{"Assigned"}, active: true, owner: "creator", userFields: map[string]string{"assigned_to": "user-a"}, revision: "3"}
	authorizer, err := NewAuthorizer(store, workflowPolicyRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.CanReadWorkflow(context.Background(), humanWorkflowExecution(t, "Assigned"), contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}); err != nil || !allowed {
		t.Fatalf("assigned actor cannot read current workflow subject: allowed=%t err=%v", allowed, err)
	}
	store.userFields["assigned_to"] = "other-user"
	if allowed, err := authorizer.CanReadWorkflow(context.Background(), humanWorkflowExecution(t, "Assigned"), contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}); err != nil || allowed {
		t.Fatalf("unassigned actor read workflow subject: allowed=%t err=%v", allowed, err)
	}
}

func humanWorkflowExecution(t *testing.T, roles ...string) policy.Execution {
	t.Helper()
	exec, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user-a", SubjectUserID: "user-a",
		PrincipalType: contract.PrincipalHuman, Site: "tenant-a.example.test", Roles: roles, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create verified human execution:", err)
	}
	return exec
}

func TestWorkflowAuthorizerUsesCurrentMembershipInsteadOfSessionRoles(t *testing.T) {
	store := &authorizationTestStore{roles: []string{"Reader"}, active: true, owner: "user-a", revision: "3"}
	authorizer, err := NewAuthorizer(store, workflowPolicyRegistry())
	if err != nil {
		t.Fatal("construct authorizer:", err)
	}
	// The verified session snapshot says Administrator; live DB membership
	// says Reader, which cannot publish or create workflow instances.
	exec := humanWorkflowExecution(t, doctype.AdminRole)
	if allowed, err := authorizer.CanManageWorkflowDefinitions(context.Background(), exec); err != nil || allowed {
		t.Fatalf("stale session role authorized definition management: allowed=%t err=%v", allowed, err)
	}
	if allowed, err := authorizer.CanStartWorkflow(context.Background(), exec, "invoice-flow", contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}); err != nil || allowed {
		t.Fatalf("missing live create permission authorized workflow start: allowed=%t err=%v", allowed, err)
	}
	if store.roleCalls != 2 {
		t.Fatalf("live membership queried %d times, want once per policy decision", store.roleCalls)
	}
}

func TestWorkflowAuthorizerAppliesCurrentSubjectReadOwnerAndRevisionRules(t *testing.T) {
	store := &authorizationTestStore{roles: []string{"Reader"}, active: true, owner: "user-a", revision: "3"}
	authorizer, _ := NewAuthorizer(store, workflowPolicyRegistry())
	exec := humanWorkflowExecution(t, "Operator")
	if allowed, err := authorizer.CanReadWorkflow(context.Background(), exec, contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "1"}); err != nil || !allowed {
		t.Fatalf("current subject read was denied after subject revision changed: allowed=%t err=%v", allowed, err)
	}
	store.roles = []string{"Owner"}
	store.owner = "other-user"
	if allowed, err := authorizer.CanReadWorkflow(context.Background(), exec, contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}); err != nil || allowed {
		t.Fatalf("owner-scoped read ignored current owner: allowed=%t err=%v", allowed, err)
	}
	store.roles = []string{"Reader", "Publisher"}
	if allowed, err := authorizer.CanStartWorkflow(context.Background(), exec, "invoice-flow", contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "2"}); err == nil || allowed {
		t.Fatalf("start accepted changed subject revision: allowed=%t err=%v", allowed, err)
	}
}

func TestWorkflowAuthorizerRestrictsCancelAndReconciliation(t *testing.T) {
	store := &authorizationTestStore{roles: []string{"Reader"}, active: true, owner: "user-a", revision: "3", starter: "user-a"}
	authorizer, _ := NewAuthorizer(store, workflowPolicyRegistry())
	exec := humanWorkflowExecution(t, "Reader")
	instance := contract.WorkflowInstance{ID: "01K7INSTANCE00000000000000000", Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "3"}}
	if allowed, err := authorizer.CanCancelWorkflow(context.Background(), exec, instance); err != nil || !allowed {
		t.Fatalf("starter cannot cancel readable workflow: allowed=%t err=%v", allowed, err)
	}
	store.starter = "another-user"
	if allowed, err := authorizer.CanCancelWorkflow(context.Background(), exec, instance); err != nil || allowed {
		t.Fatalf("non-starter canceled workflow: allowed=%t err=%v", allowed, err)
	}
	if allowed, err := authorizer.CanReconcileWorkflow(context.Background(), exec, instance); err != nil || allowed {
		t.Fatalf("non-operator reconciled workflow: allowed=%t err=%v", allowed, err)
	}
	store.roles = []string{"Operator"}
	if allowed, err := authorizer.CanReconcileWorkflow(context.Background(), exec, instance); err != nil || !allowed {
		t.Fatalf("operator could not reconcile readable workflow: allowed=%t err=%v", allowed, err)
	}
}
