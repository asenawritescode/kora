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
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

func TestReassignCommitsAssignmentHistoryAndReceiptTogether(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := reassignTestCommit()
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	expectReassignStart(mock, change, now)
	mock.ExpectQuery("SELECT status, assignee_kind, assignee_id FROM _kora_request_assignment").WithArgs(string(change.Identity.Tenant), string(change.RequestID), string(change.AssignmentID)).
		WillReturnRows(sqlmock.NewRows([]string{"status", "assignee_kind", "assignee_id"}).AddRow("pending", "user", "old-user"))
	mock.ExpectExec("UPDATE _kora_request_assignment SET assignee_kind = ").WithArgs("user", string(change.Replacement.Principal), "4", "pending", string(change.Identity.Tenant), string(change.RequestID), string(change.AssignmentID), "pending", "blocked").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request SET revision = ").WithArgs(int64(2), string(change.Identity.Tenant), string(change.RequestID), int64(1), "pending").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs(string(change.Identity.Tenant), string(change.RequestID), int64(2), "reassigned", string(change.Identity.Actor), string(change.AssignmentID), nil, change.Reason, `{"kind":"user","id":"old-user"}`, `{"kind":"user","id":"new-user"}`, now).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_idempotency").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectReassignedRequestGet(mock, change, now)
	got, err := store.Reassign(context.Background(), change)
	if err != nil {
		t.Fatal("Reassign:", err)
	}
	if got.ID != change.RequestID || got.Revision != "2" {
		t.Fatalf("result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReassignRollsBackWhenHistoryCannotBeWritten(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := reassignTestCommit()
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	expectReassignStart(mock, change, now)
	mock.ExpectQuery("SELECT status, assignee_kind, assignee_id FROM _kora_request_assignment").WithArgs(string(change.Identity.Tenant), string(change.RequestID), string(change.AssignmentID)).WillReturnRows(sqlmock.NewRows([]string{"status", "assignee_kind", "assignee_id"}).AddRow("pending", "user", "old-user"))
	mock.ExpectExec("UPDATE _kora_request_assignment SET assignee_kind = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request SET revision = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnError(errors.New("history write failed"))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT command_digest, receipt_json,").WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestReassignOperation, string(change.Identity.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
	if _, err := store.Reassign(context.Background(), change); err == nil || !strings.Contains(err.Error(), "history write failed") {
		t.Fatalf("Reassign error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectReassignStart(mock sqlmock.Sqlmock, change requests.ReassignCommit, now time.Time) {
	mock.ExpectQuery("SELECT command_digest, receipt_json,").WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestReassignOperation, string(change.Identity.IdempotencyKey)).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectQuery("SELECT revision, status,").WithArgs(string(change.Identity.Tenant), string(change.RequestID)).WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "is_due", "now"}).AddRow(1, "pending", false, now))
}

func reassignTestCommit() requests.ReassignCommit {
	return requests.ReassignCommit{
		Identity:  requests.WriteIdentity{Tenant: "tenant-1", Actor: "manager-1", Operation: requestReassignOperation, IdempotencyKey: "reassign-1", CommandDigest: strings.Repeat("c", 64)},
		RequestID: "01K7REQUEST00000000000000000", ExpectedRevision: "1", ExpectedPolicyRevision: "3", ExpectedMembershipRevision: "4",
		AssignmentID: "01K7ASSIGNMENT0000000000000000", Replacement: requests.AssignmentRecord{ID: "01K7ASSIGNMENT0000000000000000", RequestID: "01K7REQUEST00000000000000000", Principal: "new-user", Status: contract.AssignmentPending}, Reason: "Delegated to the current owner",
	}
}

func expectReassignedRequestGet(mock sqlmock.Sqlmock, change requests.ReassignCommit, now time.Time) {
	mock.ExpectQuery("SELECT\\s+id, tenant_id, revision, kind, assignment_mode, status, requester_id,").WithArgs(string(change.Identity.Tenant), string(change.RequestID)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "tenant_id", "revision", "kind", "assignment_mode", "status", "requester_id", "supersedes_request_id", "subject_kind", "subject_id", "subject_revision", "subject_digest", "title", "message", "due_at", "outcome", "resolved_at", "continuation_operation_id", "continuation_state", "policy_id", "policy_revision", "information_schema_id", "information_schema_version", "source_instance_id", "source_step_id", "created_at", "updated_at"}).
			AddRow(string(change.RequestID), string(change.Identity.Tenant), int64(2), "review", "any", "pending", string(change.Identity.Actor), nil, nil, nil, nil, "", "Check", "Review this", nil, nil, nil, "", "", "", "", "", int64(0), "", "", now.Add(-time.Minute), now))
	mock.ExpectQuery("SELECT id, request_id, stage, assignee_kind,").WithArgs(string(change.Identity.Tenant), string(change.RequestID)).WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "stage", "assignee_kind", "assignee_id", "original_assignee_kind", "original_assignee_id", "status", "allowed_responses", "membership_revision"}))
}

func TestReassignReplaysReceiptAndReturnsCurrentRequest(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	change := reassignTestCommit()
	mock.ExpectQuery("SELECT command_digest, receipt_json,").WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), requestReassignOperation, string(change.Identity.IdempotencyKey)).
		WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow(change.Identity.CommandDigest, `{"request_id":"`+string(change.RequestID)+`"}`, true))
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	expectReassignedRequestGet(mock, change, now)
	got, err := store.Reassign(context.Background(), change)
	if err != nil {
		t.Fatal("Reassign replay:", err)
	}
	if got.ID != change.RequestID || got.Revision != "2" {
		t.Fatalf("replay result = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
