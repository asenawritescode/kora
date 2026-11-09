package ai

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestResumeRunRotatesTokenAndRejectsStaleToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	initialToken := "resume-old"
	runRows := sqlmock.NewRows([]string{
		"id", "site", "conversation_id", "channel", "status", "model", "provider", "current_step_id", "summary", "input_message", "output_message", "error_message", "cancel_reason", "resume_token", "created_at", "updated_at", "completed_at", "cancelled_at", "retention_expires_at",
	}).AddRow("run-1", "site-a", "conv-1", "chat", "planning", "gpt-4o", "openai_api_key", "", "", "hello", "", "", "", initialToken, time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC))
	mock.ExpectQuery("SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at FROM _kora_ai_run WHERE id = \\?").
		WithArgs("run-1").
		WillReturnRows(runRows)
	mock.ExpectExec("INSERT INTO _kora_ai_run").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at FROM _kora_ai_run WHERE id = \\?").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "site", "conversation_id", "channel", "status", "model", "provider", "current_step_id", "summary", "input_message", "output_message", "error_message", "cancel_reason", "resume_token", "created_at", "updated_at", "completed_at", "cancelled_at", "retention_expires_at",
		}).AddRow("run-1", "site-a", "conv-1", "chat", "planning", "gpt-4o", "openai_api_key", "", "", "hello", "", "", "", "resume-new", time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)))

	rec, err := ResumeRun(context.Background(), db, "run-1", initialToken)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if rec.ResumeToken == initialToken || rec.ResumeToken == "" {
		t.Fatalf("expected token rotation, got %q", rec.ResumeToken)
	}

	if _, err := ResumeRun(context.Background(), db, "run-1", initialToken); err == nil {
		t.Fatal("expected stale resume token to be rejected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRunReturnsCancelledSnapshotWithoutMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	token := "resume-terminal"
	mock.ExpectQuery("SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at FROM _kora_ai_run WHERE id = \\?").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "site", "conversation_id", "channel", "status", "model", "provider", "current_step_id", "summary", "input_message", "output_message", "error_message", "cancel_reason", "resume_token", "created_at", "updated_at", "completed_at", "cancelled_at", "retention_expires_at",
		}).AddRow("run-1", "site-a", "conv-1", "chat", "cancelled", "gpt-4o", "openai_api_key", "step-1", "summary", "hello", "", "", "user request", token, now, now, now, now, time.Time{}))

	rec, err := ResumeRun(context.Background(), db, "run-1", token)
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if rec.Status != "cancelled" || rec.ResumeToken != token {
		t.Fatalf("unexpected snapshot: %+v", rec)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupExpiredRemovesRunGraph(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM _kora_ai_message").
		WithArgs(now, now).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM _kora_ai_step").
		WithArgs(now, now).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM _kora_ai_run").
		WithArgs(now, now).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM _kora_ai_conversation").
		WithArgs(now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	removed, err := CleanupExpired(context.Background(), db, now)
	if err != nil {
		t.Fatalf("CleanupExpired: %v", err)
	}
	if removed != 7 {
		t.Fatalf("removed = %d, want 7", removed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRunSummaryPersistsLatestStepSummary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	createdAt := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)
	completedAt := time.Date(2026, 8, 13, 9, 40, 0, 0, time.UTC)
	cancelledAt := time.Date(2026, 8, 13, 9, 50, 0, 0, time.UTC)
	retentionExpiresAt := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)

	runRows := sqlmock.NewRows([]string{
		"id", "site", "conversation_id", "channel", "status", "model", "provider", "current_step_id", "summary", "input_message", "output_message", "error_message", "cancel_reason", "resume_token", "created_at", "updated_at", "completed_at", "cancelled_at", "retention_expires_at",
	}).AddRow(
		"run-1", "site-a", "conv-1", "chat", "completed", "gpt-4o", "openai_api_key", "step-1", "old summary", "hello", "assistant answer", "", "", "resume-1",
		createdAt, updatedAt, completedAt, cancelledAt, retentionExpiresAt,
	)
	mock.ExpectQuery("SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at FROM _kora_ai_run WHERE id = \\?").
		WithArgs("run-1").
		WillReturnRows(runRows)
	mock.ExpectQuery("SELECT COALESCE\\(summary, ''\\) FROM _kora_ai_step WHERE run_id = \\? ORDER BY created_at DESC LIMIT 1").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"summary"}).AddRow("compacted summary"))
	mock.ExpectExec("INSERT INTO _kora_ai_run").
		WithArgs(
			"run-1", "site-a", "conv-1", "chat", "completed", "gpt-4o", "openai_api_key", "step-1",
			"compacted summary", "hello", "assistant answer", "", "", "resume-1",
			sqlmock.AnyArg(), sqlmock.AnyArg(), completedAt, cancelledAt, retentionExpiresAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_ai_conversation").
		WithArgs(
			"conv-1", "site-a", "chat", "", "", "compacted summary", "completed", "run-1",
			sqlmock.AnyArg(), retentionExpiresAt, sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RefreshRunSummary(context.Background(), db, "run-1"); err != nil {
		t.Fatalf("RefreshRunSummary: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshRunSummaryPrefersLatestStepSummary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at FROM _kora_ai_run WHERE id = \\?").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "site", "conversation_id", "channel", "status", "model", "provider", "current_step_id", "summary", "input_message", "output_message", "error_message", "cancel_reason", "resume_token", "created_at", "updated_at", "completed_at", "cancelled_at", "retention_expires_at",
		}).AddRow("run-1", "site-a", "conv-1", "chat", "planning", "gpt-4o", "openai_api_key", "", "old summary", "hello", "assistant reply", "", "", "resume-1", time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC), time.Date(2026, 8, 13, 11, 0, 0, 0, time.UTC)))
	mock.ExpectQuery("SELECT COALESCE\\(summary, ''\\) FROM _kora_ai_step WHERE run_id = \\? ORDER BY created_at DESC LIMIT 1").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"summary"}).AddRow("step summary"))
	mock.ExpectExec("INSERT INTO _kora_ai_run").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_ai_conversation").
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RefreshRunSummary(context.Background(), db, "run-1"); err != nil {
		t.Fatalf("RefreshRunSummary: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
