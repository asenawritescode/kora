// Package purchasing owns the governed Purchase Request approval policy.
package purchasing

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	flow "github.com/asenawritescode/kora/workflow"
)

const (
	ApprovalPolicyID            = "purchase-request-approval-v1"
	WorkflowApprovalSubmitGrant = "requests.purchase_request_approval"
	ApprovalAction              = "Approve"
	PurchaseRequest             = "Purchase Request"
)

// UserRoles is the current, database-backed tenant membership snapshot used
// while resolving or rechecking an approval participant.
type UserRoles struct {
	Principal contract.PrincipalID
	Roles     []string
}

// ApprovalStore supplies live subject and membership data. Implementations
// keep SQL and dialect behavior at the persistence boundary.
type ApprovalStore interface {
	AuthorizationRevisions(context.Context) (policyRevision, membershipRevision contract.Revision, err error)
	UserRoles(context.Context, string, contract.PrincipalID) (roles []string, active bool, err error)
	UsersWithRoles(context.Context, string, []string, int) ([]UserRoles, error)
	Subject(context.Context, *doctype.Registry, contract.SubjectRef) (*doctype.Document, string, error)
}

// RequestAuthorizer combines the baseline direct-task policy with the single
// registered Purchase Request approval policy. Other governed requests fail
// closed until their owning domain supplies a policy.
type RequestAuthorizer struct {
	direct   requests.Authorizer
	store    ApprovalStore
	registry *doctype.Registry
	policies flow.StatusPolicyReader
}

func NewRequestAuthorizer(direct requests.Authorizer, store ApprovalStore, registry *doctype.Registry, policies flow.StatusPolicyReader) (*RequestAuthorizer, error) {
	if direct == nil || store == nil || registry == nil || registry.Permissions == nil || policies == nil {
		return nil, fmt.Errorf("purchasing request policy requires direct policy, live store, registry, and published status actions")
	}
	return &RequestAuthorizer{direct: direct, store: store, registry: registry, policies: policies}, nil
}

var _ requests.Authorizer = (*RequestAuthorizer)(nil)

func (a *RequestAuthorizer) Submission(ctx context.Context, exec policy.Execution, command contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	if command.Kind != contract.RequestApproval {
		return a.direct.Submission(ctx, exec, command)
	}
	if err := requireHuman(ctx, exec); err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	return a.purchaseApprovalSubmission(ctx, exec, command, false)
}

// WorkflowSubmission authorizes only the registered Purchase Request
// approval policy for a trusted workflow service with an exact service grant.
// Public/user submission continues through Submission and still requires a
// human actor.
func (a *RequestAuthorizer) WorkflowSubmission(ctx context.Context, exec policy.Execution, command contract.SubmitRequest, source contract.RequestSource) (requests.SubmissionAuthorization, error) {
	if _, err := contract.ParseInstanceID(string(source.InstanceID)); err != nil {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodeValidationFailed, "workflow request source instance is invalid")
	}
	if _, err := contract.ParseStepID(string(source.StepID)); err != nil {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodeValidationFailed, "workflow request source step is invalid")
	}
	actor := exec.Actor()
	if ctx == nil || !exec.Valid() || actor.PrincipalType != contract.PrincipalService || actor.SubjectUserID != "" || actor.DelegationID != "" || !exec.Allows(WorkflowApprovalSubmitGrant, "submit") {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "workflow service lacks the registered Purchase Request approval grant")
	}
	if command.Kind != contract.RequestApproval {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "workflow request policy does not authorize this request kind")
	}
	return a.purchaseApprovalSubmission(ctx, exec, command, true)
}

func (a *RequestAuthorizer) purchaseApprovalSubmission(ctx context.Context, exec policy.Execution, command contract.SubmitRequest, workflowSource bool) (requests.SubmissionAuthorization, error) {
	if command.PolicyID != ApprovalPolicyID || command.Subject == nil || command.Subject.Kind != PurchaseRequest || command.Assignment != nil || command.InformationSchema != nil {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "no registered purchasing approval policy applies to this request")
	}
	policyRevision, membershipRevision, err := a.store.AuthorizationRevisions(ctx)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	requester := contract.PrincipalID(exec.Actor().PrincipalID)
	document, owner, err := a.store.Subject(ctx, a.registry, *command.Subject)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	if !workflowSource {
		requesterRoles, active, err := a.store.UserRoles(ctx, exec.Actor().Site, requester)
		if err != nil {
			return requests.SubmissionAuthorization{}, err
		}
		if !active {
			return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "requester is not an active tenant user")
		}
		if err := a.requireSubjectRead(requesterRoles, requester, owner); err != nil {
			return requests.SubmissionAuthorization{}, err
		}
	}
	if err := subjectMatches(*command.Subject, document); err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	workflow, approveTransition, rejectTransition, state, err := a.approvalTransitions(ctx, exec.Tenant(), document)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	roles := append([]string(nil), approveTransition.AllowedRoles...)
	if len(roles) == 0 {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "Purchase Request approval has no eligible workflow roles")
	}
	users, err := a.store.UsersWithRoles(ctx, exec.Actor().Site, roles, 51)
	if err != nil {
		return requests.SubmissionAuthorization{}, err
	}
	if len(users) > 50 {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodeResourceExhausted, "Purchase Request approval has more than 50 eligible reviewers")
	}
	assignments := make([]requests.AssignmentRecord, 0, len(users))
	for _, user := range users {
		if user.Principal == requester || !a.canReadSubject(user.Roles, user.Principal, owner) || !a.canSubmitSubject(user.Roles) {
			continue
		}
		if _, _, err := flow.ApplyStatusTransition(workflow, state, ApprovalAction, user.Roles, document); err != nil {
			continue
		}
		allowedResponses := []contract.ResponseKind{contract.ResponseApprove}
		if rejectTransition != nil {
			if _, _, err := flow.ApplyStatusTransition(workflow, state, "Reject", user.Roles, document); err == nil {
				allowedResponses = append(allowedResponses, contract.ResponseReject)
			}
		}
		assignments = append(assignments, requests.AssignmentRecord{Principal: user.Principal,
			OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: string(user.Principal)}, Stage: 1,
			Status: contract.AssignmentPending, AllowedResponses: allowedResponses,
			MembershipRevision: membershipRevision})
	}
	if len(assignments) == 0 {
		return requests.SubmissionAuthorization{}, contract.NewError(contract.CodePermissionDenied, "no active reviewer can read and approve this Purchase Request")
	}
	return requests.SubmissionAuthorization{PolicyID: ApprovalPolicyID, PolicyRevision: policyRevision, MembershipRevision: membershipRevision,
		AssignmentMode: contract.AssignmentAny, Assignments: assignments, ExpectedSubject: command.Subject,
		SubjectDigest: kernel.CanonicalDocHash(document.Fields)}, nil
}

func (a *RequestAuthorizer) Response(ctx context.Context, exec policy.Execution, record requests.RequestRecord, command contract.RespondRequest) (requests.ResponseAuthorization, error) {
	if record.Kind != contract.RequestApproval {
		return a.direct.Response(ctx, exec, record, command)
	}
	if err := requireHuman(ctx, exec); err != nil {
		return requests.ResponseAuthorization{}, err
	}
	if record.PolicyID != ApprovalPolicyID || record.Subject == nil || record.Subject.Kind != PurchaseRequest {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "no registered policy applies to this approval")
	}
	policyRevision, membershipRevision, err := a.store.AuthorizationRevisions(ctx)
	if err != nil {
		return requests.ResponseAuthorization{}, err
	}
	actor := contract.PrincipalID(exec.Actor().PrincipalID)
	roles, active, err := a.store.UserRoles(ctx, exec.Actor().Site, actor)
	if err != nil {
		return requests.ResponseAuthorization{}, err
	}
	if !active {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "reviewer is not an active tenant user")
	}
	assignmentAllowed := false
	for _, assignment := range record.Assignments {
		if assignment.ID != command.AssignmentID || assignment.Principal != actor || assignment.Status != contract.AssignmentPending {
			continue
		}
		for _, response := range assignment.AllowedResponses {
			assignmentAllowed = assignmentAllowed || response == command.Response
		}
	}
	if !assignmentAllowed {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "reviewer is not eligible for this approval response")
	}
	document, owner, err := a.store.Subject(ctx, a.registry, *record.Subject)
	if err != nil {
		return requests.ResponseAuthorization{}, err
	}
	if err := subjectMatches(*record.Subject, document); err != nil || kernel.CanonicalDocHash(document.Fields) != record.SubjectDigest {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodeStaleSubject, "Purchase Request changed after approval was assigned")
	}
	if !a.canReadSubject(roles, actor, owner) || !a.canSubmitSubject(roles) {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "reviewer no longer has access to approve this Purchase Request")
	}
	workflow, approveTransition, rejectTransition, state, err := a.approvalTransitions(ctx, exec.Tenant(), document)
	if err != nil {
		return requests.ResponseAuthorization{}, err
	}
	action := ""
	switch command.Response {
	case contract.ResponseApprove:
		action = approveTransition.Action
	case contract.ResponseReject:
		if rejectTransition != nil {
			action = rejectTransition.Action
		}
	}
	if action == "" {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "current workflow policy does not allow this response")
	}
	if _, _, err := flow.ApplyStatusTransition(workflow, state, action, roles, document); err != nil {
		return requests.ResponseAuthorization{}, contract.NewError(contract.CodePermissionDenied, "current workflow policy does not allow this approval")
	}
	return requests.ResponseAuthorization{PolicyRevision: policyRevision, MembershipRevision: membershipRevision,
		ExpectedSubject: record.Subject, SubjectDigest: record.SubjectDigest}, nil
}

func (a *RequestAuthorizer) Management(ctx context.Context, exec policy.Execution, record requests.RequestRecord, operation string, command *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	if record.Kind == contract.RequestApproval {
		return requests.ManagementAuthorization{}, contract.NewError(contract.CodePermissionDenied, "Purchase Request approvals cannot be managed through direct-task operations")
	}
	return a.direct.Management(ctx, exec, record, operation, command)
}

func (a *RequestAuthorizer) Visibility(ctx context.Context, exec policy.Execution) (requests.VisibilityScope, error) {
	return a.direct.Visibility(ctx, exec)
}

func (a *RequestAuthorizer) approvalTransitions(ctx context.Context, tenant contract.TenantID, document *doctype.Document) (*flow.StatusPolicy, *flow.StatusTransition, *flow.StatusTransition, string, error) {
	if document == nil || document.DocType != PurchaseRequest {
		return nil, nil, nil, "", contract.NewError(contract.CodePermissionDenied, "Purchase Request subject is unavailable")
	}
	statusPolicy, found, err := a.policies.GetStatusPolicy(ctx, tenant, PurchaseRequest)
	if err != nil {
		return nil, nil, nil, "", err
	}
	if !found || statusPolicy == nil {
		return nil, nil, nil, "", contract.NewError(contract.CodeDependencyUnavailable, "Purchase Request status actions are not published")
	}
	state := fmt.Sprint(document.Get(statusPolicy.StateField))
	if state == "<nil>" || state == "" {
		state = statusPolicy.InitialState
	}
	if state != "Submitted" {
		return nil, nil, nil, "", contract.NewError(contract.CodeConflict, "Purchase Request is no longer awaiting approval")
	}
	var approve, reject *flow.StatusTransition
	for i := range statusPolicy.Transitions {
		transition := &statusPolicy.Transitions[i]
		if transition.From != state {
			continue
		}
		switch strings.ToLower(transition.Action) {
		case strings.ToLower(ApprovalAction):
			approve = transition
		case "reject":
			reject = transition
		}
	}
	if approve == nil || approve.To == "" {
		return nil, nil, nil, "", contract.NewError(contract.CodeDependencyUnavailable, "Purchase Request approval action is not configured")
	}
	return statusPolicy, approve, reject, state, nil
}

func (a *RequestAuthorizer) canReadSubject(roles []string, principal contract.PrincipalID, owner string) bool {
	if a.registry.Permissions == nil {
		return false
	}
	allowed, ownerOnly := a.registry.Permissions.UserCan(roles, PurchaseRequest, "read")
	return allowed && (!ownerOnly || owner == string(principal))
}

func (a *RequestAuthorizer) canSubmitSubject(roles []string) bool {
	if a.registry.Permissions == nil {
		return false
	}
	allowed, ownerOnly := a.registry.Permissions.UserCan(roles, PurchaseRequest, "submit")
	return allowed && !ownerOnly
}

func (a *RequestAuthorizer) requireSubjectRead(roles []string, principal contract.PrincipalID, owner string) error {
	if !a.canReadSubject(roles, principal, owner) {
		return contract.NewError(contract.CodePermissionDenied, "requester cannot read this Purchase Request")
	}
	return nil
}

func subjectMatches(reference contract.SubjectRef, document *doctype.Document) error {
	if document == nil || document.Name != reference.ID || document.Revision == 0 || string(reference.Revision) != strconv.FormatUint(document.Revision, 10) {
		return contract.NewError(contract.CodeStaleSubject, "Purchase Request revision changed")
	}
	return nil
}

func requireHuman(ctx context.Context, exec policy.Execution) error {
	if ctx == nil || !exec.Valid() || exec.Actor().PrincipalType != contract.PrincipalHuman || exec.Actor().PrincipalID == "" || exec.Actor().SubjectUserID != exec.Actor().PrincipalID || strings.TrimSpace(exec.Actor().Site) == "" {
		return contract.NewError(contract.CodePermissionDenied, "verified tenant user execution is required for Purchase Request approval")
	}
	return ctx.Err()
}
