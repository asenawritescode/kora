package requests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

const (
	opSubmit   = "request.submit"
	opRespond  = "request.respond"
	opReassign = "request.reassign"
	opCancel   = "request.cancel"
)

type Clock interface{ Now() time.Time }
type IDSource interface{ NewID() string }

type SubmissionAuthorization struct {
	PolicyID           string
	PolicyRevision     contract.Revision
	MembershipRevision contract.Revision
	AssignmentMode     contract.AssignmentMode
	Assignments        []AssignmentRecord
	ExpectedSubject    *contract.SubjectRef
	SubjectDigest      string
}

type ResponseAuthorization struct {
	PolicyRevision     contract.Revision
	MembershipRevision contract.Revision
	ExpectedSubject    *contract.SubjectRef
	// SubjectDigest is read from the live domain subject. A match is required
	// before recording the decision; local effects still revalidate it inside
	// their domain transaction, while external continuations must reconcile it.
	SubjectDigest string
}

type ManagementAuthorization struct {
	PolicyRevision     contract.Revision
	MembershipRevision contract.Revision
	Replacement        *AssignmentRecord
}

type Authorizer interface {
	Submission(context.Context, policy.Execution, contract.SubmitRequest) (SubmissionAuthorization, error)
	Response(context.Context, policy.Execution, RequestRecord, contract.RespondRequest) (ResponseAuthorization, error)
	Management(context.Context, policy.Execution, RequestRecord, string, *contract.ReassignRequest) (ManagementAuthorization, error)
	Visibility(context.Context, policy.Execution) (VisibilityScope, error)
}

// WorkflowSubmissionAuthorizer is an optional domain-owned policy boundary
// used only by PrepareWorkflowSubmit. A workflow request cannot fall back to
// the public user-submission policy because its verified actor is a service
// principal and its source is bound by the durable workflow executor.
type WorkflowSubmissionAuthorizer interface {
	WorkflowSubmission(context.Context, policy.Execution, contract.SubmitRequest, contract.RequestSource) (SubmissionAuthorization, error)
}

type Projector interface {
	Request(context.Context, policy.Execution, RequestRecord) (contract.RequestView, error)
	History(context.Context, policy.Execution, RequestRecord, contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error)
	Response(context.Context, policy.Execution, RequestRecord, DecisionRecord) (contract.ResponseDetail, error)
}

type ResponseSchemas interface {
	Registered(context.Context, contract.SchemaRef) error
	Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error
}

type Service struct {
	store           Store
	authorizer      Authorizer
	projector       Projector
	responseSchemas ResponseSchemas
	clock           Clock
	ids             IDSource
}

func NewService(store Store, authorizer Authorizer, projector Projector, responseSchemas ResponseSchemas, clock Clock, ids IDSource) (*Service, error) {
	if store == nil || authorizer == nil || projector == nil || responseSchemas == nil || clock == nil || ids == nil {
		return nil, fmt.Errorf("request service requires store, authorizer, projector, response schemas, clock and ID source")
	}
	return &Service{store: store, authorizer: authorizer, projector: projector, responseSchemas: responseSchemas, clock: clock, ids: ids}, nil
}

func (s *Service) Submit(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest) (contract.RequestView, error) {
	change, err := s.prepareSubmit(ctx, exec, cmd)
	if err != nil {
		return contract.RequestView{}, err
	}
	stored, err := s.store.Create(ctx, change)
	if err != nil {
		return contract.RequestView{}, err
	}
	return s.projector.Request(ctx, exec, stored)
}

// PrepareSubmit performs the same policy and subject checks as Submit and
// returns an opaque commit that can be composed with a workflow wait in a
// caller-owned transaction. The authorization snapshot is rechecked by the
// SQL store inside that transaction.
func (s *Service) PrepareSubmit(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest) (*PreparedSubmission, error) {
	change, err := s.prepareSubmit(ctx, exec, cmd)
	if err != nil {
		return nil, err
	}
	store, ok := s.store.(TransactionCreateStore)
	if !ok {
		return nil, contract.NewError(contract.CodeInternal, "request store does not support transaction-bound creation")
	}
	return &PreparedSubmission{store: store, change: change}, nil
}

// PrepareWorkflowSubmit binds a request to a workflow instance and step. Source
// identity is supplied by the workflow runtime, never by the public request
// DTO, and participates in the idempotency digest.
func (s *Service) PrepareWorkflowSubmit(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest, source contract.RequestSource) (*PreparedSubmission, error) {
	if _, err := contract.ParseInstanceID(string(source.InstanceID)); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow request source instance is invalid")
	}
	if _, err := contract.ParseStepID(string(source.StepID)); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow request source step is invalid")
	}
	workflowAuthorizer, ok := s.authorizer.(WorkflowSubmissionAuthorizer)
	if !ok {
		return nil, contract.NewError(contract.CodePermissionDenied, "workflow-originated request policy is unavailable")
	}
	change, err := s.prepareSubmitUsing(ctx, exec, cmd, func() (SubmissionAuthorization, error) {
		return workflowAuthorizer.WorkflowSubmission(ctx, exec, cmd, source)
	})
	if err != nil {
		return nil, err
	}
	digest, err := canonicalDigest(struct {
		Operation string
		Command   contract.SubmitRequest
		Source    contract.RequestSource
	}{opSubmit, cmd, source})
	if err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "workflow request command cannot be canonicalized")
	}
	change.Request.Source = &source
	change.Identity.CommandDigest = digest
	store, ok := s.store.(TransactionCreateStore)
	if !ok {
		return nil, contract.NewError(contract.CodeInternal, "request store does not support transaction-bound creation")
	}
	return &PreparedSubmission{store: store, change: change}, nil
}

func (s *Service) prepareSubmit(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest) (CreateCommit, error) {
	return s.prepareSubmitUsing(ctx, exec, cmd, func() (SubmissionAuthorization, error) {
		return s.authorizer.Submission(ctx, exec, cmd)
	})
}

func (s *Service) prepareSubmitUsing(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest, authorize func() (SubmissionAuthorization, error)) (CreateCommit, error) {
	if err := s.validateExecution(exec); err != nil {
		return CreateCommit{}, err
	}
	if err := contract.ValidateSubmitRequest(cmd); err != nil {
		return CreateCommit{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if !validSubmitShape(cmd) {
		return CreateCommit{}, contract.NewError(contract.CodeValidationFailed, "request kind requires a valid subject, policy, assignment and information schema")
	}
	if authorize == nil {
		return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "request submission policy is unavailable")
	}
	authorization, err := authorize()
	if err != nil {
		return CreateCommit{}, err
	}
	if !sameSubject(cmd.Subject, authorization.ExpectedSubject) {
		return CreateCommit{}, contract.NewError(contract.CodeConflict, "request subject changed during authorization")
	}
	if _, err := contract.ParseRevision(string(authorization.PolicyRevision)); err != nil {
		return CreateCommit{}, contract.NewError(contract.CodeInternal, "submission authorization lacks a policy revision")
	}
	if _, err := contract.ParseRevision(string(authorization.MembershipRevision)); err != nil {
		return CreateCommit{}, contract.NewError(contract.CodeInternal, "submission authorization lacks a membership revision")
	}
	if cmd.Subject != nil {
		if len(authorization.SubjectDigest) != 64 {
			return CreateCommit{}, contract.NewError(contract.CodeInternal, "subject authorization lacks a content digest")
		}
		if _, err := hex.DecodeString(authorization.SubjectDigest); err != nil || authorization.SubjectDigest != strings.ToLower(authorization.SubjectDigest) {
			return CreateCommit{}, contract.NewError(contract.CodeInternal, "subject authorization digest is invalid")
		}
	}
	if cmd.Kind == contract.RequestInformation {
		if err := s.responseSchemas.Registered(ctx, *cmd.InformationSchema); err != nil {
			return CreateCommit{}, contract.NewError(contract.CodeValidationFailed, "information response schema is not registered")
		}
	}
	if cmd.Kind == contract.RequestApproval && authorization.PolicyID == "" {
		return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "approval policy is unavailable")
	}
	if cmd.PolicyID != "" && authorization.PolicyID != cmd.PolicyID {
		return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "requested policy is not authorized")
	}
	if !validAssignmentMode(authorization.AssignmentMode) {
		return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "request assignment mode is not authorized")
	}
	if err := validateAuthorizedAssignment(cmd.Assignment, authorization); err != nil {
		return CreateCommit{}, err
	}
	actor, err := executionPrincipal(exec)
	if err != nil {
		return CreateCommit{}, err
	}
	if len(authorization.Assignments) == 0 || len(authorization.Assignments) > 50 {
		return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "authorized request assignments are empty or exceed the limit")
	}
	id, err := s.newRequestID()
	if err != nil {
		return CreateCommit{}, err
	}
	now := s.clock.Now().UTC()
	if cmd.DueAt != nil && !cmd.DueAt.After(now) {
		return CreateCommit{}, contract.NewError(contract.CodeValidationFailed, "request deadline must be in the future")
	}
	assignments := make([]AssignmentRecord, len(authorization.Assignments))
	for i, source := range authorization.Assignments {
		assignmentID, err := s.newAssignmentID()
		if err != nil {
			return CreateCommit{}, err
		}
		if cmd.Kind == contract.RequestApproval && source.Principal == actor {
			return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "approval requester cannot be an assignee")
		}
		if source.MembershipRevision != "" && source.MembershipRevision != authorization.MembershipRevision {
			return CreateCommit{}, contract.NewError(contract.CodeConflict, "assignment membership snapshot changed")
		}
		if !validAssignmentResponses(cmd.Kind, source.AllowedResponses) {
			return CreateCommit{}, contract.NewError(contract.CodePermissionDenied, "assignment response set is invalid for the request kind")
		}
		source.ID, source.RequestID = assignmentID, id
		source.MembershipRevision = authorization.MembershipRevision
		assignments[i] = source
	}
	identity, err := s.writeIdentity(exec, opSubmit, cmd.Meta.IdempotencyKey, struct {
		Operation string
		Command   contract.SubmitRequest
	}{opSubmit, cmd})
	if err != nil {
		return CreateCommit{}, err
	}
	record := RequestRecord{Tenant: exec.Tenant(), ID: id, Revision: "1", Kind: cmd.Kind, AssignmentMode: authorization.AssignmentMode,
		Status: contract.RequestPending, Requester: actor, Subject: cloneSubject(cmd.Subject), SubjectDigest: authorization.SubjectDigest,
		Title: cmd.Title, Message: cmd.Message, DueAt: cloneTime(cmd.DueAt), CreatedAt: now, UpdatedAt: now,
		PolicyID: authorization.PolicyID, PolicyRevision: authorization.PolicyRevision, InformationSchema: cloneSchema(cmd.InformationSchema), Assignments: assignments}
	event, err := s.event(exec, "request.created", id, record.Revision, record.Status, now)
	if err != nil {
		return CreateCommit{}, err
	}
	actorCopy := actor
	created := contract.RequestHistoryEntry{Sequence: "1", Kind: contract.HistoryCreated, Actor: &actorCopy, At: now}
	return CreateCommit{Identity: identity, Request: record, ExpectedPolicyRevision: authorization.PolicyRevision,
		ExpectedMembershipRevision: authorization.MembershipRevision, ExpectedSubject: cloneSubject(authorization.ExpectedSubject),
		Assignments: assignments, History: created, Events: []contract.EventEnvelope{event}}, nil
}

func (s *Service) Get(ctx context.Context, exec policy.Execution, id contract.RequestID) (contract.RequestView, error) {
	if err := s.validateExecution(exec); err != nil {
		return contract.RequestView{}, err
	}
	record, scope, err := s.visibleRecord(ctx, exec, id)
	if err != nil {
		return contract.RequestView{}, err
	}
	_ = scope
	return s.projector.Request(ctx, exec, record)
}

func (s *Service) List(ctx context.Context, exec policy.Execution, filter contract.RequestFilter) (contract.Page[contract.RequestView], error) {
	if err := s.validateExecution(exec); err != nil {
		return contract.Page[contract.RequestView]{}, err
	}
	scope, err := s.visibilityScope(ctx, exec)
	if err != nil {
		return contract.Page[contract.RequestView]{}, err
	}
	stored, err := s.store.ListVisible(ctx, scope, filter)
	if err != nil {
		return contract.Page[contract.RequestView]{}, err
	}
	page := contract.Page[contract.RequestView]{Items: make([]contract.RequestView, 0, len(stored.Items)), NextCursor: stored.NextCursor}
	for _, record := range stored.Items {
		view, err := s.projector.Request(ctx, exec, record)
		if err != nil {
			return contract.Page[contract.RequestView]{}, err
		}
		page.Items = append(page.Items, view)
	}
	return page, nil
}

func (s *Service) Respond(ctx context.Context, exec policy.Execution, id contract.RequestID, cmd contract.RespondRequest) (contract.ResponseReceipt, error) {
	change, replay, err := s.prepareResponse(ctx, exec, id, cmd)
	if err != nil {
		return contract.ResponseReceipt{}, err
	}
	if replay != nil {
		return *replay, nil
	}
	return s.store.CommitDecision(ctx, change)
}

// PrepareResponse performs response-time authorization and creates an opaque,
// revision-bound decision. Composers call CommitInTx after starting their unit
// of work so a local domain mutation and this decision share one transaction.
func (s *Service) PrepareResponse(ctx context.Context, exec policy.Execution, id contract.RequestID, cmd contract.RespondRequest) (*PreparedResponse, error) {
	change, replay, err := s.prepareResponse(ctx, exec, id, cmd)
	if err != nil {
		return nil, err
	}
	if replay != nil {
		copy := *replay
		return &PreparedResponse{replay: &copy}, nil
	}
	store, ok := s.store.(TransactionDecisionStore)
	if !ok {
		return nil, contract.NewError(contract.CodeInternal, "request store does not support transaction-bound decisions")
	}
	return &PreparedResponse{store: store, change: change}, nil
}

func (s *Service) prepareResponse(ctx context.Context, exec policy.Execution, id contract.RequestID, cmd contract.RespondRequest) (DecisionCommit, *contract.ResponseReceipt, error) {
	if err := s.validateExecution(exec); err != nil {
		return DecisionCommit{}, nil, err
	}
	if err := contract.ValidateRespondRequest(cmd); err != nil {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	expectedRevision, err := requireRevision(cmd.Meta.ExpectedRevision)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	identity, err := s.writeIdentity(exec, opRespond, cmd.Meta.IdempotencyKey, struct {
		Operation string
		RequestID contract.RequestID
		Command   contract.RespondRequest
	}{opRespond, id, cmd})
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	if identity.IdempotencyKey != "" {
		if lookup, ok := s.store.(DecisionReceiptLookup); ok {
			receipt, found, lookupErr := lookup.LookupResponseReceipt(ctx, identity)
			if lookupErr != nil {
				return DecisionCommit{}, nil, lookupErr
			}
			if found {
				if receipt.RequestID != id {
					return DecisionCommit{}, nil, contract.NewError(contract.CodeInternal, "stored response receipt does not match request")
				}
				if _, visibilityErr := s.Get(ctx, exec, id); visibilityErr != nil {
					return DecisionCommit{}, nil, visibilityErr
				}
				return DecisionCommit{}, &receipt, nil
			}
		}
	}
	record, err := s.store.Get(ctx, exec.Tenant(), id)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	actor, err := executionPrincipal(exec)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	authorization, err := s.authorizer.Response(ctx, exec, record, cmd)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	if !sameSubject(record.Subject, authorization.ExpectedSubject) {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeStaleSubject, "request subject changed during authorization")
	}
	if record.Subject != nil && (authorization.SubjectDigest == "" || authorization.SubjectDigest != record.SubjectDigest) {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeStaleSubject, "request subject content changed during authorization")
	}
	if _, err := contract.ParseRevision(string(authorization.PolicyRevision)); err != nil {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeInternal, "response authorization lacks a policy revision")
	}
	if _, err := contract.ParseRevision(string(authorization.MembershipRevision)); err != nil {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeInternal, "response authorization lacks a membership revision")
	}
	if cmd.Response == contract.ResponseProvide {
		if record.Kind != contract.RequestInformation || record.InformationSchema == nil || cmd.Information == nil {
			return DecisionCommit{}, nil, contract.NewError(contract.CodeValidationFailed, "information response does not match the request")
		}
		if cmd.Information.SchemaID != record.InformationSchema.ID || cmd.Information.SchemaVersion != record.InformationSchema.Version {
			return DecisionCommit{}, nil, contract.NewError(contract.CodeValidationFailed, "information response schema does not match the request")
		}
		if err := s.responseSchemas.Validate(ctx, *record.InformationSchema, *cmd.Information); err != nil {
			return DecisionCommit{}, nil, contract.NewError(contract.CodeValidationFailed, "information response failed schema validation")
		}
	} else if cmd.Information != nil {
		return DecisionCommit{}, nil, contract.NewError(contract.CodeValidationFailed, "only information responses may include values")
	}
	if !responseAllowedByKind(record.Kind, cmd.Response) {
		return DecisionCommit{}, nil, contract.NewError(contract.CodePermissionDenied, "response is invalid for this request kind")
	}
	decisionID, err := s.newDecisionID()
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	next, err := incrementRevision(record.Revision)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	now := s.clock.Now().UTC()
	actorContext := exec.Actor()
	onBehalfOf := contract.PrincipalID("")
	if actorContext.SubjectUserID != "" {
		onBehalfOf = contract.PrincipalID(actorContext.SubjectUserID)
	}
	decision := DecisionRecord{Tenant: exec.Tenant(), ID: decisionID, RequestID: id, AssignmentID: cmd.AssignmentID, Actor: actor, OnBehalfOf: onBehalfOf,
		Response: cmd.Response, Comment: cmd.Comment, Information: cloneInformation(cmd.Information), At: now}
	event, err := s.event(exec, "request.responded", id, next, "", now)
	if err != nil {
		return DecisionCommit{}, nil, err
	}
	placeholderReceipt := contract.ResponseReceipt{RequestID: id, DecisionID: decisionID, Revision: next, Status: contract.RequestPending}
	return DecisionCommit{Request: record, ExpectedRevision: expectedRevision, Decision: decision, Receipt: placeholderReceipt, Identity: identity,
		ExpectedSubject: cloneSubject(authorization.ExpectedSubject), ExpectedPolicyRevision: authorization.PolicyRevision,
		ExpectedMembershipRevision: authorization.MembershipRevision, Events: []contract.EventEnvelope{event}}, nil, nil
}

func (s *Service) Reassign(ctx context.Context, exec policy.Execution, id contract.RequestID, cmd contract.ReassignRequest) (contract.RequestView, error) {
	if err := s.validateExecution(exec); err != nil {
		return contract.RequestView{}, err
	}
	if err := contract.ValidateReassignRequest(cmd); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	expectedRevision, err := requireRevision(cmd.Meta.ExpectedRevision)
	if err != nil {
		return contract.RequestView{}, err
	}
	record, err := s.store.Get(ctx, exec.Tenant(), id)
	if err != nil {
		return contract.RequestView{}, err
	}
	authorization, err := s.authorizer.Management(ctx, exec, record, "reassign", &cmd)
	if err != nil {
		return contract.RequestView{}, err
	}
	if _, err := contract.ParseRevision(string(authorization.PolicyRevision)); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeInternal, "reassignment authorization lacks a policy revision")
	}
	if _, err := contract.ParseRevision(string(authorization.MembershipRevision)); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeInternal, "reassignment authorization lacks a membership revision")
	}
	if authorization.Replacement == nil {
		return contract.RequestView{}, contract.NewError(contract.CodePermissionDenied, "reassignment target did not resolve to an eligible user")
	}
	var current *AssignmentRecord
	for i := range record.Assignments {
		if record.Assignments[i].ID == cmd.AssignmentID {
			current = &record.Assignments[i]
			break
		}
	}
	if current == nil {
		return contract.RequestView{}, contract.NewError(contract.CodeNotFound, "assignment not found")
	}
	replacement := *authorization.Replacement
	replacement.ID, replacement.RequestID = current.ID, id
	replacement.OriginalTarget, replacement.Stage = current.OriginalTarget, current.Stage
	replacement.Status, replacement.AllowedResponses = current.Status, append([]contract.ResponseKind(nil), current.AllowedResponses...)
	replacement.MembershipRevision = authorization.MembershipRevision
	identity, err := s.writeIdentity(exec, opReassign, cmd.Meta.IdempotencyKey, struct {
		Operation string
		RequestID contract.RequestID
		Command   contract.ReassignRequest
	}{opReassign, id, cmd})
	if err != nil {
		return contract.RequestView{}, err
	}
	now := s.clock.Now().UTC()
	next, err := incrementRevision(record.Revision)
	if err != nil {
		return contract.RequestView{}, err
	}
	event, err := s.event(exec, "request.reassigned", id, next, record.Status, now)
	if err != nil {
		return contract.RequestView{}, err
	}
	updated, err := s.store.Reassign(ctx, ReassignCommit{Identity: identity, RequestID: id, ExpectedRevision: expectedRevision,
		ExpectedPolicyRevision: authorization.PolicyRevision, ExpectedMembershipRevision: authorization.MembershipRevision,
		AssignmentID: cmd.AssignmentID, Replacement: replacement, Reason: cmd.Reason, Events: []contract.EventEnvelope{event}})
	if err != nil {
		return contract.RequestView{}, err
	}
	return s.projector.Request(ctx, exec, updated)
}

func (s *Service) Cancel(ctx context.Context, exec policy.Execution, id contract.RequestID, cmd contract.CancelRequest) (contract.RequestView, error) {
	if err := s.validateExecution(exec); err != nil {
		return contract.RequestView{}, err
	}
	if err := contract.ValidateCancelRequest(cmd); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	expectedRevision, err := requireRevision(cmd.Meta.ExpectedRevision)
	if err != nil {
		return contract.RequestView{}, err
	}
	record, err := s.store.Get(ctx, exec.Tenant(), id)
	if err != nil {
		return contract.RequestView{}, err
	}
	authorization, err := s.authorizer.Management(ctx, exec, record, "cancel", nil)
	if err != nil {
		return contract.RequestView{}, err
	}
	if _, err := contract.ParseRevision(string(authorization.PolicyRevision)); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeInternal, "cancellation authorization lacks a policy revision")
	}
	if _, err := contract.ParseRevision(string(authorization.MembershipRevision)); err != nil {
		return contract.RequestView{}, contract.NewError(contract.CodeInternal, "cancellation authorization lacks a membership revision")
	}
	identity, err := s.writeIdentity(exec, opCancel, cmd.Meta.IdempotencyKey, struct {
		Operation string
		RequestID contract.RequestID
		Command   contract.CancelRequest
	}{opCancel, id, cmd})
	if err != nil {
		return contract.RequestView{}, err
	}
	now := s.clock.Now().UTC()
	next, err := incrementRevision(record.Revision)
	if err != nil {
		return contract.RequestView{}, err
	}
	event, err := s.event(exec, "request.cancelled", id, next, contract.RequestCancelled, now)
	if err != nil {
		return contract.RequestView{}, err
	}
	updated, err := s.store.Cancel(ctx, CancelCommit{Identity: identity, RequestID: id, ExpectedRevision: expectedRevision,
		ExpectedPolicyRevision: authorization.PolicyRevision, ExpectedMembershipRevision: authorization.MembershipRevision,
		Reason: cmd.Reason, Events: []contract.EventEnvelope{event}})
	if err != nil {
		return contract.RequestView{}, err
	}
	return s.projector.Request(ctx, exec, updated)
}

func (s *Service) History(ctx context.Context, exec policy.Execution, id contract.RequestID, page contract.PageRequest) (contract.Page[contract.RequestHistoryEntry], error) {
	record, scope, err := s.visibleRecord(ctx, exec, id)
	if err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, err
	}
	stored, err := s.store.History(ctx, scope, id, page)
	if err != nil {
		return contract.Page[contract.RequestHistoryEntry]{}, err
	}
	result := contract.Page[contract.RequestHistoryEntry]{Items: make([]contract.RequestHistoryEntry, 0, len(stored.Items)), NextCursor: stored.NextCursor}
	for _, entry := range stored.Items {
		view, err := s.projector.History(ctx, exec, record, entry)
		if err != nil {
			return contract.Page[contract.RequestHistoryEntry]{}, err
		}
		result.Items = append(result.Items, view)
	}
	return result, nil
}

func (s *Service) Response(ctx context.Context, exec policy.Execution, requestID contract.RequestID, decisionID contract.DecisionID) (contract.ResponseDetail, error) {
	record, scope, err := s.visibleRecord(ctx, exec, requestID)
	if err != nil {
		return contract.ResponseDetail{}, err
	}
	decision, err := s.store.Response(ctx, scope, requestID, decisionID)
	if err != nil {
		return contract.ResponseDetail{}, err
	}
	return s.projector.Response(ctx, exec, record, decision)
}

func (s *Service) MarkRead(ctx context.Context, exec policy.Execution, id contract.RequestID) error {
	_, scope, err := s.visibleRecord(ctx, exec, id)
	if err != nil {
		return err
	}
	return s.store.MarkRead(ctx, scope, id)
}

func (s *Service) ExpireDue(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	if tenant == "" {
		return 0, contract.NewError(contract.CodeValidationFailed, "expiry tenant is required")
	}
	return s.store.ExpireDue(ctx, tenant, limit)
}

func (s *Service) visibleRecord(ctx context.Context, exec policy.Execution, id contract.RequestID) (RequestRecord, VisibilityScope, error) {
	if err := s.validateExecution(exec); err != nil {
		return RequestRecord{}, VisibilityScope{}, err
	}
	scope, err := s.visibilityScope(ctx, exec)
	if err != nil {
		return RequestRecord{}, VisibilityScope{}, err
	}
	record, err := s.store.Get(ctx, exec.Tenant(), id)
	if err != nil {
		return RequestRecord{}, VisibilityScope{}, err
	}
	if !scope.CanManageAll && !requestRecordVisible(record, scope.Actor) {
		return RequestRecord{}, VisibilityScope{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	return record, scope, nil
}

func (s *Service) visibilityScope(ctx context.Context, exec policy.Execution) (VisibilityScope, error) {
	scope, err := s.authorizer.Visibility(ctx, exec)
	if err != nil {
		return VisibilityScope{}, err
	}
	if scope.Tenant != exec.Tenant() || scope.Actor != contract.PrincipalID(exec.Actor().PrincipalID) {
		return VisibilityScope{}, contract.NewError(contract.CodeInternal, "request visibility scope does not match verified execution")
	}
	return scope, nil
}

func (s *Service) validateExecution(exec policy.Execution) error {
	if !exec.Valid() {
		return contract.NewError(contract.CodeUnauthenticated, "verified request execution is required")
	}
	if _, err := contract.ParsePrincipalID(exec.Actor().PrincipalID); err != nil {
		return contract.NewError(contract.CodeUnauthenticated, "verified request principal is invalid")
	}
	return nil
}

func (s *Service) writeIdentity(exec policy.Execution, operation string, key contract.IdempotencyKey, command any) (WriteIdentity, error) {
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return WriteIdentity{}, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	digest, err := canonicalDigest(command)
	if err != nil {
		return WriteIdentity{}, contract.NewError(contract.CodeValidationFailed, "request command cannot be canonicalized")
	}
	return WriteIdentity{Tenant: exec.Tenant(), Actor: contract.PrincipalID(exec.Actor().PrincipalID), Operation: operation, IdempotencyKey: key, CommandDigest: digest}, nil
}

func (s *Service) newRequestID() (contract.RequestID, error) {
	id := s.ids.NewID()
	if _, err := contract.ParseRequestID(id); err != nil {
		return "", contract.NewError(contract.CodeInternal, "ID source returned an invalid request ID")
	}
	return contract.RequestID(id), nil
}
func (s *Service) newAssignmentID() (contract.AssignmentID, error) {
	id := s.ids.NewID()
	if _, err := contract.ParseAssignmentID(id); err != nil {
		return "", contract.NewError(contract.CodeInternal, "ID source returned an invalid assignment ID")
	}
	return contract.AssignmentID(id), nil
}
func (s *Service) newDecisionID() (contract.DecisionID, error) {
	id := s.ids.NewID()
	if _, err := contract.ParseDecisionID(id); err != nil {
		return "", contract.NewError(contract.CodeInternal, "ID source returned an invalid decision ID")
	}
	return contract.DecisionID(id), nil
}

func (s *Service) event(exec policy.Execution, eventType string, requestID contract.RequestID, revision contract.Revision, status contract.RequestStatus, at time.Time) (contract.EventEnvelope, error) {
	id := s.ids.NewID()
	actor := exec.Actor()
	var visibleStatus *contract.RequestStatus
	if status != "" {
		value := status
		visibleStatus = &value
	}
	data, err := contract.EncodeData(struct {
		RequestID contract.RequestID      `json:"request_id"`
		Revision  contract.Revision       `json:"revision"`
		Status    *contract.RequestStatus `json:"status,omitempty"`
	}{requestID, revision, visibleStatus})
	if err != nil {
		return contract.EventEnvelope{}, err
	}
	event := contract.EventEnvelope{ID: id, Type: eventType, Version: contract.CurrentVersion, Source: "kora.engine", Site: actor.Site,
		AggregateType: "request", AggregateID: string(requestID), OccurredAt: at.UTC(), Actor: actor, Data: data}
	if err := event.Validate(); err != nil {
		return contract.EventEnvelope{}, err
	}
	return event, nil
}

func canonicalDigest(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", fmt.Errorf("trailing JSON data")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func requestRecordVisible(record RequestRecord, actor contract.PrincipalID) bool {
	if record.Requester == actor {
		return true
	}
	for _, assignment := range record.Assignments {
		if assignment.Principal == actor {
			return true
		}
	}
	return false
}

func validSubmitShape(cmd contract.SubmitRequest) bool {
	if !validRequestKind(cmd.Kind) {
		return false
	}
	if cmd.Kind == contract.RequestApproval {
		if cmd.Subject == nil || cmd.PolicyID == "" || cmd.Assignment != nil {
			return false
		}
	}
	if cmd.Kind == contract.RequestInformation {
		return cmd.InformationSchema != nil
	}
	return cmd.InformationSchema == nil
}

func incrementRevision(value contract.Revision) (contract.Revision, error) {
	next := nextRevision(value)
	if next == "" {
		return "", contract.NewError(contract.CodeConflict, "request revision cannot be advanced")
	}
	return next, nil
}

func validAssignmentMode(mode contract.AssignmentMode) bool {
	return mode == contract.AssignmentAny || mode == contract.AssignmentAll || mode == contract.AssignmentSequential
}

func validateAuthorizedAssignment(requested *contract.AssignmentSpec, authorization SubmissionAuthorization) error {
	if requested == nil {
		return nil
	}
	if requested.Mode != authorization.AssignmentMode {
		return contract.NewError(contract.CodePermissionDenied, "requested assignment mode is not authorized")
	}
	requestedTargets := make(map[contract.Assignee]struct{}, len(requested.Assignees))
	for _, target := range requested.Assignees {
		requestedTargets[target] = struct{}{}
	}
	authorizedTargets := make(map[contract.Assignee]struct{}, len(authorization.Assignments))
	for _, assignment := range authorization.Assignments {
		authorizedTargets[assignment.OriginalTarget] = struct{}{}
	}
	if len(requestedTargets) != len(authorizedTargets) {
		return contract.NewError(contract.CodePermissionDenied, "requested assignments do not match authorized targets")
	}
	for target := range requestedTargets {
		if _, ok := authorizedTargets[target]; !ok {
			return contract.NewError(contract.CodePermissionDenied, "requested assignments do not match authorized targets")
		}
	}
	return nil
}

func validRequestKind(kind contract.RequestKind) bool {
	return kind == contract.RequestApproval || kind == contract.RequestReview || kind == contract.RequestInformation || kind == contract.RequestAcknowledgment || kind == contract.RequestTask
}

func validAssignmentResponses(kind contract.RequestKind, responses []contract.ResponseKind) bool {
	if len(responses) == 0 {
		return false
	}
	allowed := make(map[contract.ResponseKind]bool)
	switch kind {
	case contract.RequestApproval:
		allowed[contract.ResponseApprove], allowed[contract.ResponseReject], allowed[contract.ResponseReturn] = true, true, true
	case contract.RequestReview, contract.RequestTask:
		allowed[contract.ResponseComplete], allowed[contract.ResponseReturn] = true, true
	case contract.RequestInformation:
		allowed[contract.ResponseProvide] = true
	case contract.RequestAcknowledgment:
		allowed[contract.ResponseAcknowledge] = true
	default:
		return false
	}
	seen := make(map[contract.ResponseKind]bool)
	for _, response := range responses {
		if !allowed[response] || seen[response] {
			return false
		}
		seen[response] = true
	}
	return true
}

func responseAllowedByKind(kind contract.RequestKind, response contract.ResponseKind) bool {
	return validAssignmentResponses(kind, []contract.ResponseKind{response})
}

func requireRevision(revision contract.Revision) (contract.Revision, error) {
	parsed, err := contract.ParseRevision(string(revision))
	if err != nil {
		return "", contract.NewError(contract.CodeValidationFailed, "expected request revision is required")
	}
	return parsed, nil
}

func executionPrincipal(exec policy.Execution) (contract.PrincipalID, error) {
	id := exec.Actor().PrincipalID
	principal, err := contract.ParsePrincipalID(id)
	if err != nil {
		return "", contract.NewError(contract.CodeUnauthenticated, "verified request principal is invalid")
	}
	return principal, nil
}

func sameSubject(a, b *contract.SubjectRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.ID == b.ID && a.Revision == b.Revision
}
func cloneSubject(value *contract.SubjectRef) *contract.SubjectRef {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneSchema(value *contract.SchemaRef) *contract.SchemaRef {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}
func cloneInformation(value *contract.InformationResponse) *contract.InformationResponse {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Values = append(json.RawMessage(nil), value.Values...)
	return &copy
}
func nextRevision(value contract.Revision) contract.Revision {
	parsed, err := contract.ParseRevision(string(value))
	if err != nil {
		return ""
	}
	n, err := strconv.ParseUint(string(parsed), 10, 64)
	if err != nil || n == ^uint64(0) {
		return ""
	}
	return contract.Revision(strconv.FormatUint(n+1, 10))
}

func uniqueResponses(assignments []AssignmentRecord) []contract.ResponseKind {
	seen := make(map[contract.ResponseKind]bool)
	var values []contract.ResponseKind
	for _, assignment := range assignments {
		for _, response := range assignment.AllowedResponses {
			if !seen[response] {
				seen[response] = true
				values = append(values, response)
			}
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}
