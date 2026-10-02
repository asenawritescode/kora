//go:build integration

package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/script"
)

func TestAIRecordMutationUsesKernelReceiptAndAudit(t *testing.T) {
	dsn := os.Getenv("KORA_AI_MUTATION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_AI_MUTATION_MYSQL_DSN to a MySQL account allowed to create disposable databases")
	}
	database, dialect := newAIKernelMySQLDatabase(t, dsn)
	registry := doctype.NewRegistry()
	task := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Label: "Title", Reqd: true}}}
	registry.Register(task)
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: task.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true})
	for _, statement := range dialect.CreateTable(task) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create Task table: %v", err)
		}
	}
	ctx := WithDialect(t.Context(), dialect)
	if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
		t.Fatalf("create AI approval/audit tables: %v", err)
	}
	for _, statement := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create lifecycle script tables: %v", err)
		}
	}
	scriptStore := &script.Store{DB: database, Dialect: dialect}
	hookRunner := &aiMutationHookRunner{}

	site := "ai-kernel-integration"
	for name, event := range map[string]script.Event{
		"task-before-insert": script.EventBeforeInsert,
		"task-before-save":   script.EventBeforeSave,
		"task-after-insert":  script.EventAfterInsert,
		"task-after-save":    script.EventAfterSave,
	} {
		if err := scriptStore.Insert(script.ScriptRecord{
			Name: name, Site: site, ScriptType: script.TypeDocEvent,
			DocType: "Task", Event: event, IsActive: true, Script: "return document",
		}); err != nil {
			t.Fatalf("insert lifecycle hook %q: %v", name, err)
		}
	}
	tx := &orm.TxManager{
		DB: database, Registry: registry, Dialect: dialect, SiteName: site,
		Context: context.WithValue(ctx, "session_created_at", time.Now().UTC()), CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
		CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect),
		ScriptStore: scriptStore, ScriptRunner: hookRunner,
	}
	callTool := func(callID, toolName string, args map[string]any, runID, stepID string) string {
		t.Helper()
		argBytes, err := json.Marshal(args)
		if err != nil {
			t.Fatal("encode AI tool arguments:", err)
		}
		calls := []any{map[string]any{"id": callID, "function": map[string]any{"name": toolName, "arguments": string(argBytes)}}}
		results := executeToolCallsForAI(tx.Context, tx, registry, calls, "alice@example.test", site, runID, stepID, "conversation-1", []string{doctype.AdminRole})
		if len(results) != 1 {
			t.Fatalf("tool %s returned %d responses, want one", toolName, len(results))
		}
		return fmt.Sprint(results[0]["content"])
	}
	createArgs := map[string]any{"title": "Initial"}
	if pending := callTool("call-create-1", "task_create", createArgs, "run-1", "step-1"); !strings.Contains(pending, "Approval required") {
		t.Fatalf("first create should require approval, got %q", pending)
	}
	if _, err := GrantApprovalForOperation(tx.Context, database, site, "run-1", "task_create", createArgs, "alice@example.test"); err != nil {
		t.Fatalf("grant create approval: %v", err)
	}
	created := callTool("call-create-1", "task_create", createArgs, "run-1", "step-1")
	replayedCreate := callTool("call-create-1", "task_create", createArgs, "run-1", "step-1")
	if !strings.HasPrefix(created, "Created Task \"") || replayedCreate != created {
		t.Fatalf("AI create/replay outputs differ: first=%q replay=%q", created, replayedCreate)
	}
	nameStart := strings.Index(created, "\"") + 1
	nameEnd := strings.Index(created[nameStart:], "\"") + nameStart
	if nameStart == 0 || nameEnd <= nameStart {
		t.Fatalf("could not parse generated Task name from %q", created)
	}
	name := created[nameStart:nameEnd]

	updateArgs := map[string]any{"name": name, "title": "Updated"}
	if pending := callTool("call-update-1", "task_update", updateArgs, "run-1", "step-2"); !strings.Contains(pending, "Approval required") {
		t.Fatalf("first update should require approval, got %q", pending)
	}
	if _, err := GrantApprovalForOperation(tx.Context, database, site, "run-1", "task_update", updateArgs, "alice@example.test"); err != nil {
		t.Fatalf("grant update approval: %v", err)
	}
	updated := callTool("call-update-1", "task_update", updateArgs, "run-1", "step-2")
	replayedUpdate := callTool("call-update-1", "task_update", updateArgs, "run-1", "step-2")
	if !strings.HasPrefix(updated, "Updated Task \"") || replayedUpdate != updated {
		t.Fatalf("AI update/replay outputs differ: first=%q replay=%q", updated, replayedUpdate)
	}

	channelArgs := map[string]any{"title": "Channel"}
	const channelKey = "external-channel-retry-0001"
	channelCreate := ExecuteConfirmedToolWithIdempotencyKey(tx, registry, "task_create", channelArgs, "alice@example.test", site, channelKey)
	channelReplay := ExecuteConfirmedToolWithIdempotencyKey(tx, registry, "task_create", channelArgs, "alice@example.test", site, channelKey)
	if !strings.HasPrefix(channelCreate, "Created Task \"") || channelReplay != channelCreate {
		t.Fatalf("channel create/replay outputs differ: first=%q replay=%q", channelCreate, channelReplay)
	}

	var title string
	if err := database.QueryRow("SELECT title FROM `"+task.RawTableName()+"` WHERE name = ?", name).Scan(&title); err != nil {
		t.Fatalf("read AI-mutated Task: %v", err)
	}
	if title != "Updated" {
		t.Fatalf("Task title = %q, want Updated", title)
	}
	for event, want := range map[script.Event]int{
		script.EventBeforeInsert: 2,
		script.EventBeforeSave:   3,
		script.EventAfterInsert:  2,
		script.EventAfterSave:    3,
	} {
		if got := hookRunner.count(event); got != want {
			t.Fatalf("AI lifecycle event %q ran %d times, want %d (tool-call replays must not rerun hooks)", event, got, want)
		}
	}
	for table, want := range map[string]int{"_kora_operation_audit": 3, "_kora_outbox": 3, "_kora_idempotency_receipt": 3} {
		var count int
		query := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(table) + " WHERE site = ?"
		if err := database.QueryRow(query, site).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d (replays must have no extra effects)", table, count, want)
		}
	}
	var source, actor string
	if err := database.QueryRow("SELECT source, actor_user FROM _kora_operation_audit WHERE site = ? ORDER BY created_at LIMIT 1", site).Scan(&source, &actor); err != nil {
		t.Fatalf("read AI audit attribution: %v", err)
	}
	if source != string(kernel.SourceAI) || actor != "alice@example.test" {
		t.Fatalf("AI audit attribution source=%q actor=%q", source, actor)
	}
	if _, err := executeAIRecordMutationWithKey(tx, registry, kernel.CommandRecordCreate, task.Name, "", map[string]any{"title": "Denied"}, "guest@example.test", []string{"Guest"}, "ai:run-1:step-3:call-denied", false); err == nil {
		t.Fatal("AI create without create permission succeeded")
	}
	var total int
	if err := database.QueryRow("SELECT COUNT(*) FROM `" + task.RawTableName() + "`").Scan(&total); err != nil {
		t.Fatal("count Tasks after denied create:", err)
	}
	if total != 2 {
		t.Fatalf("denied create left %d Tasks, want 2", total)
	}
}

type aiMutationHookRunner struct {
	events []script.Event
}

func (r *aiMutationHookRunner) Execute(_ context.Context, req script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.events = append(r.events, req.Event)
	return &script.ExecuteResult{}, nil
}

func (r *aiMutationHookRunner) Validate(string) error { return nil }
func (r *aiMutationHookRunner) Close() error          { return nil }

func (r *aiMutationHookRunner) count(event script.Event) int {
	count := 0
	for _, got := range r.events {
		if got == event {
			count++
		}
	}
	return count
}

func newAIKernelMySQLDatabase(t *testing.T, dsn string) (*sql.DB, kdb.Dialect) {
	t.Helper()
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open MySQL admin connection:", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Fatal("ping MySQL admin connection:", err)
	}
	name := fmt.Sprintf("kora_ai_kernel_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		admin.Close()
		t.Fatal("create disposable MySQL database:", err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		t.Fatal("parse MySQL DSN:", err)
	}
	cfg.DBName = name
	siteDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		admin.Exec("DROP DATABASE `" + name + "`")
		admin.Close()
		t.Fatal("open disposable MySQL database:", err)
	}
	dialect := kdb.Resolve("mysql")
	for _, statements := range [][]string{dialect.SystemTableSQL(), kdb.KernelTablesMySQL(), kdb.OutboxTablesMySQL()} {
		for _, statement := range statements {
			trimmed := strings.TrimSpace(statement)
			if strings.HasPrefix(trimmed, "ALTER TABLE") || strings.HasPrefix(trimmed, "UPDATE ") || strings.HasPrefix(trimmed, "CREATE INDEX") {
				continue
			}
			if _, err := siteDB.Exec(statement); err != nil {
				siteDB.Close()
				admin.Exec("DROP DATABASE `" + name + "`")
				admin.Close()
				t.Fatalf("create kernel support tables: %v", err)
			}
		}
	}
	t.Cleanup(func() {
		_ = siteDB.Close()
		_, _ = admin.Exec("DROP DATABASE `" + name + "`")
		_ = admin.Close()
	})
	return siteDB, dialect
}
