package requests

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type generatedIDs struct{}

func (generatedIDs) NewID() string { return contract.NewID() }

type serviceTestAuthorizer struct {
	submission                                               SubmissionAuthorization
	response                                                 ResponseAuthorization
	management                                               ManagementAuthorization
	scope                                                    VisibilityScope
	submissionErr, responseErr, managementErr, visibilityErr error
	responseActor                                            string
}

func (a *serviceTestAuthorizer) Submission(_ context.Context, _ policy.Execution, _ contract.SubmitRequest) (SubmissionAuthorization, error) {
	return a.submission, a.submissionErr
}
func (a *serviceTestAuthorizer) WorkflowSubmission(_ context.Context, _ policy.Execution, _ contract.SubmitRequest, _ contract.RequestSource) (SubmissionAuthorization, error) {
	return a.submission, a.submissionErr
}
func (a *serviceTestAuthorizer) Response(_ context.Context, exec policy.Execution, _ RequestRecord, _ contract.RespondRequest) (ResponseAuthorization, error) {
	a.responseActor = exec.Actor().PrincipalID
	return a.response, a.responseErr
}
func (a *serviceTestAuthorizer) Management(_ context.Context, _ policy.Execution, _ RequestRecord, _ string, _ *contract.ReassignRequest) (ManagementAuthorization, error) {
	return a.management, a.managementErr
}
func (a *serviceTestAuthorizer) Visibility(context.Context, policy.Execution) (VisibilityScope, error) {
	return a.scope, a.visibilityErr
}

type serviceTestProjector struct{}

func (serviceTestProjector) Request(_ context.Context, _ policy.Execution, record RequestRecord) (contract.RequestView, error) {
	assignments := make([]contract.AssignmentView, 0, len(record.Assignments))
	for _, assignment := range record.Assignments {
		assignments = append(assignments, contract.AssignmentView{ID: assignment.ID, Stage: assignment.Stage, Assignee: contract.Assignee{Kind: contract.AssigneeUser, ID: string(assignment.Principal)}, Status: assignment.Status, AllowedResponses: append([]contract.ResponseKind(nil), assignment.AllowedResponses...)})
	}
	return contract.RequestView{ID: record.ID, Revision: record.Revision, Kind: record.Kind, Status: record.Status, Requester: record.Requester, Subject: record.Subject, Title: record.Title, Message: record.Message, Assignments: assignments, AllowedResponses: uniqueResponses(record.Assignments), CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}, nil
}
func (serviceTestProjector) History(_ context.Context, _ policy.Execution, _ RequestRecord, entry contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	return entry, nil
}
func (serviceTestProjector) Response(_ context.Context, _ policy.Execution, _ RequestRecord, value DecisionRecord) (contract.ResponseDetail, error) {
	return contract.ResponseDetail{DecisionID: value.ID, RequestID: value.RequestID, AssignmentID: value.AssignmentID, Response: value.Response, Comment: value.Comment, Information: value.Information, At: value.At}, nil
}

type serviceTestSchemas struct {
	calls int
	err   error
}

func (s *serviceTestSchemas) Registered(context.Context, contract.SchemaRef) error { return nil }
func (s *serviceTestSchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	s.calls++
	return s.err
}

type serviceTestStore struct {
	created                *CreateCommit
	decision               *DecisionCommit
	record                 RequestRecord
	receipt                contract.ResponseReceipt
	createErr, decisionErr error
	lookupReceipt          contract.ResponseReceipt
	lookupFound            bool
	lookupErr              error
	lookupIdentity         WriteIdentity
}

type transactionalServiceTestStore struct {
	*serviceTestStore
	tx          *sql.Tx
	commit      int
	createdInTx int
}

func (s *transactionalServiceTestStore) CreateInTx(_ context.Context, tx *sql.Tx, change CreateCommit) (RequestRecord, error) {
	s.tx = tx
	s.createdInTx++
	s.created = &change
	s.record = change.Request
	s.record.Assignments = change.Assignments
	return s.record, nil
}

func (s *transactionalServiceTestStore) CommitDecisionInTx(_ context.Context, tx *sql.Tx, change DecisionCommit) (contract.ResponseReceipt, error) {
	s.tx = tx
	s.commit++
	s.decision = &change
	return s.receipt, s.decisionErr
}

func (s *serviceTestStore) Get(_ context.Context, _ contract.TenantID, id contract.RequestID) (RequestRecord, error) {
	if s.record.ID != id {
		return RequestRecord{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	return s.record, nil
}
func (s *serviceTestStore) CommitDecision(_ context.Context, change DecisionCommit) (contract.ResponseReceipt, error) {
	s.decision = &change
	if s.decisionErr != nil {
		return contract.ResponseReceipt{}, s.decisionErr
	}
	return s.receipt, nil
}
func (s *serviceTestStore) LookupResponseReceipt(_ context.Context, identity WriteIdentity) (contract.ResponseReceipt, bool, error) {
	s.lookupIdentity = identity
	return s.lookupReceipt, s.lookupFound, s.lookupErr
}
func (s *serviceTestStore) Create(_ context.Context, change CreateCommit) (RequestRecord, error) {
	s.created = &change
	if s.createErr != nil {
		return RequestRecord{}, s.createErr
	}
	s.record = change.Request
	s.record.Assignments = change.Assignments
	return s.record, nil
}
func (s *serviceTestStore) ListVisible(context.Context, VisibilityScope, contract.RequestFilter) (contract.Page[RequestRecord], error) {
	return contract.Page[RequestRecord]{}, nil
}
func (s *serviceTestStore) Reassign(context.Context, ReassignCommit) (RequestRecord, error) {
	return RequestRecord{}, errors.New("unexpected Reassign")
}
func (s *serviceTestStore) Cancel(context.Context, CancelCommit) (RequestRecord, error) {
	return RequestRecord{}, errors.New("unexpected Cancel")
}
func (s *serviceTestStore) History(context.Context, VisibilityScope, contract.RequestID, contract.PageRequest) (contract.Page[contract.RequestHistoryEntry], error) {
	return contract.Page[contract.RequestHistoryEntry]{}, nil
}
func (s *serviceTestStore) Response(context.Context, VisibilityScope, contract.RequestID, contract.DecisionID) (DecisionRecord, error) {
	return DecisionRecord{}, nil
}
func (s *serviceTestStore) MarkRead(context.Context, VisibilityScope, contract.RequestID) error {
	return nil
}
func (s *serviceTestStore) ExpireDue(context.Context, contract.TenantID, uint16) (uint16, error) {
	return 0, nil
}
func (s *serviceTestStore) MarkStale(context.Context, contract.TenantID, contract.RequestID, contract.Revision, contract.SubjectRef) error {
	return nil
}

func testExecution(t *testing.T) policy.Execution {
	t.Helper()
	exec, err := policy.NewVerifiedExecution("tenant-1", contract.ActorContext{PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", AuthenticatedAt: time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func newTestService(t *testing.T, store *serviceTestStore, auth *serviceTestAuthorizer, schemas *serviceTestSchemas) *Service {
	t.Helper()
	svc, err := NewService(store, auth, serviceTestProjector{}, schemas, fixedClock{time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)}, generatedIDs{})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestSubmitUsesVerifiedActorAndAuthorizedAssignmentSnapshot(t *testing.T) {
	exec := testExecution(t)
	auth := &serviceTestAuthorizer{submission: SubmissionAuthorization{PolicyRevision: "5", MembershipRevision: "7", AssignmentMode: contract.AssignmentAny,
		Assignments: []AssignmentRecord{{Principal: "bob", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "bob"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "7"}}}}
	store := &serviceTestStore{}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "submit-key-1"}, Kind: contract.RequestTask, Title: "Complete review", Message: "Review the invoice",
		Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "bob"}}}}
	view, err := svc.Submit(context.Background(), exec, cmd)
	if err != nil {
		t.Fatal("Submit:", err)
	}
	if store.created == nil {
		t.Fatal("Submit did not call durable store")
	}
	if view.Requester != "alice" || store.created.Request.Requester != "alice" || store.created.Request.Tenant != "tenant-1" {
		t.Fatalf("caller identity was not derived from Execution: %#v", store.created.Request)
	}
	if len(store.created.Assignments) != 1 || store.created.Assignments[0].Principal != "bob" || store.created.Assignments[0].RequestID != view.ID || store.created.Assignments[0].ID == "" {
		t.Fatalf("authorized assignment snapshot = %#v", store.created.Assignments)
	}
	if store.created.ExpectedPolicyRevision != "5" || store.created.ExpectedMembershipRevision != "7" || len(store.created.Events) != 1 {
		t.Fatalf("submission commit omitted authorization fences or event: %#v", store.created)
	}
	if string(store.created.Events[0].Data) != `{"request_id":"`+string(view.ID)+`","revision":"1","status":"pending"}` {
		t.Fatalf("request event contains unexpected data: %s", store.created.Events[0].Data)
	}
}

func TestPreparedSubmissionUsesVerifiedAuthorizationAndCallerTransaction(t *testing.T) {
	exec := testExecution(t)
	auth := &serviceTestAuthorizer{submission: SubmissionAuthorization{PolicyRevision: "5", MembershipRevision: "7", AssignmentMode: contract.AssignmentAny,
		Assignments: []AssignmentRecord{{Principal: "bob", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "bob"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "7"}}}}
	base := &serviceTestStore{}
	store := &transactionalServiceTestStore{serviceTestStore: base}
	svc, err := NewService(store, auth, serviceTestProjector{}, &serviceTestSchemas{}, fixedClock{time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)}, generatedIDs{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "workflow-human-key-1"}, Kind: contract.RequestTask, Title: "Review", Message: "Please review"}
	source := contract.RequestSource{InstanceID: "01K7INSTANCE00000000000000000", StepID: "manager-approval"}
	prepared, err := svc.PrepareWorkflowSubmit(context.Background(), exec, cmd, source)
	if err != nil {
		t.Fatal("PrepareSubmit:", err)
	}
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.CommitInTx(context.Background(), tx); err != nil {
		t.Fatal("CommitInTx:", err)
	}
	if store.createdInTx != 1 || store.tx != tx || store.created == nil || store.created.Request.Requester != "alice" || store.created.Request.Tenant != "tenant-1" || len(store.created.Assignments) != 1 || store.created.Assignments[0].Principal != "bob" || store.created.Request.Source == nil || *store.created.Request.Source != source {
		t.Fatalf("prepared request lost verified identity/assignment or transaction: %+v", store.created)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("caller rollback:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestWorkflowSubmissionFailsClosedWithoutWorkflowPolicy(t *testing.T) {
	base := &serviceTestStore{}
	store := &transactionalServiceTestStore{serviceTestStore: base}
	svc, err := NewService(store, plainServiceTestAuthorizer{inner: &serviceTestAuthorizer{}}, serviceTestProjector{}, &serviceTestSchemas{}, fixedClock{time.Now().UTC()}, generatedIDs{})
	if err != nil {
		t.Fatal(err)
	}
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "workflow-no-policy-1"}, Kind: contract.RequestTask, Title: "Review", Message: "Please review"}
	source := contract.RequestSource{InstanceID: "01K7INSTANCE00000000000000000", StepID: "review"}
	if _, err := svc.PrepareWorkflowSubmit(context.Background(), testExecution(t), cmd, source); err == nil {
		t.Fatal("workflow request fell back to public user-submission policy")
	}
	if store.createdInTx != 0 || store.created != nil {
		t.Fatalf("missing workflow policy created a request: %+v", store)
	}
}

// plainServiceTestAuthorizer deliberately exposes only the ordinary user API.
type plainServiceTestAuthorizer struct{ inner *serviceTestAuthorizer }

func (a plainServiceTestAuthorizer) Submission(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest) (SubmissionAuthorization, error) {
	return a.inner.Submission(ctx, exec, cmd)
}
func (a plainServiceTestAuthorizer) Response(ctx context.Context, exec policy.Execution, record RequestRecord, cmd contract.RespondRequest) (ResponseAuthorization, error) {
	return a.inner.Response(ctx, exec, record, cmd)
}
func (a plainServiceTestAuthorizer) Management(ctx context.Context, exec policy.Execution, record RequestRecord, operation string, command *contract.ReassignRequest) (ManagementAuthorization, error) {
	return a.inner.Management(ctx, exec, record, operation, command)
}
func (a plainServiceTestAuthorizer) Visibility(ctx context.Context, exec policy.Execution) (VisibilityScope, error) {
	return a.inner.Visibility(ctx, exec)
}

func TestSubmitAllowsServerPolicyToSupplyDirectAssignment(t *testing.T) {
	exec := testExecution(t)
	auth := &serviceTestAuthorizer{submission: SubmissionAuthorization{PolicyRevision: "1", MembershipRevision: "1", AssignmentMode: contract.AssignmentAny,
		Assignments: []AssignmentRecord{{Principal: "bob", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "bob"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}}}}}
	store := &serviceTestStore{}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "submit-key-2"}, Kind: contract.RequestTask, Title: "Review", Message: "Please review"}
	if _, err := svc.Submit(context.Background(), exec, cmd); err != nil {
		t.Fatal("Submit without caller-selected assignment:", err)
	}
	if store.created == nil || len(store.created.Assignments) != 1 || store.created.Assignments[0].Principal != "bob" {
		t.Fatalf("server policy assignment was not persisted: %#v", store.created)
	}
}

func TestSubmitRejectsChangedAuthorizedTargetOrMode(t *testing.T) {
	for name, requested := range map[string]*contract.AssignmentSpec{
		"target": {Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "charlie"}}},
		"mode":   {Mode: contract.AssignmentAll, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "bob"}}},
	} {
		t.Run(name, func(t *testing.T) {
			exec := testExecution(t)
			auth := &serviceTestAuthorizer{submission: SubmissionAuthorization{PolicyRevision: "1", MembershipRevision: "1", AssignmentMode: contract.AssignmentAny,
				Assignments: []AssignmentRecord{{Principal: "bob", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "bob"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}}}}}
			store := &serviceTestStore{}
			svc := newTestService(t, store, auth, &serviceTestSchemas{})
			cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "submit-key-3"}, Kind: contract.RequestTask, Title: "Review", Message: "Please review", Assignment: requested}
			if _, err := svc.Submit(context.Background(), exec, cmd); err == nil {
				t.Fatal("request accepted after authorized target or mode changed")
			}
			if store.created != nil {
				t.Fatal("unauthorized assignment reached persistence")
			}
		})
	}
}

func TestSubmitRejectsSelfApprovalBeforePersistence(t *testing.T) {
	exec := testExecution(t)
	auth := &serviceTestAuthorizer{submission: SubmissionAuthorization{PolicyID: "purchase-approval", PolicyRevision: "1", MembershipRevision: "1", SubjectDigest: strings.Repeat("a", 64), AssignmentMode: contract.AssignmentAny,
		Assignments: []AssignmentRecord{{Principal: "alice", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "alice"}, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseApprove}}}}}
	store := &serviceTestStore{}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "submit-key-1"}, Kind: contract.RequestApproval, Subject: &contract.SubjectRef{Kind: "Invoice", ID: "invoice-1", Revision: "2"}, PolicyID: "purchase-approval", Title: "Approve invoice", Message: "Approve?"}
	if _, err := svc.Submit(context.Background(), exec, cmd); err == nil {
		t.Fatal("self-approval assignment was accepted")
	}
	if store.created != nil {
		t.Fatal("self-approval reached persistence")
	}
}

func TestRespondRechecksCurrentAuthorizationAndUsesExecutionActor(t *testing.T) {
	exec := testExecution(t)
	assignmentID := contract.AssignmentID(contract.NewID())
	requestID := contract.RequestID(contract.NewID())
	store := &serviceTestStore{record: RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "3", Kind: contract.RequestTask, AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "owner",
		Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "8"}}},
		receipt: contract.ResponseReceipt{RequestID: requestID, DecisionID: contract.DecisionID(contract.NewID()), Revision: "4", Status: contract.RequestResolved}}
	auth := &serviceTestAuthorizer{response: ResponseAuthorization{PolicyRevision: "6", MembershipRevision: "8"}}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-key-1", ExpectedRevision: "3"}, AssignmentID: assignmentID, Response: contract.ResponseComplete, Comment: "Done"}
	receipt, err := svc.Respond(context.Background(), exec, requestID, cmd)
	if err != nil {
		t.Fatal("Respond:", err)
	}
	if receipt.Status != contract.RequestResolved || store.decision == nil {
		t.Fatalf("response receipt/commit = %#v / %#v", receipt, store.decision)
	}
	if store.decision.Decision.Actor != "alice" || auth.responseActor != "alice" {
		t.Fatalf("response actor was caller-controlled: decision=%q authorized=%q", store.decision.Decision.Actor, auth.responseActor)
	}
	if store.decision.ExpectedPolicyRevision != "6" || store.decision.ExpectedMembershipRevision != "8" || store.decision.ExpectedRevision != "3" {
		t.Fatalf("response commit omitted current policy fence: %#v", store.decision)
	}
}

func TestPreparedResponseCommitsThroughCallerTransaction(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	base := &serviceTestStore{record: RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "2", Kind: contract.RequestTask,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "owner",
		Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}}}}}
	store := &transactionalServiceTestStore{serviceTestStore: &serviceTestStore{record: base.record, receipt: contract.ResponseReceipt{RequestID: requestID, Status: contract.RequestResolved}}}
	auth := &serviceTestAuthorizer{response: ResponseAuthorization{PolicyRevision: "4", MembershipRevision: "5"}}
	svc := newTestService(t, store.serviceTestStore, auth, &serviceTestSchemas{})
	// NewService receives the base Store interface; replace with a service wired
	// to the transactional wrapper so PrepareResponse can verify the capability.
	svc.store = store
	cmd := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "prepared-1", ExpectedRevision: "2"}, AssignmentID: assignmentID, Response: contract.ResponseComplete}
	prepared, err := svc.PrepareResponse(context.Background(), exec, requestID, cmd)
	if err != nil {
		t.Fatal("prepare response:", err)
	}
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectRollback()
	receipt, err := prepared.CommitInTx(context.Background(), tx)
	if err != nil {
		t.Fatal("commit prepared response:", err)
	}
	if store.tx != tx || store.commit != 1 || receipt.RequestID != requestID || store.decision == nil || store.decision.ExpectedPolicyRevision != "4" {
		t.Fatalf("transaction-bound response = tx %p, calls %d, receipt %#v, change %#v", store.tx, store.commit, receipt, store.decision)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("caller rollback:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResponseRetryReturnsPriorReceiptBeforeCurrentAssignmentAuthorization(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT0000000000000000")
	receipt := contract.ResponseReceipt{RequestID: requestID, DecisionID: "01K7DECISION000000000000000000", Revision: "2", Status: contract.RequestResolved}
	store := &serviceTestStore{record: RequestRecord{Tenant: exec.Tenant(), ID: requestID, Revision: "2", Kind: contract.RequestApproval, Status: contract.RequestResolved, Requester: "requester", Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice"}}}, lookupReceipt: receipt, lookupFound: true}
	authorizer := &serviceTestAuthorizer{scope: VisibilityScope{Tenant: exec.Tenant(), Actor: "alice"}, responseErr: errors.New("completed assignment is no longer eligible")}
	svc := newTestService(t, store, authorizer, &serviceTestSchemas{})
	command := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-retry-1", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseApprove}
	got, err := svc.Respond(context.Background(), exec, requestID, command)
	if err != nil {
		t.Fatal("same-command retry should return its original receipt:", err)
	}
	if got != receipt {
		t.Fatalf("retry receipt=%+v, want %+v", got, receipt)
	}
	if authorizer.responseActor != "" || store.decision != nil {
		t.Fatalf("replay re-ran response authorization or commit: actor=%q decision=%+v", authorizer.responseActor, store.decision)
	}
	if store.lookupIdentity.Actor != "alice" || store.lookupIdentity.Operation != opRespond || store.lookupIdentity.CommandDigest == "" {
		t.Fatalf("receipt lookup identity was not canonical/verified: %+v", store.lookupIdentity)
	}
}

func TestPreparedResponseMarksReplaySoComposerCanSkipDomainEffect(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT0000000000000000")
	receipt := contract.ResponseReceipt{RequestID: requestID, DecisionID: "01K7DECISION000000000000000000", Revision: "2", Status: contract.RequestResolved}
	store := &serviceTestStore{record: RequestRecord{Tenant: exec.Tenant(), ID: requestID, Revision: "2", Kind: contract.RequestApproval, Status: contract.RequestResolved, Requester: "requester", Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice"}}}, lookupReceipt: receipt, lookupFound: true}
	authorizer := &serviceTestAuthorizer{scope: VisibilityScope{Tenant: exec.Tenant(), Actor: "alice"}}
	svc := newTestService(t, store, authorizer, &serviceTestSchemas{})
	prepared, err := svc.PrepareResponse(context.Background(), exec, requestID, contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-retry-2", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseApprove})
	if err != nil {
		t.Fatal("prepare retry:", err)
	}
	got, replay := prepared.ReplayReceipt()
	if !replay || got != receipt {
		t.Fatalf("prepared replay=(%+v,%t), want prior receipt", got, replay)
	}
}

func TestResponseReplayStillRequiresCurrentRequestVisibility(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT0000000000000000")
	receipt := contract.ResponseReceipt{RequestID: requestID, DecisionID: "01K7DECISION000000000000000000", Revision: "2", Status: contract.RequestResolved}
	store := &serviceTestStore{record: RequestRecord{Tenant: exec.Tenant(), ID: requestID, Revision: "2", Kind: contract.RequestApproval, Status: contract.RequestResolved, Requester: "requester", Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "former-assignee"}}}, lookupReceipt: receipt, lookupFound: true}
	authorizer := &serviceTestAuthorizer{scope: VisibilityScope{Tenant: exec.Tenant(), Actor: "alice"}}
	svc := newTestService(t, store, authorizer, &serviceTestSchemas{})
	_, err := svc.Respond(context.Background(), exec, requestID, contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-hidden-retry", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseApprove})
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodeNotFound {
		t.Fatalf("retry for a no-longer-visible request error=%v, want not found", err)
	}
	if authorizer.responseActor != "" || store.decision != nil {
		t.Fatalf("hidden replay continued into response authorization/commit: actor=%q decision=%+v", authorizer.responseActor, store.decision)
	}
}

func TestRespondDoesNotCommitWhenMembershipRecheckFails(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	store := &serviceTestStore{record: RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "1", Kind: contract.RequestTask, AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending,
		Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}}}}}
	auth := &serviceTestAuthorizer{responseErr: contract.NewError(contract.CodePermissionDenied, "membership was revoked")}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-key-1", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseComplete}
	if _, err := svc.Respond(context.Background(), exec, requestID, cmd); err == nil {
		t.Fatal("response was accepted after authorization denial")
	}
	if store.decision != nil {
		t.Fatal("unauthorized response reached persistence")
	}
}

func TestRespondRejectsChangedSubjectContentBeforeCommit(t *testing.T) {
	exec := testExecution(t)
	requestID := contract.RequestID(contract.NewID())
	assignmentID := contract.AssignmentID(contract.NewID())
	subject := &contract.SubjectRef{Kind: "Invoice", ID: "invoice-1", Revision: "4"}
	store := &serviceTestStore{record: RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "1", Kind: contract.RequestApproval,
		AssignmentMode: contract.AssignmentAny, Status: contract.RequestPending, Requester: "owner", Subject: subject, SubjectDigest: strings.Repeat("a", 64),
		Assignments: []AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice", Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseApprove}}}}}
	auth := &serviceTestAuthorizer{response: ResponseAuthorization{PolicyRevision: "1", MembershipRevision: "1", ExpectedSubject: subject, SubjectDigest: strings.Repeat("b", 64)}}
	svc := newTestService(t, store, auth, &serviceTestSchemas{})
	cmd := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "respond-key-2", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseApprove}
	if _, err := svc.Respond(context.Background(), exec, requestID, cmd); err == nil {
		t.Fatal("response was accepted for changed subject content")
	} else {
		var coded *contract.Error
		if !errors.As(err, &coded) || coded.Type != contract.CodeStaleSubject {
			t.Fatalf("changed-subject response error = %v, want STALE_SUBJECT", err)
		}
	}
	if store.decision != nil {
		t.Fatal("changed-subject response reached persistence")
	}
}

func TestCanonicalCommandDigestIgnoresObjectKeyOrder(t *testing.T) {
	left, err := canonicalDigest(map[string]any{"payload": map[string]any{"b": 2, "a": 1}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalDigest(map[string]any{"payload": map[string]any{"a": 1, "b": 2}})
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("canonical digest differs: %s != %s", left, right)
	}
}
