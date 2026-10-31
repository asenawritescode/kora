//go:build integration

package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
	workflowstore "github.com/asenawritescode/kora/workflow/sqlstore"
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
	for _, statement := range configstore.ConfigRevisionSchema(dialect) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dialect); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}
	scriptStore := &script.Store{DB: database, Dialect: dialect}
	hookRunner := &aiMutationHookRunner{}

	site := "ai-kernel-integration"
	execution := testAIExecution(t, site, "alice@example.test", []string{doctype.AdminRole})
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
		CurrentUserRoles: []string{doctype.AdminRole}, Execution: execution, Outbox: outbox.NewSQLWriter(dialect),
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
	updated := callTool("call-update-1", "task_update", updateArgs, "run-1", "step-2")
	replayedUpdate := callTool("call-update-1", "task_update", updateArgs, "run-1", "step-2")
	if !strings.HasPrefix(updated, "Updated Task \"") || replayedUpdate != updated {
		t.Fatalf("AI update/replay outputs differ: first=%q replay=%q", updated, replayedUpdate)
	}
	var pendingApprovals int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_ai_approval WHERE site = ? AND tool_name = 'task_update'", site).Scan(&pendingApprovals); err != nil {
		t.Fatalf("count AI update approvals: %v", err)
	}
	if pendingApprovals != 0 {
		t.Fatalf("ordinary AI record update created %d unnecessary approval records", pendingApprovals)
	}

	channelArgs := map[string]any{"title": "Channel"}
	const channelKey = "external-channel-retry-0001"
	channelCreate := ExecuteConfirmedToolWithExecution(tx, registry, "task_create", channelArgs, "alice@example.test", site, channelKey, execution)
	channelReplay := ExecuteConfirmedToolWithExecution(tx, registry, "task_create", channelArgs, "alice@example.test", site, channelKey, execution)
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
	guestExecution := testAIExecution(t, site, "guest@example.test", []string{"Guest"})
	if _, err := executeAIRecordMutationWithExecution(tx, registry, kernel.CommandRecordCreate, task.Name, "", map[string]any{"title": "Denied"}, "guest@example.test", []string{"Guest"}, "ai:run-1:step-3:call-denied", false, guestExecution); err == nil {
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

func TestAIMutationBundleConvertsLeadWithDurableApprovalAtomically(t *testing.T) {
	dsn := os.Getenv("KORA_AI_MUTATION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_AI_MUTATION_MYSQL_DSN to a MySQL account allowed to create disposable databases")
	}
	database, dialect := newAIKernelMySQLDatabase(t, dsn)
	site := "ai-crm-conversion-integration"
	packDir := filepath.Join("..", "..", "config", "v0", "crm", "doctypes")
	parseDocType := func(filename string) *doctype.DocType {
		t.Helper()
		content, err := os.ReadFile(filepath.Join(packDir, filename))
		if err != nil {
			t.Fatalf("read CRM %s: %v", filename, err)
		}
		parsed, err := doctype.ParseYAML(content, filename)
		if err != nil {
			t.Fatalf("parse CRM %s: %v", filename, err)
		}
		return parsed
	}
	company := parseDocType("company.yaml")
	contact := parseDocType("contact.yaml")
	deal := parseDocType("deal.yaml")
	dealItem := parseDocType("deal_item.yaml")
	lead := parseDocType("lead.yaml")
	registry := doctype.NewRegistry()
	for _, dt := range []*doctype.DocType{company, contact, deal, dealItem, lead} {
		registry.Register(dt)
		registry.Permissions.SetPermission(&doctype.Permission{Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Submit: true})
	}
	var databaseName string
	if err := database.QueryRow("SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatalf("read disposable database name: %v", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatalf("migrate CRM conversion tables: %v", err)
	}
	workflowContent, err := os.ReadFile(filepath.Join("..", "..", "config", "v0", "crm", "workflows", "lead.json"))
	if err != nil {
		t.Fatalf("read CRM Lead Engine v2 workflow: %v", err)
	}
	var definition contract.WorkflowDefinition
	if err := json.Unmarshal(workflowContent, &definition); err != nil {
		t.Fatalf("parse CRM Lead Engine v2 workflow: %v", err)
	}

	ctx := WithDialect(t.Context(), dialect)
	if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
		t.Fatalf("create AI approval tables: %v", err)
	}
	for _, statement := range configstore.ConfigRevisionSchema(dialect) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dialect); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}
	definitions, err := workflowstore.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatalf("create workflow definition store: %v", err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatalf("create workflow definition tables: %v", err)
	}
	tenant, err := contract.ParseTenantID(site)
	if err != nil {
		t.Fatalf("parse workflow tenant: %v", err)
	}
	actor, err := contract.ParsePrincipalID("system-test-migration")
	if err != nil {
		t.Fatalf("parse workflow migration principal: %v", err)
	}
	if err := definitions.PublishImportedBatch(ctx, tenant, actor, []contract.WorkflowDefinition{definition}, time.Now().UTC()); err != nil {
		t.Fatalf("publish CRM Lead lifecycle definition: %v", err)
	}
	execution := testAIExecution(t, site, "sales@example.test", []string{doctype.AdminRole})
	tx := &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, SiteName: site, Context: context.WithValue(ctx, "session_created_at", time.Now().UTC()), Execution: execution, Outbox: outbox.NewSQLWriter(dialect), WorkflowStatusPolicies: definitions}
	createdLead, err := executeAIRecordMutationWithExecution(tx, registry, kernel.CommandRecordCreate, lead.Name, "", map[string]any{"lead_name": "Northwind expansion"}, "sales@example.test", nil, "initial-lead", false, execution)
	if err != nil {
		t.Fatalf("create Lead fixture: %v", err)
	}
	// Seed this integration's precondition through SQL: conversion begins with
	// an already-qualified Lead, whose prior qualifying path is outside this
	// test's conversion transaction.
	if _, err := database.Exec("UPDATE "+dialect.QuoteIdent(lead.RawTableName())+" SET lifecycle_stage = ? WHERE name = ?", "Qualified", createdLead.Name); err != nil {
		t.Fatalf("seed qualified Lead state: %v", err)
	}
	storedLead, err := tx.GetDoc(lead, createdLead.Name, "")
	if err != nil {
		t.Fatalf("read qualified Lead revision: %v", err)
	}
	args := map[string]any{"records": []any{
		map[string]any{"key": "company", "doctype": "Company", "data": map[string]any{"company_name": "Northwind"}},
		map[string]any{"key": "contact", "doctype": "Contact", "data": map[string]any{"first_name": "Amina", "last_name": "Njeri", "company": "$records.company.name"}},
		map[string]any{"key": "deal", "doctype": "Deal", "data": map[string]any{"deal_name": "Northwind expansion", "company": "$records.company.name", "contact": "$records.contact.name", "items": []any{map[string]any{"product_name": "Annual subscription", "quantity": 1, "unit_price": 12000.0}}}},
		map[string]any{"key": "lead", "doctype": "Lead", "operation": "update", "name": createdLead.Name, "expected_revision": float64(storedLead.Revision), "transition_action": "Convert", "data": map[string]any{"company": "$records.company.name", "contact": "$records.contact.name", "converted_deal": "$records.deal.name"}},
	}}
	const runID = "crm-conversion-run"
	if err := EnsureApprovalPending(ctx, database, site, runID, "sales@example.test", "agent", "mutate_records", 0, args); err != nil {
		t.Fatalf("persist conversion approval: %v", err)
	}
	if _, err := GrantApprovalForOperation(ctx, database, site, runID, "mutate_records", args, "sales@example.test"); err != nil {
		t.Fatalf("grant conversion approval: %v", err)
	}
	const bundleKey = "crm-conversion-bundle-once"
	first := executeAIMutationBundle(tx, registry, args, "sales@example.test", site, runID, "", bundleKey, execution)
	replay := executeAIMutationBundle(tx, registry, args, "sales@example.test", site, runID, "", bundleKey, execution)
	if !strings.Contains(first, "Applied the approved atomic operation") || replay != first {
		t.Fatalf("conversion execution/replay = %q / %q", first, replay)
	}
	convertedLead, err := tx.GetDoc(lead, createdLead.Name, "")
	if err != nil {
		t.Fatalf("read converted Lead: %v", err)
	}
	if convertedLead.Get("lifecycle_stage") != "Converted" || convertedLead.Get("company") == nil || convertedLead.Get("contact") == nil || convertedLead.Get("converted_deal") == nil {
		t.Fatalf("Lead did not retain the conversion links and configured state: %#v", convertedLead.Fields)
	}
	for _, dt := range []*doctype.DocType{company, contact, deal} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + dialect.QuoteIdent(dt.RawTableName())).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", dt.Name, err)
		}
		if count != 1 {
			t.Fatalf("%s rows = %d after replay, want 1", dt.Name, count)
		}
	}
	var itemCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM " + dialect.QuoteIdent(deal.RawChildTableName("items"))).Scan(&itemCount); err != nil {
		t.Fatalf("count Deal line items: %v", err)
	}
	if itemCount != 1 {
		t.Fatalf("Deal line items = %d after replay, want 1", itemCount)
	}
	var auditCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_operation_audit WHERE site = ?", site).Scan(&auditCount); err != nil {
		t.Fatalf("count committed operation audit rows: %v", err)
	}
	if auditCount != 5 {
		t.Fatalf("operation audit rows = %d, want initial Lead plus four atomic conversion records", auditCount)
	}
}

func TestAIRecordMutationUsesExactVerifiedGrantAndPrincipal(t *testing.T) {
	dsn := os.Getenv("KORA_AI_MUTATION_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_AI_MUTATION_MYSQL_DSN to a MySQL account allowed to create disposable databases")
	}
	database, dialect := newAIKernelMySQLDatabase(t, dsn)
	site := "ai-exact-grant-" + fmt.Sprintf("%x", time.Now().UnixNano())
	dt := &doctype.DocType{Name: "AI Grant Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
	registry := doctype.NewRegistry()
	registry.Register(dt)
	for _, statement := range dialect.CreateTable(dt) {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create exact-grant DocType table: %v", err)
		}
	}
	defer func() {
		_, _ = database.Exec("DROP TABLE " + dialect.QuoteIdent(dt.RawTableName()))
		_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_naming_series WHERE doctype = ?"), dt.Name)
		for _, table := range []string{"_kora_operation_audit", "_kora_idempotency_receipt", "_kora_outbox"} {
			_, _ = database.Exec("DELETE FROM "+dialect.QuoteIdent(table)+" WHERE site = ?", site)
		}
	}()
	execution, err := policy.NewVerifiedExecutionWithGrants(contract.TenantID(site), contract.ActorContext{
		PrincipalID: "service:ai-writer", PrincipalType: contract.PrincipalService, Site: site,
	}, []policy.Grant{{Resource: dt.Name, Operation: "create"}})
	if err != nil {
		t.Fatal("create verified exact-grant execution:", err)
	}
	tx := &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, SiteName: site, Outbox: outbox.NewSQLWriter(dialect)}
	const key = "ai-exact-grant-create-1"
	first, err := executeAIRecordMutationWithExecution(tx, registry, kernel.CommandRecordCreate, dt.Name, "", map[string]any{"title": "verified service create"}, "forged-owner", []string{doctype.AdminRole}, key, false, execution)
	if err != nil {
		t.Fatal("AI create with exact verified grant:", err)
	}
	replay, err := executeAIRecordMutationWithExecution(tx, registry, kernel.CommandRecordCreate, dt.Name, "", map[string]any{"title": "verified service create"}, "forged-owner", []string{doctype.AdminRole}, key, false, execution)
	if err != nil {
		t.Fatal("AI exact-grant replay:", err)
	}
	if first.Name == "" || replay.Name != first.Name {
		t.Fatalf("AI exact-grant create/replay names = %q/%q, want same non-empty name", first.Name, replay.Name)
	}
	var owner string
	if err := database.QueryRow("SELECT owner FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name = ?", first.Name).Scan(&owner); err != nil {
		t.Fatal("read persisted owner:", err)
	}
	if owner != "service:ai-writer" {
		t.Fatalf("AI adapter used caller owner %q, want verified principal owner", owner)
	}
	for table, want := range map[string]int{"_kora_operation_audit": 1, "_kora_idempotency_receipt": 1, "_kora_outbox": 1} {
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM "+dialect.QuoteIdent(table)+" WHERE site = ?", site).Scan(&count); err != nil {
			t.Fatalf("count %s rows: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d after AI replay", table, count, want)
		}
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
