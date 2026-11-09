package ai

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRecordAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ctx := enrichAuditContext(context.Background(), "user-1", "sid-1", "corr-1", "idem-1")
	mock.ExpectExec("INSERT INTO _kora_ai_audit").
		WithArgs(
			sqlmock.AnyArg(),
			"site-a",
			"run-1",
			"step-1",
			"conv-1",
			"model_attempt",
			"gpt-4o",
			"completed",
			"user-1",
			"sid-1",
			"corr-1",
			"idem-1",
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordAudit(ctx, db, AuditEvent{
		Site:           "site-a",
		RunID:          "run-1",
		StepID:         "step-1",
		ConversationID: "conv-1",
		Kind:           "model_attempt",
		Name:           "gpt-4o",
		Status:         "completed",
		Details:        map[string]any{"round": 1},
	}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
