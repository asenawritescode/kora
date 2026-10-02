package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	aiStore "github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresAIApprovalListBootstrap(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test AI run storage against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_ai_runs_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	dialect := db.Resolve("postgres")
	if err := aiStore.EnsureAIRunTables(t.Context(), database, dialect); err != nil {
		t.Fatal("bootstrap PostgreSQL AI run tables:", err)
	}
	if err := aiStore.EnsureAIRunTables(t.Context(), database, dialect); err != nil {
		t.Fatal("repeat PostgreSQL AI run table bootstrap:", err)
	}
	requestedAt := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := database.Exec(`INSERT INTO _kora_ai_approval (id, site, operation_id, actor_principal_id, actor_principal_type, tool_name, state, target_fingerprint, argument_hash, record_version, requested_at, granted_by, auth_session_id) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		"approval-pg", "site-pg", "operation-pg", "operator-pg", "user", "update_record", "pending_approval", "target-hash", "argument-hash", 1, requestedAt, "", "session-pg"); err != nil {
		t.Fatal("insert PostgreSQL AI approval fixture:", err)
	}

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/ai/approvals", nil)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", "site-pg")
	ctx.Set("site_db_type", "postgres")
	handler.HandleAIListApprovals(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL AI approval list returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Data []aiApprovalListItem `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal("decode PostgreSQL AI approval list:", err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "approval-pg" || response.Data[0].OperationID != "operation-pg" {
		t.Fatalf("PostgreSQL AI approval list = %#v, want seeded approval", response.Data)
	}

	storeCtx := aiStore.WithDialect(t.Context(), dialect)
	conversation := aiStore.ConversationRecord{ID: "conversation-pg", Site: "site-pg", Channel: "chat", SubjectKey: "subject-pg", Title: "PG lifecycle", Status: "active", LastMessageAt: requestedAt}
	if err := aiStore.UpsertConversation(storeCtx, database, conversation); err != nil {
		t.Fatal("create PostgreSQL AI conversation:", err)
	}
	if got, err := aiStore.LoadConversation(storeCtx, database, "site-pg", "subject-pg"); err != nil || got.ID != conversation.ID {
		t.Fatalf("load PostgreSQL AI conversation = %#v, %v", got, err)
	}
	run := aiStore.RunRecord{ID: "run-pg", Site: "site-pg", ConversationID: conversation.ID, Status: "planning", InputMessage: "hello"}
	if err := aiStore.UpsertRun(storeCtx, database, run); err != nil {
		t.Fatal("create PostgreSQL AI run:", err)
	}
	if got, err := aiStore.LoadRun(storeCtx, database, run.ID); err != nil || got.InputMessage != run.InputMessage {
		t.Fatalf("load PostgreSQL AI run = %#v, %v", got, err)
	}
	if err := aiStore.AppendMessage(storeCtx, database, "site-pg", conversation.ID, run.ID, "user", "hello", "message", "", 1); err != nil {
		t.Fatal("append PostgreSQL AI message:", err)
	}
	if err := aiStore.UpsertStep(storeCtx, database, "step-pg", "site-pg", run.ID, conversation.ID, "plan", "planning", "started", "", "{}", "{}", ""); err != nil {
		t.Fatal("upsert PostgreSQL AI step:", err)
	}
	if err := aiStore.UpsertTask(storeCtx, database, aiStore.TaskRecord{ID: "task-pg", Site: "site-pg", RunID: run.ID, ConversationID: conversation.ID, Title: "Task", Status: "queued"}); err != nil {
		t.Fatal("upsert PostgreSQL AI task:", err)
	}
	if tasks, err := aiStore.ListRunTasks(storeCtx, database, run.ID); err != nil || len(tasks) != 1 || tasks[0].ID != "task-pg" {
		t.Fatalf("list PostgreSQL AI tasks = %#v, %v", tasks, err)
	}
	if err := aiStore.MarkTaskStatus(storeCtx, database, "task-pg", "completed", "done"); err != nil {
		t.Fatal("update PostgreSQL AI task:", err)
	}
	if err := aiStore.UpdateStepStatus(storeCtx, database, "step-pg", "completed", "done", "", "", "", ""); err != nil {
		t.Fatal("update PostgreSQL AI step:", err)
	}
	if _, err := aiStore.ListRunSteps(storeCtx, database, run.ID); err != nil {
		t.Fatal("list PostgreSQL AI steps:", err)
	}
	if err := aiStore.RecordUsage(storeCtx, database, contract.UsageEvent{ID: "usage-pg", Site: "site-pg", Model: "model-pg", Provider: "provider-pg", RunID: run.ID, Attempt: 1, Status: "completed"}); err != nil {
		t.Fatal("record PostgreSQL AI usage:", err)
	}
	if err := aiStore.RecordAudit(storeCtx, database, aiStore.AuditEvent{ID: "audit-pg", Site: "site-pg", RunID: run.ID, Kind: "tool_call", Status: "completed"}); err != nil {
		t.Fatal("record PostgreSQL AI audit:", err)
	}
	reservation, err := aiStore.ReserveBudget(storeCtx, database, "site-pg", "model-pg", 10, 100, "integration test")
	if err != nil {
		t.Fatal("reserve PostgreSQL AI budget:", err)
	}
	if err := aiStore.FinalizeBudget(storeCtx, database, reservation, 8); err != nil {
		t.Fatal("finalize PostgreSQL AI budget:", err)
	}
	if err := aiStore.ReleaseBudget(storeCtx, database, reservation); err != nil {
		t.Fatal("release PostgreSQL AI budget:", err)
	}
	if err := aiStore.CancelRun(storeCtx, database, run.ID, "integration cancellation"); err != nil {
		t.Fatal("cancel PostgreSQL AI run:", err)
	}
	if _, err := aiStore.ResumeRun(storeCtx, database, run.ID, ""); err != nil {
		t.Fatal("resume PostgreSQL AI run:", err)
	}
	if _, err := database.Exec(`UPDATE _kora_ai_conversation SET retention_expires_at = $1 WHERE id = $2`, requestedAt.Add(-time.Hour), conversation.ID); err != nil {
		t.Fatal("expire PostgreSQL AI conversation fixture:", err)
	}
	if _, err := aiStore.CleanupExpired(storeCtx, database, requestedAt); err != nil {
		t.Fatal("cleanup PostgreSQL AI retention:", err)
	}
}
