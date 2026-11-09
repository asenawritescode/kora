package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
)

func TestCancelCommitsRequestAssignmentsHistoryAndReceipt(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := cancelTestCommit()
	expectCancelStart(mock, change)
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	mock.ExpectExec("UPDATE _kora_request SET revision = ").WithArgs(
		int64(2), "cancelled", string(change.Identity.Tenant), string(change.RequestID), int64(1), "pending",
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WithArgs(
		"cancelled", string(change.Identity.Tenant), string(change.RequestID), "pending", "blocked",
	).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs(
		string(change.Identity.Tenant), string(change.RequestID), int64(2), "cancelled", string(change.Identity.Actor), nil, nil, change.Reason, nil, nil, now,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_idempotency").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectCancelledRequestGet(mock, change, now)
	got, err := store.Cancel(context.Background(), change)
	if err != nil {
		t.Fatal("Cancel:", err)
	}
	if got.Status != contract.RequestCancelled || got.Revision != "2" {
		t.Fatalf("cancel result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelRollsBackWhenHistoryWriteFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := cancelTestCommit()
	expectCancelStart(mock, change)
	mock.ExpectExec("UPDATE _kora_request SET revision = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnError(errors.New("history write failed"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestCancelOperation, string(change.Identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
	if _, err := store.Cancel(context.Background(), change); err == nil || !strings.Contains(err.Error(), "history write failed") {
		t.Fatalf("Cancel history failure = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectCancelStart(mock sqlmock.Sqlmock, change requests.CancelCommit) {
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestCancelOperation, string(change.Identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").
		WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectQuery("SELECT revision, status, ").
		WithArgs(string(change.Identity.Tenant), string(change.RequestID)).
		WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "is_due", "now"}).AddRow(1, "pending", false, time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)))
}

func cancelTestCommit() requests.CancelCommit {
	return requests.CancelCommit{
		Identity: requests.WriteIdentity{Tenant: "tenant-1", Actor: "requester-1", Operation: requestCancelOperation,
			IdempotencyKey: "cancel-request-1", CommandDigest: strings.Repeat("b", 64)},
		RequestID: "01K7REQUEST00000000000000000", ExpectedRevision: "1", ExpectedPolicyRevision: "3", ExpectedMembershipRevision: "4", Reason: "No longer needed",
	}
}

func expectCancelledRequestGet(mock sqlmock.Sqlmock, change requests.CancelCommit, now time.Time) {
	created := now.Add(-time.Minute)
	mock.ExpectQuery("SELECT\\s+id, tenant_id, revision, kind, assignment_mode, status, requester_id,").
		WithArgs(string(change.Identity.Tenant), string(change.RequestID)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "tenant_id", "revision", "kind", "assignment_mode", "status", "requester_id", "supersedes_request_id",
			"subject_kind", "subject_id", "subject_revision", "subject_digest", "title", "message", "due_at", "outcome", "resolved_at",
			"continuation_operation_id", "continuation_state", "policy_id", "policy_revision", "information_schema_id", "information_schema_version",
			"source_instance_id", "source_step_id", "created_at", "updated_at",
		}).AddRow(
			string(change.RequestID), string(change.Identity.Tenant), int64(2), "review", "any", "cancelled", string(change.Identity.Actor),
			nil, nil, nil, nil, "", "Check", "Please review", nil, nil, nil, "", "", "", "", "", int64(0), "", "", created, now,
		))
	mock.ExpectQuery("SELECT id, request_id, stage, assignee_kind,").WithArgs(string(change.Identity.Tenant), string(change.RequestID)).WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "stage", "assignee_kind", "assignee_id", "original_assignee_kind", "original_assignee_id", "status", "allowed_responses", "membership_revision"}))
}
