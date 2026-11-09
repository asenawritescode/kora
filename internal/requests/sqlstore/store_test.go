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
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
)

func TestCreateCommitsRequestAssignmentsHistoryOutboxAndReceiptTogether(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	expectNoCreateReceipt(mock, change.Identity)
	mock.ExpectBegin()
	expectNoCreateReceiptTx(mock, change.Identity)
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectExec("INSERT INTO _kora_request").WithArgs(
		string(change.Request.ID), string(change.Identity.Tenant), int64(1), string(change.Request.Kind), string(change.Request.AssignmentMode), string(change.Request.Status), string(change.Request.Requester),
		nil, nil, nil, nil, "", change.Request.Title, change.Request.Message, nil, nil, nil, "", "", "", "", "", int64(0), "", "", sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_assignment").WithArgs(
		string(change.Assignments[0].ID), string(change.Identity.Tenant), string(change.Request.ID), int64(change.Assignments[0].Stage),
		string(contract.AssigneeUser), string(change.Assignments[0].Principal), string(change.Assignments[0].OriginalTarget.Kind), change.Assignments[0].OriginalTarget.ID,
		string(change.Assignments[0].Status), `["complete","return_for_changes"]`, string(change.Assignments[0].MembershipRevision), sqlmock.AnyArg(), sqlmock.AnyArg(),
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs(
		string(change.Identity.Tenant), string(change.Request.ID), int64(1), string(contract.HistoryCreated), string(change.Identity.Actor), nil, nil, "", nil, nil, change.History.At,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	expectSourceSequence(mock, change.Events[0].Site, 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WithArgs(
		change.Events[0].ID, change.Events[0].Site, int64(1), change.Events[0].Type, change.Events[0].Version,
		change.Events[0].AggregateType, change.Events[0].AggregateID, sqlmock.AnyArg(), "pending", 0, change.Events[0].OccurredAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_idempotency").WithArgs(
		string(change.Identity.Tenant), string(change.Identity.Actor), change.Identity.Operation, string(change.Identity.IdempotencyKey),
		change.Identity.CommandDigest, sqlmock.AnyArg(), change.Request.CreatedAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	got, err := store.Create(context.Background(), change)
	if err != nil {
		t.Fatal("Create:", err)
	}
	if got.ID != change.Request.ID || got.Tenant != change.Identity.Tenant {
		t.Fatalf("Create returned wrong request identity: %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("transaction expectations:", err)
	}
}

func TestCreateInTxLeavesCommitAndRollbackToCaller(t *testing.T) {
	database, mock, store, change := createTestStore(t)
	mock.ExpectBegin()
	expectNoCreateReceiptTx(mock, change.Identity)
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectExec("INSERT INTO _kora_request").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_assignment").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnResult(sqlmock.NewResult(1, 1))
	expectSourceSequence(mock, "tenant-route", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_idempotency").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectRollback()

	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin caller transaction:", err)
	}
	got, err := store.CreateInTx(context.Background(), tx, change)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal("CreateInTx:", err)
	}
	if got.ID != change.Request.ID {
		t.Fatalf("CreateInTx returned ID %q, want %q", got.ID, change.Request.ID)
	}
	// The request store must leave the enclosing transaction open. Roll it back
	// as its workflow composer would when wait registration later fails.
	if err := tx.Rollback(); err != nil {
		t.Fatal("caller rollback:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("transaction expectations:", err)
	}
}

func TestCreateInTxRejectsChangedIdempotencyContentWithoutReacquiringConnection(t *testing.T) {
	database, mock, store, change := createTestStore(t)
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), change.Identity.Operation, string(change.Identity.IdempotencyKey)).
		WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow("different-digest", `{"request_id":"01K7REQUEST000000000000000000"}`, true))
	if _, err := store.CreateInTx(context.Background(), tx, change); err == nil {
		t.Fatal("changed transaction-bound retry was accepted")
	} else {
		var apiErr *contract.Error
		if !errors.As(err, &apiErr) || apiErr.Type != contract.CodeIdempotencyKeyReused {
			t.Fatalf("changed transaction-bound retry error = %v", err)
		}
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("caller transaction was not left open:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("transaction-bound replay expectations:", err)
	}
}

func TestCreateRollsBackWhenHistoryWriteFails(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	expectNoCreateReceipt(mock, change.Identity)
	mock.ExpectBegin()
	expectNoCreateReceiptTx(mock, change.Identity)
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectExec("INSERT INTO _kora_request").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_assignment").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnError(errors.New("history unavailable"))
	mock.ExpectRollback()
	expectNoCreateReceipt(mock, change.Identity)

	if _, err := store.Create(context.Background(), change); err == nil || !strings.Contains(err.Error(), "history unavailable") {
		t.Fatalf("Create error = %v, want history failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("transaction expectations:", err)
	}
}

func TestCreateRejectsStaleAuthorizationEpochBeforeWritingRequest(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	expectNoCreateReceipt(mock, change.Identity)
	mock.ExpectBegin()
	expectNoCreateReceiptTx(mock, change.Identity)
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").
		WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(4, 4))
	mock.ExpectRollback()
	expectNoCreateReceipt(mock, change.Identity)
	if _, err := store.Create(context.Background(), change); err == nil || !strings.Contains(err.Error(), "authorization changed") {
		t.Fatalf("Create with stale authorization epoch error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("stale epoch must prevent request writes:", err)
	}
}

func TestCreateRollsBackWhenOutboxPublicationCannotBeRecorded(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	expectNoCreateReceipt(mock, change.Identity)
	mock.ExpectBegin()
	expectNoCreateReceiptTx(mock, change.Identity)
	mock.ExpectQuery("SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1 LOCK IN SHARE MODE").WillReturnRows(sqlmock.NewRows([]string{"policy_revision", "membership_revision"}).AddRow(3, 4))
	mock.ExpectExec("INSERT INTO _kora_request").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_assignment").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnResult(sqlmock.NewResult(1, 1))
	expectSourceSequence(mock, "tenant-route", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WillReturnError(errors.New("outbox unavailable"))
	mock.ExpectRollback()
	expectNoCreateReceipt(mock, change.Identity)

	if _, err := store.Create(context.Background(), change); err == nil || !strings.Contains(err.Error(), "outbox unavailable") {
		t.Fatalf("Create error = %v, want outbox failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("transaction expectations:", err)
	}
}

func expectSourceSequence(mock sqlmock.Sqlmock, tenant string, sequence int64) {
	mock.ExpectExec("INSERT INTO _kora_tenant_event_sequence").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT last_sequence FROM _kora_tenant_event_sequence WHERE id = 1 FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(sequence - 1))
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(last_sequence\\), 0\\) FROM _kora_event_sequence").
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(0))
	mock.ExpectExec("UPDATE _kora_tenant_event_sequence SET last_sequence = \\? WHERE id = 1").WithArgs(sequence).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestCreateReplaysSameIdempotencyKeyAndRejectsDifferentCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		digest  string
		wantErr contract.Code
	}{
		{name: "same command", digest: strings.Repeat("a", 64)},
		{name: "different command", digest: strings.Repeat("b", 64), wantErr: contract.CodeIdempotencyKeyReused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mock, store, change := createTestStore(t)
			change.Identity.CommandDigest = tc.digest
			encoded, err := json.Marshal(createReceipt{RequestID: change.Request.ID})
			if err != nil {
				t.Fatal(err)
			}
			expectNoCreateReceipt(mock, change.Identity)
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT command_digest, receipt_json,").
				WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), change.Identity.Operation, string(change.Identity.IdempotencyKey)).
				WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow(strings.Repeat("a", 64), string(encoded), true))
			if tc.wantErr == "" {
				mock.ExpectQuery("SELECT\\s+id, tenant_id, revision,").
					WithArgs(string(change.Identity.Tenant), string(change.Request.ID)).
					WillReturnRows(sqlmock.NewRows([]string{
						"id", "tenant_id", "revision", "kind", "assignment_mode", "status", "requester_id", "supersedes_request_id",
						"subject_kind", "subject_id", "subject_revision", "subject_digest", "title", "message",
						"due_at", "outcome", "resolved_at", "continuation_operation_id", "continuation_state",
						"policy_id", "policy_revision", "information_schema_id", "information_schema_version",
						"source_instance_id", "source_step_id", "created_at", "updated_at",
					}).AddRow(
						string(change.Request.ID), string(change.Identity.Tenant), int64(1), string(change.Request.Kind), string(change.Request.AssignmentMode), string(change.Request.Status), string(change.Request.Requester),
						nil, nil, nil, nil, "", change.Request.Title, change.Request.Message, nil, nil, nil, "", "", "", "", "", int64(0), "", "", change.Request.CreatedAt, change.Request.UpdatedAt,
					))
				assignment := change.Assignments[0]
				encodedResponses, err := json.Marshal(assignment.AllowedResponses)
				if err != nil {
					t.Fatal(err)
				}
				mock.ExpectQuery("SELECT id, request_id, stage, assignee_kind,").WithArgs(string(change.Identity.Tenant), string(change.Request.ID)).
					WillReturnRows(sqlmock.NewRows([]string{"id", "request_id", "stage", "assignee_kind", "assignee_id", "original_assignee_kind", "original_assignee_id", "status", "allowed_responses", "membership_revision"}).
						AddRow(string(assignment.ID), string(assignment.RequestID), int64(assignment.Stage), string(contract.AssigneeUser), string(assignment.Principal), string(assignment.OriginalTarget.Kind), assignment.OriginalTarget.ID, string(assignment.Status), string(encodedResponses), string(assignment.MembershipRevision)))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
				mock.ExpectQuery("SELECT command_digest, receipt_json,").
					WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), change.Identity.Operation, string(change.Identity.IdempotencyKey)).
					WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow(strings.Repeat("a", 64), string(encoded), true))
			}

			got, err := store.Create(context.Background(), change)
			if tc.wantErr != "" {
				var contractErr *contract.Error
				if !errors.As(err, &contractErr) || contractErr.Type != tc.wantErr {
					t.Fatalf("Create error = %v, want code %s", err, tc.wantErr)
				}
			} else if err != nil || got.ID != change.Request.ID || got.Status != change.Request.Status {
				t.Fatalf("Create replay = %#v, %v", got, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal("transaction expectations:", err)
			}
		})
	}
}

func expectNoCreateReceipt(mock sqlmock.Sqlmock, identity requests.WriteIdentity) {
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey)).
		WillReturnError(sql.ErrNoRows)
}

func expectNoCreateReceiptTx(mock sqlmock.Sqlmock, identity requests.WriteIdentity) {
	expectNoCreateReceipt(mock, identity)
}

func TestCreateRejectsMismatchedIdentityBeforeDatabaseAccess(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	change.Identity.Actor = "other-user"
	if _, err := store.Create(context.Background(), change); err == nil {
		t.Fatal("mismatched write actor was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("validation should reject before database access:", err)
	}
}

func TestCreateRejectsExpiredIdempotencyReceiptWithoutReplaying(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	mock.ExpectQuery("SELECT command_digest, receipt_json,").
		WithArgs(string(change.Identity.Tenant), string(change.Identity.Actor), change.Identity.Operation, string(change.Identity.IdempotencyKey)).
		WillReturnRows(sqlmock.NewRows([]string{"command_digest", "receipt_json", "active"}).AddRow(change.Identity.CommandDigest, `{}`, false))
	_, err := store.Create(context.Background(), change)
	var contractErr *contract.Error
	if !errors.As(err, &contractErr) || contractErr.Type != contract.CodeConflict {
		t.Fatalf("Create error = %v, want expired retry conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("expired receipt must not start a transaction:", err)
	}
}

func TestMarkReadRecordsMonotonicPerPrincipalWatermark(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	scope := requests.VisibilityScope{Tenant: change.Identity.Tenant, Actor: change.Request.Requester}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT r.revision FROM _kora_request r").
		WithArgs(string(scope.Tenant), string(change.Request.ID), string(scope.Actor), "user", string(scope.Actor)).
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(int64(7)))
	mock.ExpectExec("INSERT INTO _kora_request_read").
		WithArgs(string(change.Request.ID), string(scope.Actor), int64(7)).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := store.MarkRead(context.Background(), scope, change.Request.ID); err != nil {
		t.Fatal("MarkRead:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("read receipt transaction expectations:", err)
	}
}

func TestMarkReadDoesNotRevealMissingRequests(t *testing.T) {
	_, mock, store, change := createTestStore(t)
	scope := requests.VisibilityScope{Tenant: change.Identity.Tenant, Actor: change.Request.Requester}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT r.revision FROM _kora_request r").
		WithArgs(string(scope.Tenant), string(change.Request.ID), string(scope.Actor), "user", string(scope.Actor)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	if err := store.MarkRead(context.Background(), scope, change.Request.ID); err == nil {
		t.Fatal("missing request read receipt was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("missing request transaction expectations:", err)
	}
}

func TestIdempotencyExpiryUsesDatabaseClockForEachDialect(t *testing.T) {
	for _, tc := range []struct {
		dialect string
		want    string
	}{
		{dialect: "libsql", want: "datetime('now', '+30 days')"},
		{dialect: "postgres", want: "NOW() + INTERVAL '30 days'"},
		{dialect: "mysql", want: "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL 30 DAY)"},
	} {
		if got := idempotencyExpiryExpression(db.Resolve(tc.dialect)); got != tc.want {
			t.Errorf("%s expiry expression = %q, want %q", tc.dialect, got, tc.want)
		}
	}
}

func TestReadReceiptUpsertNeverMovesTheWatermarkBackward(t *testing.T) {
	for _, dialectName := range []string{"libsql", "postgres", "mysql"} {
		query := readReceiptUpsert(db.Resolve(dialectName))
		if !strings.Contains(query, "history_sequence") {
			t.Errorf("%s read receipt upsert omitted watermark", dialectName)
		}
		if !strings.Contains(query, "MAX(") && !strings.Contains(query, "GREATEST(") {
			t.Errorf("%s read receipt upsert can move watermark backward: %s", dialectName, query)
		}
	}
}

func createTestStore(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *Store, requests.CreateCommit) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)
	requestID := contract.RequestID("01K7REQUEST00000000000000000")
	assignmentID := contract.AssignmentID("01K7ASSIGNMENT000000000000000")
	actor := contract.PrincipalID("requester-1")
	request := requests.RequestRecord{
		Tenant: "tenant-1", ID: requestID, Revision: "1", Kind: contract.RequestReview, AssignmentMode: contract.AssignmentAny,
		Status: contract.RequestPending, Requester: actor, Title: "Review expense", Message: "Check the receipt",
		CreatedAt: now, UpdatedAt: now,
	}
	assignment := requests.AssignmentRecord{
		ID: assignmentID, RequestID: requestID, Principal: "reviewer-1",
		OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "reviewer-1"},
		Stage:          1, Status: contract.AssignmentPending,
		AllowedResponses: []contract.ResponseKind{contract.ResponseComplete, contract.ResponseReturn}, MembershipRevision: "4",
	}
	createdBy := actor
	change := requests.CreateCommit{
		Identity: requests.WriteIdentity{
			Tenant: "tenant-1", Actor: actor, Operation: requestCreateOperation,
			IdempotencyKey: "submit-request-1", CommandDigest: strings.Repeat("a", 64),
		},
		Request: request, ExpectedPolicyRevision: "3", ExpectedMembershipRevision: "4",
		Assignments: []requests.AssignmentRecord{assignment},
		History:     contract.RequestHistoryEntry{Sequence: "1", Kind: contract.HistoryCreated, Actor: &createdBy, At: now},
		Events: []contract.EventEnvelope{{
			ID: "01K7EVENT0000000000000000000", Type: "request.created", Version: 1,
			Source: "engine", Site: "tenant-route", AggregateType: "request", AggregateID: string(requestID),
			OccurredAt: now, Data: json.RawMessage(`{"request_id":"01K7REQUEST00000000000000000"}`),
		}},
	}
	return database, mock, store, change
}
