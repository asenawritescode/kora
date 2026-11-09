package ai

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAppendMessageDefaults(t *testing.T) {
	tests := []struct {
		name string
		msg  MessageRecord
		kind string
		seq  int
	}{
		{"zero kind and sequence", MessageRecord{Site: "s", ConversationID: "c", RunID: "r", Role: "user", Content: "hi", StepID: "st"}, "message", 1},
		{"negative sequence", MessageRecord{Site: "s", ConversationID: "c", RunID: "r", Role: "user", Content: "hi", Kind: "summary", Sequence: -4}, "summary", 1},
		{"explicit values", MessageRecord{Site: "s", ConversationID: "c", RunID: "r", Role: "assistant", Content: "yo", Kind: "summary", StepID: "st", Sequence: 7}, "summary", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			mock.ExpectExec("INSERT INTO _kora_ai_message").
				WithArgs(sqlmock.AnyArg(), tt.msg.Site, tt.msg.ConversationID, tt.msg.RunID, tt.msg.Role, tt.msg.Content, tt.kind, tt.msg.StepID, tt.seq, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(1, 1))
			if err := AppendMessage(context.Background(), db, tt.msg); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAppendConversationMessageAssignsNextSequence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT COALESCE\\(MAX\\(sequence\\), 0\\) \\+ 1").
		WithArgs("s", "c").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(5))
	mock.ExpectExec("INSERT INTO _kora_ai_message").
		WithArgs(sqlmock.AnyArg(), "s", "c", "r", "user", "hi", "message", "", 5, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	msg := MessageRecord{Site: "s", ConversationID: "c", RunID: "r", Role: "user", Content: "hi", Sequence: 99}
	if err := AppendConversationMessage(context.Background(), db, msg); err != nil {
		t.Fatalf("AppendConversationMessage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertStepIDDefault(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		wantA any
	}{
		{"empty ID is generated", "", sqlmock.AnyArg()},
		{"explicit ID is kept", "step-1", "step-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			mock.ExpectExec("INSERT INTO _kora_ai_step").
				WithArgs(tt.wantA, "s", "r", "c", "k", "planning", "sum", "tool", "in", "out", "err", sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(1, 1))
			step := StepRecord{ID: tt.id, Site: "s", RunID: "r", ConversationID: "c", StepKey: "k", Status: "planning", Summary: "sum", ToolName: "tool", InputJSON: "in", OutputJSON: "out", ErrorMessage: "err"}
			if err := UpsertStep(context.Background(), db, step); err != nil {
				t.Fatalf("UpsertStep: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpdateStepStatusNoopWithoutStepID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	if err := UpdateStepStatus(context.Background(), db, "", StepUpdate{Status: "failed"}); err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	mock.ExpectExec("UPDATE _kora_ai_step").
		WithArgs("failed", "", "", "", "out", "", sqlmock.AnyArg(), "step-1").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := UpdateStepStatus(context.Background(), db, "step-1", StepUpdate{Status: "failed", OutputJSON: "out"}); err != nil {
		t.Fatalf("UpdateStepStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueFollowUpTasksAppliesScopeAndDefaults(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec("INSERT INTO _kora_ai_task").
		WithArgs("t1", "s", "r", "c", "", "task", "first", "", "queued", 0, "", sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO _kora_ai_task").
		WithArgs("t2", "s", "r", "c", "", "followup", "second", "", "done", 0, "", sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	tasks := []TaskRecord{
		{ID: "t1", Site: "ignored", Title: "first"},
		{ID: "t2", RunID: "ignored", Title: "second", Kind: "followup", Status: "done"},
	}
	if err := QueueFollowUpTasks(context.Background(), db, RunScope{Site: "s", RunID: "r", ConversationID: "c"}, tasks); err != nil {
		t.Fatalf("QueueFollowUpTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReserveBudgetDefaults(t *testing.T) {
	tests := []struct {
		name       string
		req        BudgetRequest
		used       int
		wantTokens int
		wantErr    bool
	}{
		{"non-positive request counts as 1", BudgetRequest{Site: "s", Model: "m", RequestedTokens: 0, Budget: 100, Note: "n"}, 0, 1, false},
		{"zero budget is unlimited", BudgetRequest{Site: "s", Model: "m", RequestedTokens: 500, Budget: 0, Note: "n"}, 900, 500, false},
		{"over budget is refused", BudgetRequest{Site: "s", Model: "m", RequestedTokens: 20, Budget: 100, Note: "n"}, 90, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT COALESCE\\(SUM\\(requested_tokens\\), 0\\)").
				WithArgs("s", "m").
				WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(tt.used))
			if tt.wantErr {
				mock.ExpectRollback()
			} else {
				mock.ExpectExec("INSERT INTO _kora_ai_budget_reservation").
					WithArgs(sqlmock.AnyArg(), "s", "m", tt.wantTokens, "n", sqlmock.AnyArg()).
					WillReturnResult(sqlmock.NewResult(1, 1))
				mock.ExpectCommit()
			}
			res, err := ReserveBudget(context.Background(), db, tt.req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReserveBudget err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && (res.RequestedTokens != tt.wantTokens || res.Site != "s" || res.Model != "m" || res.Note != "n") {
				t.Fatalf("reservation = %+v", res)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
