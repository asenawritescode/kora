package process

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
)

type replayStore struct {
	requests.Store
	record  requests.RequestRecord
	receipt contract.ResponseReceipt
	found   bool
}

func (s *replayStore) Get(_ context.Context, _ contract.TenantID, id contract.RequestID) (requests.RequestRecord, error) {
	if s.record.ID != id {
		return requests.RequestRecord{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	return s.record, nil
}
func (s *replayStore) LookupResponseReceipt(_ context.Context, _ requests.WriteIdentity) (contract.ResponseReceipt, bool, error) {
	return s.receipt, s.found, nil
}

type replayAuthorizer struct {
	requests.Authorizer
	scope requests.VisibilityScope
}

func (a *replayAuthorizer) Visibility(context.Context, policy.Execution) (requests.VisibilityScope, error) {
	return a.scope, nil
}

type replayProjector struct{ requests.Projector }

func (replayProjector) Request(context.Context, policy.Execution, requests.RequestRecord) (contract.RequestView, error) {
	return contract.RequestView{}, nil
}

type replaySchemas struct{ requests.ResponseSchemas }

func (replaySchemas) Registered(context.Context, contract.SchemaRef) error { return nil }
func (replaySchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	return nil
}

type replayClock struct{}

func (replayClock) Now() time.Time { return time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC) }

type replayIDs struct{}

func (replayIDs) NewID() string { return contract.NewID() }

func TestApproveAndApplyReplayReturnsReceiptWithoutOpeningTenantTransaction(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-stable-1", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, SubjectUserID: "alice", Site: "tenant-route",
		Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("execution:", err)
	}
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT0000000000000000")
	receipt := contract.ResponseReceipt{RequestID: requestID, DecisionID: "01K7DECISION000000000000000000", Revision: "2", Status: contract.RequestResolved}
	store := &replayStore{found: true, receipt: receipt, record: requests.RequestRecord{
		Tenant: execution.Tenant(), ID: requestID, Revision: "2", Kind: contract.RequestApproval,
		Status: contract.RequestResolved, Requester: "alice", Assignments: []requests.AssignmentRecord{{ID: assignmentID, RequestID: requestID, Principal: "alice"}},
	}}
	authorizer := &replayAuthorizer{scope: requests.VisibilityScope{Tenant: execution.Tenant(), Actor: "alice"}}
	requestService, err := requests.NewService(store, authorizer, replayProjector{}, replaySchemas{}, replayClock{}, replayIDs{})
	if err != nil {
		t.Fatal("request service:", err)
	}
	transactionOpens := 0
	coordinator, err := NewCoordinator(requestService, kernel.New(db.Resolve("mysql"), nil), doctype.NewRegistry(), func(context.Context, contract.TenantID) (*sql.DB, *sql.Tx, func(), error) {
		transactionOpens++
		return nil, nil, nil, errors.New("transaction opener must not run on replay")
	}, map[string]WorkflowApproval{"invoice-approval-v1": {SubjectKind: "Invoice", Action: "approve"}})
	if err != nil {
		t.Fatal("coordinator:", err)
	}
	command := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "approval-retry-1", ExpectedRevision: "1"}, AssignmentID: assignmentID, Response: contract.ResponseApprove}
	got, err := coordinator.ApproveAndApply(context.Background(), execution, requestID, command)
	if err != nil {
		t.Fatal("ApproveAndApply replay:", err)
	}
	if got != receipt {
		t.Fatalf("receipt=%+v, want %+v", got, receipt)
	}
	if transactionOpens != 0 {
		t.Fatalf("replay opened %d tenant transactions, want zero", transactionOpens)
	}
}

func TestWorkflowOperationUsesOnlyPreparedSubjectAndRegisteredAction(t *testing.T) {
	execution, err := policy.NewVerifiedExecution("tenant-stable-1", contract.ActorContext{
		PrincipalID: "alice", PrincipalType: contract.PrincipalHuman, Site: "tenant-route", Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("execution:", err)
	}
	requestContext := requests.ResponseContext{Tenant: execution.Tenant(), RequestID: "01K7REQUEST00000000000000000", Kind: contract.RequestApproval,
		PolicyID: "invoice-approval-v1", Subject: &contract.SubjectRef{Kind: "Invoice", ID: "INV-1", Revision: "7"}}
	operation, err := workflowOperation(execution, requestContext, WorkflowApproval{Action: "approve"}, "client-key")
	if err != nil {
		t.Fatal("build workflow operation:", err)
	}
	if operation.Command != kernel.CommandWorkflowStatusTransition || operation.Context.Execution.Tenant() != execution.Tenant() || operation.Context.User != "alice" || operation.Context.IdempotencyKey == "client-key" {
		t.Fatalf("workflow operation does not carry verified, derived execution context: %+v", operation)
	}
	var payload kernel.WorkflowStatusTransitionPayload
	if err := json.Unmarshal(operation.Payload, &payload); err != nil {
		t.Fatal("decode operation payload:", err)
	}
	if payload.Doctype != "Invoice" || payload.Name != "INV-1" || payload.Action != "approve" || payload.ExpectedRevision != 7 {
		t.Fatalf("operation payload=%+v", payload)
	}
	requestContext.Subject.Revision = "07"
	if _, err := workflowOperation(execution, requestContext, WorkflowApproval{Action: "approve"}, "client-key"); err == nil {
		t.Fatal("non-canonical subject revision was accepted")
	}
}

func TestTenantTransactionOpenerAcquiresAdmissionOnceAndReusesRequestPermit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock database:", err)
	}
	defer database.Close()
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal("admission:", err)
	}
	opener, err := NewTenantTransactionOpener("tenant-a", database, admission)
	if err != nil {
		t.Fatal("opener:", err)
	}
	if _, _, _, err := opener(context.Background(), "tenant-b"); err == nil {
		t.Fatal("opener accepted a different tenant")
	}
	if admission.Snapshot().Active != 0 {
		t.Fatal("tenant mismatch consumed an admission slot")
	}

	requestContext, releaseRequest, err := dbaccess.AcquireWorkAdmission(context.Background(), admission)
	if err != nil {
		t.Fatal("acquire simulated HTTP request permit:", err)
	}
	mock.ExpectBegin()
	databaseForRequest, requestTx, releaseTx, err := opener(requestContext, "tenant-a")
	if err != nil || databaseForRequest != database || requestTx == nil || releaseTx == nil {
		t.Fatalf("open request transaction = (database match %t, transaction %t, release callback %t, error %v)", databaseForRequest == database, requestTx != nil, releaseTx != nil, err)
	}
	if got := admission.Snapshot(); got.Active != 1 || got.Waiters != 0 {
		t.Fatalf("nested transaction reacquired admission: %+v", got)
	}
	mock.ExpectCommit()
	if err := requestTx.Commit(); err != nil {
		t.Fatal("commit request transaction:", err)
	}
	releaseTx()
	if admission.Snapshot().Active != 1 {
		t.Fatal("transaction release freed the caller-owned request permit")
	}
	releaseRequest()
	if admission.Snapshot().Active != 0 {
		t.Fatal("request completion leaked its admission permit")
	}

	mock.ExpectBegin()
	_, backgroundTx, releaseBackground, err := opener(context.Background(), "tenant-a")
	if err != nil {
		t.Fatal("open background transaction:", err)
	}
	if admission.Snapshot().Active != 1 {
		t.Fatal("background transaction did not acquire active-work admission")
	}
	mock.ExpectCommit()
	if err := backgroundTx.Commit(); err != nil {
		t.Fatal("commit background transaction:", err)
	}
	releaseBackground()
	if got := admission.Snapshot(); got.Active != 0 || got.Waiters != 0 {
		t.Fatalf("background transaction leaked admission: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
