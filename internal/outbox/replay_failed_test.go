package outbox

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestReplayFailedResetsEligibleRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE _kora_outbox SET status = 'pending', next_attempt_at = NULL, last_error = NULL`).
		WithArgs(8).
		WillReturnResult(sqlmock.NewResult(0, 3))

	admission := &dispatcherAdmissionProbe{}
	p := &Publisher{DB: db, Dialect: kdb.Resolve("mysql"), MaxAttempts: 8, WorkAdmission: admission}
	n, err := p.ReplayFailed(context.Background())
	if err != nil {
		t.Fatalf("ReplayFailed: %v", err)
	}
	if n != 3 {
		t.Fatalf("ReplayFailed = %d, want 3", n)
	}
	if admission.calls.Load() != 1 || admission.active.Load() != 0 {
		t.Fatalf("replay admission calls/active = %d/%d, want 1/0", admission.calls.Load(), admission.active.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
