package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestExpireDueCommitsTerminalStateAssignmentsHistoryAndEvent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	databaseNow := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, revision, CURRENT_TIMESTAMP\\(6\\) FROM _kora_request").WithArgs("tenant-1", "pending", int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "revision", "database_now"}).AddRow(string(id), int64(3), databaseNow))
	mock.ExpectExec("UPDATE _kora_request SET status = ").WithArgs("expired", int64(4), databaseNow, databaseNow, "tenant-1", string(id), int64(3), "pending", databaseNow).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WithArgs("cancelled", databaseNow, "tenant-1", string(id), "pending", "blocked").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs("tenant-1", string(id), int64(4), "expired", nil, nil, nil, "Request deadline passed", nil, nil, databaseNow).WillReturnResult(sqlmock.NewResult(0, 1))
	expectSourceSequence(mock, "tenant-1", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WithArgs(sqlmock.AnyArg(), "tenant-1", int64(1), "request.expired", 1, "request", string(id), sqlmock.AnyArg(), "pending", 0, databaseNow).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	count, err := store.ExpireDue(context.Background(), "tenant-1", 10)
	if err != nil {
		t.Fatal("ExpireDue:", err)
	}
	if count != 1 {
		t.Fatalf("expired count = %d, want 1", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExpireDueRollsBackWhenEventCannotBeRecorded(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	id := "01K7REQUEST00000000000000000"
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id, revision, CURRENT_TIMESTAMP\\(6\\) FROM _kora_request").WillReturnRows(sqlmock.NewRows([]string{"id", "revision", "database_now"}).AddRow(id, int64(3), now))
	mock.ExpectExec("UPDATE _kora_request SET status = ").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO _kora_request_history").WillReturnResult(sqlmock.NewResult(0, 1))
	expectSourceSequence(mock, "tenant-1", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WillReturnError(errors.New("outbox unavailable"))
	mock.ExpectRollback()
	if _, err := store.ExpireDue(context.Background(), "tenant-1", 1); err == nil {
		t.Fatal("ExpireDue accepted an event persistence failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkStaleCommitsStateAssignmentsHistoryAndEvent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT revision, status, subject_kind, subject_id, subject_revision,").WithArgs("tenant-1", string(id)).
		WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "subject_kind", "subject_id", "subject_revision", "now"}).AddRow(int64(3), "pending", "invoice", "inv-9", "8", now))
	mock.ExpectExec("UPDATE _kora_request SET status = ").WithArgs("stale", int64(4), now, "tenant-1", string(id), int64(3), "pending").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_request_assignment SET status = ").WithArgs("cancelled", now, "tenant-1", string(id), "pending", "blocked").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO _kora_request_history").WithArgs("tenant-1", string(id), int64(4), "stale", nil, nil, nil, "Subject revision changed", nil, nil, now).WillReturnResult(sqlmock.NewResult(0, 1))
	expectSourceSequence(mock, "tenant-1", 1)
	mock.ExpectExec("INSERT INTO _kora_outbox").WithArgs(sqlmock.AnyArg(), "tenant-1", int64(1), "request.stale", 1, "request", string(id), sqlmock.AnyArg(), "pending", 0, now).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.MarkStale(context.Background(), "tenant-1", id, "3", contract.SubjectRef{Kind: "invoice", ID: "inv-9", Revision: "9"}); err != nil {
		t.Fatal("MarkStale:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkStaleRejectsUnchangedSubjectWithoutMutation(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT revision, status, subject_kind, subject_id, subject_revision,").WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "subject_kind", "subject_id", "subject_revision", "now"}).AddRow(int64(3), "pending", "invoice", "inv-9", "9", now))
	mock.ExpectRollback()
	if err := store.MarkStale(context.Background(), "tenant-1", id, "3", contract.SubjectRef{Kind: "invoice", ID: "inv-9", Revision: "9"}); err == nil {
		t.Fatal("MarkStale accepted unchanged subject")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkStaleDuplicateReconciliationIsIdempotent(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	id := contract.RequestID("01K7REQUEST00000000000000000")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT revision, status, subject_kind, subject_id, subject_revision,").WithArgs("tenant-1", string(id)).
		WillReturnRows(sqlmock.NewRows([]string{"revision", "status", "subject_kind", "subject_id", "subject_revision", "now"}).AddRow(int64(4), "stale", "invoice", "inv-9", "8", time.Now().UTC()))
	mock.ExpectCommit()
	if err := store.MarkStale(context.Background(), "tenant-1", id, "3", contract.SubjectRef{Kind: "invoice", ID: "inv-9", Revision: "9"}); err != nil {
		t.Fatalf("duplicate MarkStale reconciliation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
