package ai

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLiveLibSQLAIRunStoreLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_AI_LIVE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_AI_LIVE_LIBSQL_DSN to a fresh disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL database:", err)
	}
	var existingTables int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&existingTables); err != nil {
		t.Fatal("check disposable LibSQL database is empty:", err)
	}
	if existingTables != 0 {
		t.Fatalf("refusing to use non-empty LibSQL database (%d tables found)", existingTables)
	}
	dialect := kdb.Resolve("libsql")
	ctx := WithDialect(t.Context(), dialect)
	if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
		t.Fatal("bootstrap LibSQL AI run tables:", err)
	}
	if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
		t.Fatal("repeat LibSQL AI run table bootstrap:", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	conversation := ConversationRecord{ID: "conversation-libsql", Site: "site-libsql", Channel: "chat", SubjectKey: "subject-libsql", Title: "Initial", Status: "active", LastMessageAt: now}
	if err := UpsertConversation(ctx, database, conversation); err != nil {
		t.Fatal("insert LibSQL AI conversation:", err)
	}
	conversation.Title = "Updated"
	if err := UpsertConversation(ctx, database, conversation); err != nil {
		t.Fatal("upsert LibSQL AI conversation:", err)
	}
	if got, err := LoadConversation(ctx, database, conversation.Site, conversation.SubjectKey); err != nil || got.Title != "Updated" {
		t.Fatalf("load LibSQL AI conversation = %#v, %v", got, err)
	}
	run := RunRecord{ID: "run-libsql", Site: conversation.Site, ConversationID: conversation.ID, Status: "planning", InputMessage: "hello"}
	if err := UpsertRun(ctx, database, run); err != nil {
		t.Fatal("insert LibSQL AI run:", err)
	}
	run.OutputMessage = "world"
	if err := UpsertRun(ctx, database, run); err != nil {
		t.Fatal("upsert LibSQL AI run:", err)
	}
	if got, err := LoadRun(ctx, database, run.ID); err != nil || got.OutputMessage != "world" {
		t.Fatalf("load LibSQL AI run = %#v, %v", got, err)
	}
	if err := AppendMessage(ctx, database, conversation.Site, conversation.ID, run.ID, "user", "hello", "message", "", 1); err != nil {
		t.Fatal("append LibSQL AI message:", err)
	}
	if err := UpsertStep(ctx, database, "step-libsql", conversation.Site, run.ID, conversation.ID, "plan", "planning", "start", "", "{}", "{}", ""); err != nil {
		t.Fatal("upsert LibSQL AI step:", err)
	}
	if err := UpsertTask(ctx, database, TaskRecord{ID: "task-libsql", Site: conversation.Site, RunID: run.ID, ConversationID: conversation.ID, Title: "Task", Status: "queued"}); err != nil {
		t.Fatal("upsert LibSQL AI task:", err)
	}
	if err := RecordUsage(ctx, database, contract.UsageEvent{ID: "usage-libsql", Site: conversation.Site, Model: "model", Provider: "provider", RunID: run.ID, Attempt: 1, Status: "completed"}); err != nil {
		t.Fatal("record LibSQL AI usage:", err)
	}
	if err := RecordAudit(ctx, database, AuditEvent{ID: "audit-libsql", Site: conversation.Site, RunID: run.ID, Kind: "tool_call", Status: "completed"}); err != nil {
		t.Fatal("record LibSQL AI audit:", err)
	}
	reservation, err := ReserveBudget(ctx, database, conversation.Site, "model", 10, 100, "integration test")
	if err != nil {
		t.Fatal("reserve LibSQL AI budget:", err)
	}
	if err := FinalizeBudget(ctx, database, reservation, 8); err != nil {
		t.Fatal("finalize LibSQL AI budget:", err)
	}
	if err := ReleaseBudget(ctx, database, reservation); err != nil {
		t.Fatal("release LibSQL AI budget:", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM _kora_ai_conversation WHERE id = ?`, conversation.ID); err != nil {
		t.Fatal("delete LibSQL AI conversation:", err)
	}
}
