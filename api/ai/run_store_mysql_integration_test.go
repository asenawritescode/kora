package ai

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
)

func TestLiveMySQLAIRunStoreLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_AI_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_AI_LIVE_MYSQL_DSN to a disposable MySQL database")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable MySQL database:", err)
	}
	var existingTables int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()`).Scan(&existingTables); err != nil {
		t.Fatal("check disposable MySQL database is empty:", err)
	}
	if existingTables != 0 {
		t.Fatalf("refusing to use non-empty MySQL database (%d tables found)", existingTables)
	}
	ctx := WithDialect(t.Context(), kdb.Resolve("mysql"))
	if err := EnsureAIRunTables(ctx, database, kdb.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap MySQL AI run tables:", err)
	}
	if err := EnsureAIRunTables(ctx, database, kdb.Resolve("mysql")); err != nil {
		t.Fatal("repeat MySQL AI run table bootstrap:", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	conversation := ConversationRecord{ID: "conversation-mysql", Site: "site-mysql", Channel: "chat", SubjectKey: "subject-mysql", Title: "Initial", Status: "active", LastMessageAt: now}
	if err := UpsertConversation(ctx, database, conversation); err != nil {
		t.Fatal("insert MySQL AI conversation:", err)
	}
	conversation.Title = "Updated"
	if err := UpsertConversation(ctx, database, conversation); err != nil {
		t.Fatal("upsert MySQL AI conversation:", err)
	}
	if got, err := LoadConversation(ctx, database, conversation.Site, conversation.SubjectKey); err != nil || got.Title != "Updated" {
		t.Fatalf("load MySQL AI conversation = %#v, %v", got, err)
	}
	run := RunRecord{ID: "run-mysql", Site: conversation.Site, ConversationID: conversation.ID, Status: "planning", InputMessage: "hello"}
	if err := UpsertRun(ctx, database, run); err != nil {
		t.Fatal("insert MySQL AI run:", err)
	}
	run.OutputMessage = "world"
	if err := UpsertRun(ctx, database, run); err != nil {
		t.Fatal("upsert MySQL AI run:", err)
	}
	if got, err := LoadRun(ctx, database, run.ID); err != nil || got.OutputMessage != "world" {
		t.Fatalf("load MySQL AI run = %#v, %v", got, err)
	}
	if err := AppendMessage(ctx, database, conversation.Site, conversation.ID, run.ID, "user", "hello", "message", "", 1); err != nil {
		t.Fatal("append MySQL AI message:", err)
	}
	if err := UpsertStep(ctx, database, "step-mysql", conversation.Site, run.ID, conversation.ID, "plan", "planning", "start", "", "{}", "{}", ""); err != nil {
		t.Fatal("upsert MySQL AI step:", err)
	}
	if err := UpsertTask(ctx, database, TaskRecord{ID: "task-mysql", Site: conversation.Site, RunID: run.ID, ConversationID: conversation.ID, Title: "Task", Status: "queued"}); err != nil {
		t.Fatal("upsert MySQL AI task:", err)
	}
	if err := RecordUsage(ctx, database, contract.UsageEvent{ID: "usage-mysql", Site: conversation.Site, Model: "model", Provider: "provider", RunID: run.ID, Attempt: 1, Status: "completed"}); err != nil {
		t.Fatal("record MySQL AI usage:", err)
	}
	if err := RecordAudit(ctx, database, AuditEvent{ID: "audit-mysql", Site: conversation.Site, RunID: run.ID, Kind: "tool_call", Status: "completed"}); err != nil {
		t.Fatal("record MySQL AI audit:", err)
	}
	reservation, err := ReserveBudget(ctx, database, conversation.Site, "model", 10, 100, "integration test")
	if err != nil {
		t.Fatal("reserve MySQL AI budget:", err)
	}
	if err := FinalizeBudget(ctx, database, reservation, 8); err != nil {
		t.Fatal("finalize MySQL AI budget:", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM _kora_ai_conversation WHERE id = ?`, conversation.ID); err != nil {
		t.Fatal("delete MySQL AI conversation:", err)
	}
}
