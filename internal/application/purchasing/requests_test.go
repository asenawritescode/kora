package purchasing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/asenawritescode/kora/internal/workflow"
)

type fakeApprovalStore struct {
	users   map[contract.PrincipalID]UserRoles
	doc     *doctype.Document
	owner   string
	active  map[contract.PrincipalID]bool
	userErr error
}

func (s *fakeApprovalStore) AuthorizationRevisions(context.Context) (contract.Revision, contract.Revision, error) {
	return "7", "11", nil
}
func (s *fakeApprovalStore) UserRoles(_ context.Context, _ string, id contract.PrincipalID) ([]string, bool, error) {
	user, ok := s.users[id]
	return append([]string(nil), user.Roles...), ok && s.active[id], s.userErr
}
func (s *fakeApprovalStore) UsersWithRoles(_ context.Context, _ string, roles []string, limit int) ([]UserRoles, error) {
	result := make([]UserRoles, 0)
	for id, user := range s.users {
		if !s.active[id] {
			continue
		}
		for _, role := range user.Roles {
			for _, required := range roles {
				if role == required {
					result = append(result, user)
					goto next
				}
			}
		}
	next:
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}
func (s *fakeApprovalStore) Subject(context.Context, *doctype.Registry, contract.SubjectRef) (*doctype.Document, string, error) {
	copyDoc := *s.doc
	copyDoc.Fields = make(map[string]any, len(s.doc.Fields))
	for name, value := range s.doc.Fields {
		copyDoc.Fields[name] = value
	}
	return &copyDoc, s.owner, nil
}

type fakePurchaseStatusPolicy struct{}

func (fakePurchaseStatusPolicy) GetStatusPolicy(context.Context, contract.TenantID, string) (*workflow.StatusPolicy, bool, error) {
	return &workflow.StatusPolicy{
		Table: PurchaseRequest, StateField: "status", InitialState: "Draft",
		Transitions: []workflow.StatusTransition{
			{Action: ApprovalAction, From: "Submitted", To: "Approved", AllowedRoles: []string{"Purchase Manager", "Finance Manager"}, Condition: "doc.estimated_total > 0"},
			{Action: "Reject", From: "Submitted", To: "Rejected", AllowedRoles: []string{"Purchase Manager"}},
		},
	}, true, nil
}

func purchasingFixture(t *testing.T) (*RequestAuthorizer, *fakeApprovalStore, policy.Execution) {
	t.Helper()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: PurchaseRequest, Fields: []doctype.Field{{Fieldname: "estimated_total"}}})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: PurchaseRequest, Role: "Procurement Officer", Read: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: PurchaseRequest, Role: "Purchase Manager", Read: true, Submit: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: PurchaseRequest, Role: "Finance Manager", Read: true, Submit: true})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: PurchaseRequest, Role: "Auditor", Read: true})

	store := &fakeApprovalStore{
		users: map[contract.PrincipalID]UserRoles{
			"requester": {Principal: "requester", Roles: []string{"Procurement Officer"}},
			"purchase":  {Principal: "purchase", Roles: []string{"Purchase Manager"}},
			"finance":   {Principal: "finance", Roles: []string{"Finance Manager"}},
			"auditor":   {Principal: "auditor", Roles: []string{"Auditor"}},
		},
		active: map[contract.PrincipalID]bool{"requester": true, "purchase": true, "finance": true, "auditor": true},
		doc:    &doctype.Document{DocType: PurchaseRequest, Name: "PR-1", Revision: 3, Fields: map[string]any{"status": "Submitted", "estimated_total": float64(500)}}, owner: "requester",
	}
	authorizer, err := NewRequestAuthorizer(testDirectAuthorizer{}, store, registry, fakePurchaseStatusPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	exec, err := policy.NewVerifiedExecution("tenant-1", contract.ActorContext{PrincipalID: "requester", PrincipalType: contract.PrincipalHuman, SubjectUserID: "requester", Site: "tenant.example", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return authorizer, store, exec
}

type testDirectAuthorizer struct{}

func (testDirectAuthorizer) Submission(context.Context, policy.Execution, contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	return requests.SubmissionAuthorization{}, nil
}
func (testDirectAuthorizer) Response(context.Context, policy.Execution, requests.RequestRecord, contract.RespondRequest) (requests.ResponseAuthorization, error) {
	return requests.ResponseAuthorization{}, nil
}
func (testDirectAuthorizer) Management(context.Context, policy.Execution, requests.RequestRecord, string, *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	return requests.ManagementAuthorization{}, nil
}
func (testDirectAuthorizer) Visibility(context.Context, policy.Execution) (requests.VisibilityScope, error) {
	return requests.VisibilityScope{}, nil
}

func TestPurchaseApprovalDerivesReviewersAndRechecksLivePolicy(t *testing.T) {
	authorizer, store, exec := purchasingFixture(t)
	command := contract.SubmitRequest{Kind: contract.RequestApproval, PolicyID: ApprovalPolicyID, Subject: &contract.SubjectRef{Kind: PurchaseRequest, ID: "PR-1", Revision: "3"}, Title: "Approve purchase"}
	result, err := authorizer.Submission(context.Background(), exec, command)
	if err != nil {
		t.Fatal("Submission:", err)
	}
	if result.PolicyRevision != "7" || result.MembershipRevision != "11" || result.AssignmentMode != contract.AssignmentAny || len(result.Assignments) != 2 {
		t.Fatalf("unexpected policy resolution: %+v", result)
	}
	for _, assignment := range result.Assignments {
		if assignment.Principal == "requester" || assignment.Principal == "auditor" {
			t.Fatalf("ineligible reviewer assigned: %+v", assignment)
		}
		if assignment.Principal == "finance" && (len(assignment.AllowedResponses) != 1 || assignment.AllowedResponses[0] != contract.ResponseApprove) {
			t.Fatalf("Finance Manager received an unavailable response: %+v", assignment)
		}
		if assignment.Principal == "purchase" && (len(assignment.AllowedResponses) != 2 || assignment.AllowedResponses[1] != contract.ResponseReject) {
			t.Fatalf("Purchase Manager response options do not match workflow actions: %+v", assignment)
		}
	}
	record := requests.RequestRecord{Kind: contract.RequestApproval, PolicyID: ApprovalPolicyID, Subject: command.Subject, SubjectDigest: result.SubjectDigest, Assignments: result.Assignments}
	assignment := result.Assignments[0]
	responseExec, err := policy.NewVerifiedExecution(exec.Tenant(), contract.ActorContext{PrincipalID: string(assignment.Principal), PrincipalType: contract.PrincipalHuman, SubjectUserID: string(assignment.Principal), Site: "tenant.example", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = authorizer.Response(context.Background(), responseExec, record, contract.RespondRequest{AssignmentID: assignment.ID, Response: contract.ResponseApprove})
	if err != nil {
		t.Fatal("eligible Response:", err)
	}
	store.doc.Revision++
	if _, err := authorizer.Response(context.Background(), responseExec, record, contract.RespondRequest{AssignmentID: assignment.ID, Response: contract.ResponseApprove}); errorCode(err) != contract.CodeStaleSubject {
		t.Fatalf("changed subject response error=%v, want stale subject", err)
	}
	store.doc.Revision = 3
	purchaseAssignment := result.Assignments[0]
	for _, candidate := range result.Assignments {
		if candidate.Principal == "purchase" {
			purchaseAssignment = candidate
		}
	}
	purchaseExecution, err := policy.NewVerifiedExecution(exec.Tenant(), contract.ActorContext{PrincipalID: string(purchaseAssignment.Principal), PrincipalType: contract.PrincipalHuman, SubjectUserID: string(purchaseAssignment.Principal), Site: "tenant.example", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.Response(context.Background(), purchaseExecution, record, contract.RespondRequest{AssignmentID: purchaseAssignment.ID, Response: contract.ResponseReject}); err != nil {
		t.Fatalf("workflow-authorized rejection: %v", err)
	}
}

func TestPurchaseApprovalRejectsClientAssignmentsAndStaleRevision(t *testing.T) {
	authorizer, _, exec := purchasingFixture(t)
	base := contract.SubmitRequest{Kind: contract.RequestApproval, PolicyID: ApprovalPolicyID, Subject: &contract.SubjectRef{Kind: PurchaseRequest, ID: "PR-1", Revision: "3"}, Title: "Approve purchase"}
	withAssignment := base
	withAssignment.Assignment = &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "purchase"}}}
	if _, err := authorizer.Submission(context.Background(), exec, withAssignment); errorCode(err) != contract.CodePermissionDenied {
		t.Fatalf("client assignment error=%v, want permission denied", err)
	}
	base.Subject.Revision = "2"
	if _, err := authorizer.Submission(context.Background(), exec, base); errorCode(err) != contract.CodeStaleSubject {
		t.Fatalf("stale revision error=%v, want stale subject", err)
	}
}

func TestWorkflowPurchaseApprovalRequiresExactServiceGrantAndUsesLiveReviewers(t *testing.T) {
	authorizer, store, exec := purchasingFixture(t)
	serviceExec, err := policy.NewVerifiedExecutionWithGrants(exec.Tenant(), contract.ActorContext{
		PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService, Site: "tenant.example", AuthenticatedAt: time.Now().UTC(),
	}, []policy.Grant{{Resource: WorkflowApprovalSubmitGrant, Operation: "submit"}})
	if err != nil {
		t.Fatal("construct workflow identity:", err)
	}
	command := contract.SubmitRequest{Kind: contract.RequestApproval, PolicyID: ApprovalPolicyID,
		Subject: &contract.SubjectRef{Kind: PurchaseRequest, ID: "PR-1", Revision: "3"}, Title: "Approve purchase"}
	source := contract.RequestSource{InstanceID: "01K7INSTANCE00000000000000000", StepID: "approval"}
	result, err := authorizer.WorkflowSubmission(context.Background(), serviceExec, command, source)
	if err != nil {
		t.Fatal("WorkflowSubmission:", err)
	}
	if result.PolicyID != ApprovalPolicyID || result.PolicyRevision != "7" || result.MembershipRevision != "11" || len(result.Assignments) != 2 || result.ExpectedSubject == nil || result.SubjectDigest == "" {
		t.Fatalf("workflow approval did not resolve current domain policy: %+v", result)
	}
	for _, assignment := range result.Assignments {
		if assignment.Principal == "requester" || assignment.Principal == "auditor" {
			t.Fatalf("workflow assigned an ineligible reviewer: %+v", assignment)
		}
	}
	store.doc.Revision++
	if _, err := authorizer.WorkflowSubmission(context.Background(), serviceExec, command, source); errorCode(err) != contract.CodeStaleSubject {
		t.Fatalf("changed subject error=%v, want stale subject", err)
	}
	withoutGrant, err := policy.NewVerifiedExecution(exec.Tenant(), serviceExec.Actor())
	if err != nil {
		t.Fatal("construct ungranted workflow identity:", err)
	}
	if _, err := authorizer.WorkflowSubmission(context.Background(), withoutGrant, command, source); errorCode(err) != contract.CodePermissionDenied {
		t.Fatalf("missing grant error=%v, want permission denied", err)
	}
	if _, err := authorizer.Submission(context.Background(), serviceExec, command); errorCode(err) != contract.CodePermissionDenied {
		t.Fatalf("public approval submission accepted a service identity: %v", err)
	}
}

func errorCode(err error) contract.Code {
	var target *contract.Error
	if errors.As(err, &target) {
		return target.Type
	}
	return ""
}
