package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

func TestCommitDecisionAppliesQuorumTransitionsAtomically(t *testing.T) {
	for _, mode := range []struct {
		name     string
		pending  int64
		nextID   string
		resolved bool
	}{
		{name: "any", resolved: true},
		{name: "all complete", pending: 0, resolved: true},
		{name: "all waiting", pending: 1},
		{name: "sequential next", nextID: "01K7NEXTASSIGNMENT0000000000000"},
		{name: "sequential final", resolved: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			store, err := New(database, db.Resolve("mysql"))
			if err != nil {
				t.Fatal(err)
			}
			change := decisionTestCommit(mode.name)
			expectDecisionStart(mock, change, 1, mode.name)
			mock.ExpectQuery("SELECT assignee_id, stage, status, allowed_responses FROM _kora_request_assignment").
				WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID)).
				WillReturnRows(sqlmock.NewRows([]string{"assignee_id", "stage", "status", "allowed_responses"}).AddRow(string(change.Decision.Actor), 1, "pending", `["approve","reject"]`))
			if strings.HasPrefix(mode.name, "sequential") {
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM _kora_request_assignment").
					WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), "pending", "blocked", int64(1), int64(1), string(change.Decision.AssignmentID)).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			mock.ExpectExec("INSERT INTO _kora_request_decision").WithArgs(
				string(change.Decision.ID), string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID),
				string(change.Decision.Actor), "", "approve", "ok", nil, change.Decision.At,
			).WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
			if strings.HasPrefix(mode.name, "all") {
				mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM _kora_request_assignment").
					WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), "pending").
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(mode.pending))
			}
			if strings.HasPrefix(mode.name, "sequential") && mode.nextID != "" {
				mock.ExpectQuery("SELECT id FROM _kora_request_assignment").
					WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), "blocked", int64(1)).
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(mode.nextID))
				mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
			} else if strings.HasPrefix(mode.name, "sequential") {
				mock.ExpectQuery("SELECT id FROM _kora_request_assignment").
					WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), "blocked", int64(1)).
					WillReturnError(sql.ErrNoRows)
			}
			if mode.resolved {
				mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 2))
			}
			mock.ExpectExec("UPDATE _kora_request SET revision = ").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs(
				string(change.Identity.Tenant), string(change.Decision.RequestID), int64(2), "responded", string(change.Decision.Actor),
				string(change.Decision.AssignmentID), "approve", "ok", nil, nil, change.Decision.At,
			).WillReturnResult(sqlmock.NewResult(0, 1))
			if mode.resolved {
				mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs(
					string(change.Identity.Tenant), string(change.Decision.RequestID), int64(3), "resolved", string(change.Decision.Actor), nil, nil, "", nil, nil, change.Decision.At,
				).WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec("INSERT INTO _kora_request_idempotency").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			receipt, err := store.CommitDecision(context.Background(), change)
			if err != nil {
				t.Fatal("CommitDecision:", err)
			}
			if receipt.Status == contract.RequestResolved != mode.resolved {
				t.Fatalf("receipt status = %q, resolved=%v", receipt.Status, mode.resolved)
			}
			if receipt.Revision != "2" {
				t.Fatalf("receipt revision = %q", receipt.Revision)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommitDecisionRejectsStaleRequestBeforeDecisionInsert(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := decisionTestCommit("any")
	expectDecisionStart(mock, change, 2, "any")
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestRespondOperation, string(change.Identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
	if _, err := store.CommitDecision(context.Background(), change); err == nil || !strings.Contains(err.Error(), "request changed") {
		t.Fatalf("stale decision error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectDecisionStart(mock sqlmock.Sqlmock, change requests.DecisionCommit, requestRevision int64, mode string) {
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestRespondOperation, string(change.Identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").
		WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectQuery("SELECT revision, status, assignment_mode,").
		WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID)).
		WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "assignment_mode", "subject_kind", "subject_id", "subject_revision", "is_due"}).
			AddRow(requestRevision, "pending", decisionAssignmentMode(mode), nil, nil, nil, false))
}

func decisionTestCommit(mode string) requests.DecisionCommit {
	at := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	decisionID := contract.DecisionID("01K7DECISION000000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT000000000000000")
	actor := contract.PrincipalID("reviewer-1")
	digest := strings.Repeat("a", 64)
	return requests.DecisionCommit{
		Request:          requests.RequestRecord{Tenant: "tenant-1", ID: requestID, Revision: "1"},
		ExpectedRevision: "1", ExpectedPolicyRevision: "3", ExpectedMembershipRevision: "4",
		Decision: requests.DecisionRecord{Tenant: "tenant-1", ID: decisionID, RequestID: requestID, AssignmentID: assignmentID,
			Actor: actor, Response: contract.ResponseApprove, Comment: "ok", At: at},
		Receipt:  contract.ResponseReceipt{RequestID: requestID, DecisionID: decisionID},
		Identity: requests.WriteIdentity{Tenant: "tenant-1", Actor: actor, Operation: requestRespondOperation, IdempotencyKey: "respond-key-1", CommandDigest: digest},
	}
}

func decisionAssignmentMode(name string) string {
	if strings.HasPrefix(name, "all") {
		return "all"
	}
	if strings.HasPrefix(name, "sequential") {
		return "sequential"
	}
	return "any"
}

func TestCommitDecisionRollsBackWhenHistoryCannotBePersisted(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := decisionTestCommit("any")
	expectDecisionStart(mock, change, 1, "any")
	mock.ExpectQuery("SELECT assignee_id, stage, status, allowed_responses FROM _kora_request_assignment").
		WithArgs(string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID)).
		WillReturnRows(sqlmock.NewRows([]string{"assignee_id", "stage", "status", "allowed_responses"}).AddRow(string(change.Decision.Actor), 1, "pending", `["approve"]`))
	mock.ExpectExec("INSERT INTO _kora_request_decision").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request SET revision = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnError(errors.New("history write failed"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestRespondOperation, string(change.Identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
	if _, err := store.CommitDecision(context.Background(), change); err == nil || !strings.Contains(err.Error(), "history write failed") {
		t.Fatalf("CommitDecision history failure = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitDecisionReplaysStoredReceipt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := decisionTestCommit("any")
	prior := contract.ResponseReceipt{RequestID: change.Decision.RequestID, DecisionID: change.Decision.ID, Revision: "2", Status: contract.RequestResolved}
	raw, err := json.Marshal(decisionReceiptEnvelope{Receipt: prior})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestRespondOperation, string(change.Identity.IdempotencyKey)).
		WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow(change.Identity.CommandDigest, string(raw), true))
	got, err := store.CommitDecision(context.Background(), change)
	if err != nil {
		t.Fatal("CommitDecision replay:", err)
	}
	if got != prior {
		t.Fatalf("replayed receipt = %#v, want %#v", got, prior)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthorizationEpochLockAllowsSharedReadsWhereSupported(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		writer       bool
	}{
		{name: "mysql", suffix: " LOCK IN SHARE MODE"},
		{name: "postgres", suffix: " FOR SHARE"},
		{name: "libsql", writer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if tc.writer {
				mock.ExpectExec("UPDATE _kora_auth_revision SET id = id WHERE id = 1").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1" + tc.suffix).
				WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
			if err := lockAndCheckAuthorizationRevision(context.Background(), tx, db.Resolve(tc.name), "3", "4"); err != nil {
				t.Fatal(err)
			}
			mock.ExpectCommit()
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
