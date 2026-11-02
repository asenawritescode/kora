//go:build integration

package engine

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/script"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

type integrationMutationLifecycleRunner struct {
	failAfterMutation bool
	calls             int
	mutationErr       error
	targetDoctype     string
}

func (r *integrationMutationLifecycleRunner) Execute(_ context.Context, request script.ExecuteRequest) (*script.ExecuteResult, error) {
	r.calls++
	if request.Provider == nil {
		return nil, errors.New("provider is required for local mutation test")
	}
	doctypeName := r.targetDoctype
	if doctypeName == "" {
		doctypeName = "Task"
	}
	if _, err := request.Provider.CreateDoc(doctypeName, map[string]any{"title": "created-by-hook"}, "ignored-owner", "ignored-actor"); err != nil {
		r.mutationErr = err
		return nil, err
	}
	if r.failAfterMutation {
		return nil, errors.New("simulated script failure after local mutation")
	}
	return &script.ExecuteResult{}, nil
}
func (*integrationMutationLifecycleRunner) Validate(string) error { return nil }
func (*integrationMutationLifecycleRunner) Close() error          { return nil }

// TestDurableLifecycleHookWorkerCommitsAuthorizedLocalMutationAcrossDialects
// uses fresh disposable databases and the real Engine script provider. A
// failed script must roll back its nested record and outbox event; successful
// retry commits the record, event, and fenced invocation receipt together.
func TestDurableLifecycleHookWorkerCommitsAuthorizedLocalMutationAcrossDialects(t *testing.T) {
	cases := []struct{ name, driver, env string }{
		{"mysql", "mysql", "KORA_ENGINE_HOOK_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_ENGINE_HOOK_DISPOSABLE_POSTGRES_DSN"},
		{"libsql-http", "libsql", "KORA_ENGINE_HOOK_DISPOSABLE_LIBSQL_DSN"},
	}
	configured := false
	for _, tc := range cases {
		configured = configured || os.Getenv(tc.env) != ""
	}
	if !configured {
		t.Skip("set KORA_ENGINE_HOOK_DISPOSABLE_{MYSQL,POSTGRES,LIBSQL}_DSN to fresh disposable databases")
	}
	for _, tc := range cases {
		dsn := os.Getenv(tc.env)
		if dsn == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("open disposable tenant database:", err)
			}
			database.SetMaxOpenConns(8)
			t.Cleanup(func() { _ = database.Close() })
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping disposable tenant database:", err)
			}
			dialect := kdb.Resolve(tc.driver)
			for _, statement := range durableLifecycleFixtureDDL(tc.driver) {
				if _, err := database.ExecContext(ctx, statement); err != nil {
					t.Fatalf("create durable lifecycle fixture: %v\n%s", err, statement)
				}
			}

			tenant := contract.TenantID("hook-engine-" + contract.NewID())
			siteName := string(tenant)
			registry := doctype.NewRegistry()
			dt := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
			protected := &doctype.DocType{Name: "ProtectedTask", ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeTenantShared, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
			registry.LoadFull([]*doctype.DocType{dt, protected}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{
				{Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true},
				{Doctype: protected.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true},
			})
			if err := schema.MigrateSiteFromRegistry(database, "durable_hook_integration", registry, dialect); err != nil {
				t.Fatal("migrate disposable Task doctype:", err)
			}
			store := &script.Store{DB: database, Dialect: dialect}
			if err := store.EnsureAsyncHookInvocationSchema(ctx); err != nil {
				t.Fatal("ensure lifecycle invocation table:", err)
			}
			const handlerName = "module-task-after-save"
			if _, err := database.ExecContext(ctx, dbRebindForHook(dialect, `INSERT INTO _kora_script (name, site, script_type, doctype, event, priority, is_active, timeout_ms, script) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				handlerName, siteName, string(script.TypeDocEvent), "Task", string(script.EventAfterSave), 10, true, 5000, "return true"); err != nil {
				t.Fatal("insert module post-commit handler:", err)
			}
			source := contract.OperationID(contract.NewOperationID())
			actor := contract.ActorContext{PrincipalID: "hook-user", SubjectUserID: "hook-user", PrincipalType: contract.PrincipalHuman,
				Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}
			grants := []policy.Grant{{Resource: "Task", Operation: "create"}}
			execution, err := policy.NewVerifiedExecutionWithGrants(tenant, actor, grants)
			if err != nil {
				t.Fatal("create registered extension execution:", err)
			}
			target := contract.ResourceRef{Namespace: "tenant", Name: "Task", Version: 1}
			handler := contract.ResourceRef{Namespace: "module", Name: handlerName, Version: 1}
			ownerModule := contract.ModuleDescriptor{ID: "work.tasks", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: target}}, ExtensionPoints: []contract.ExtensionPoint{{
				OwnerModuleID: "work.tasks", ID: "task.after-save", Version: 1, Target: target,
				AllowedKinds: []string{string(contract.ExtensionPostCommit)}, InputSchema: contract.SchemaRef{ID: "task-event", Version: 1},
				OutputSchema: contract.SchemaRef{ID: "task-result", Version: 1}, ExecutionPhase: "after_commit", FailurePolicy: "retry",
			}}}
			extenderModule := contract.ModuleDescriptor{ID: "audit.tasks", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handler}}, Extensions: []contract.ExtensionRegistration{{
				ID: "task-audit", ExtenderModuleID: "audit.tasks", PointID: "work.tasks/task.after-save", PointVersion: 1,
				Kind: contract.ExtensionPostCommit, ArtifactDigest: strings.Repeat("d", 64), HandlerRef: handler,
				Event: string(script.EventAfterSave), Order: 10,
			}}}
			modules := []contract.ModuleDescriptor{ownerModule, extenderModule}
			extensions, err := contract.OrderModuleExtensions(modules, nil)
			if err != nil {
				t.Fatal("resolve post-commit extension:", err)
			}
			invocation := script.AsyncHookInvocation{
				TenantID: tenant, Site: siteName, SourceOperation: source, ConfigRevision: 1, Sequence: 1,
				Doctype: "Task", Event: script.EventAfterSave,
				Record: script.ScriptRecord{Name: handlerName, Site: siteName, ScriptType: script.TypeDocEvent,
					DocType: "Task", Event: script.EventAfterSave, IsActive: true, TimeoutMs: 5000, Script: "return true"},
				Document: []byte(`{"doctype":"Task","name":"SOURCE-1","title":"source"}`), Actor: actor, Grants: grants,
			}
			sourceTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin invocation source transaction:", err)
			}
			manager := &orm.TxManager{DB: database, Transaction: sourceTx, Context: ctx, ScriptStore: store,
				SiteName: siteName, Execution: execution, AsyncHookInvocations: store, AsyncHookTenantID: tenant,
				AsyncHookSourceOperation: source, AsyncHookConfigRevision: 1, ModuleDescriptors: modules, ModuleExtensions: extensions}
			if err := manager.CaptureAsyncAfterHooksTx(ctx, sourceTx, dt, script.EventAfterSave,
				&doctype.Document{DocType: "Task", Name: "SOURCE-1", Fields: map[string]any{"title": "source"}}, nil); err != nil {
				_ = sourceTx.Rollback()
				t.Fatal("capture registered module post-commit extension:", err)
			}
			if err := sourceTx.Commit(); err != nil {
				t.Fatal("commit source invocation:", err)
			}
			// A new pool/store/worker owner must recover the committed invocation
			// from SQL rather than relying on the source process's memory.
			if err := database.Close(); err != nil {
				t.Fatal("close source-process tenant pool:", err)
			}
			database, err = sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal("reopen tenant database for replacement worker:", err)
			}
			database.SetMaxOpenConns(8)
			if err := database.PingContext(ctx); err != nil {
				t.Fatal("ping reopened tenant database:", err)
			}
			store = &script.Store{DB: database, Dialect: dialect}

			writer := outbox.NewSQLWriter(dialect)
			failing := &integrationMutationLifecycleRunner{failAfterMutation: true}
			worked, err := processOneDurableLifecycleHook(ctx, failing, store, database, registry, dialect, writer,
				tenant, siteName, "worker-fail", nil, nil, nil)
			if err != nil || !worked {
				t.Fatalf("failed worker pass = (%t, %v), want handled script failure", worked, err)
			}
			if failing.mutationErr != nil {
				t.Fatalf("local domain mutation failed before injected script error: %v", failing.mutationErr)
			}
			assertDurableHookDatabaseCounts(t, ctx, database, dialect, dt, siteName, 0, 0)
			if status := readDurableInvocationStatus(t, ctx, database, dialect, tenant, source); status != "pending" {
				t.Fatalf("failed hook status = %q, want retryable pending", status)
			}
			if _, err := database.ExecContext(ctx, dbRebindForHook(dialect, `UPDATE _kora_script_invocation SET next_attempt_at = '2000-01-01 00:00:00' WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = 1`), string(tenant), string(source)); err != nil {
				t.Fatal("make retry due:", err)
			}
			succeeding := &integrationMutationLifecycleRunner{}
			worked, err = processOneDurableLifecycleHook(ctx, succeeding, store, database, registry, dialect, writer,
				tenant, siteName, "worker-retry", nil, nil, nil)
			if err != nil || !worked {
				t.Fatalf("successful worker pass = (%t, %v), want success", worked, err)
			}
			if succeeding.mutationErr != nil {
				t.Fatalf("authorized local domain mutation failed: %v", succeeding.mutationErr)
			}
			assertDurableHookDatabaseCounts(t, ctx, database, dialect, dt, siteName, 1, 1)
			if status := readDurableInvocationStatus(t, ctx, database, dialect, tenant, source); status != "succeeded" {
				t.Fatalf("successful hook status = %q, want succeeded", status)
			}
			if failing.calls != 1 || succeeding.calls != 1 {
				t.Fatalf("runner calls after retry: failed=%d succeeded=%d; want one each", failing.calls, succeeding.calls)
			}

			// A nested script write carries the broad worker grant but no trusted
			// owner-command provenance. The protected target must remain untouched.
			protectedSource := contract.OperationID(contract.NewOperationID())
			protectedInvocation := invocation
			protectedInvocation.SourceOperation = protectedSource
			protectedInvocation.Record.Name = "protected-task-after-save"
			protectedTx, err := database.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("begin protected invocation source transaction:", err)
			}
			if err := store.AppendAsyncHookTx(ctx, protectedTx, protectedInvocation); err != nil {
				_ = protectedTx.Rollback()
				t.Fatal("append protected nested-write invocation:", err)
			}
			if err := protectedTx.Commit(); err != nil {
				t.Fatal("commit protected invocation:", err)
			}
			protectedRunner := &integrationMutationLifecycleRunner{targetDoctype: protected.Name}
			worked, err = processOneDurableLifecycleHook(ctx, protectedRunner, store, database, registry, dialect, writer,
				tenant, siteName, "worker-protected", nil, nil, nil)
			if err != nil || !worked {
				t.Fatalf("protected nested-write worker pass = (%t, %v), want handled script rejection", worked, err)
			}
			var protectedRows int
			if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(protected.RawTableName())).Scan(&protectedRows); err != nil {
				t.Fatal("count protected nested script records:", err)
			}
			if protectedRows != 0 {
				t.Fatalf("nested script wrote %d commands-only records, want zero", protectedRows)
			}
			var protectedErr *contract.Error
			if !errors.As(protectedRunner.mutationErr, &protectedErr) || protectedErr.Type != contract.CodePermissionDenied {
				t.Fatalf("protected nested script mutation error = %v, want permission denied", protectedRunner.mutationErr)
			}
			assertDurableHookDatabaseCounts(t, ctx, database, dialect, dt, siteName, 1, 1)
			if status := readDurableInvocationStatus(t, ctx, database, dialect, tenant, protectedSource); status != "pending" {
				t.Fatalf("denied protected hook status = %q, want retryable pending", status)
			}

		})
	}
}

func durableLifecycleFixtureDDL(driver string) []string {
	var statements []string
	dialect := kdb.Resolve(driver)
	for _, statement := range dialect.SystemTableSQL() {
		if strings.Contains(statement, "_kora_naming_series") {
			statements = append(statements, statement)
		}
	}
	switch driver {
	case "mysql":
		statements = append(statements, kdb.KernelTablesMySQL()...)
		statements = append(statements, kdb.OutboxTablesMySQL()...)
	case "postgres":
		statements = append(statements, kdb.KernelTablesPostgres()...)
		statements = append(statements, kdb.OutboxTablesPostgres()...)
	case "libsql":
		statements = append(statements, kdb.KernelTablesLibSQL()...)
		statements = append(statements, kdb.OutboxTablesLibSQL()...)
	}
	statements = append(statements,
		`CREATE TABLE IF NOT EXISTS _kora_script (name VARCHAR(140) PRIMARY KEY, site VARCHAR(160) NOT NULL, script_type VARCHAR(50) NOT NULL, doctype VARCHAR(140) NOT NULL, event VARCHAR(100) NOT NULL, method_path VARCHAR(255) NOT NULL DEFAULT '', schedule VARCHAR(100) NOT NULL DEFAULT '', priority INTEGER NOT NULL DEFAULT 10, is_active BOOLEAN NOT NULL DEFAULT TRUE, run_as VARCHAR(140) NOT NULL DEFAULT '', timeout_ms INTEGER NOT NULL DEFAULT 5000, script TEXT NOT NULL, compiled_at TIMESTAMP NULL, compile_error TEXT NULL, created_by VARCHAR(140) NOT NULL DEFAULT '', updated_by VARCHAR(140) NOT NULL DEFAULT '', creation TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, modified TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS _kora_script_execution (id VARCHAR(64) PRIMARY KEY, site VARCHAR(160) NOT NULL, script_name VARCHAR(140) NOT NULL, script_type VARCHAR(50) NOT NULL, doctype VARCHAR(140) NOT NULL, docname VARCHAR(140) NOT NULL, event VARCHAR(100) NOT NULL, trigger_user VARCHAR(255) NOT NULL, duration_ms INTEGER NOT NULL, status VARCHAR(20) NOT NULL, error_message TEXT NULL, logged_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
	)
	return statements
}

func assertDurableHookDatabaseCounts(t *testing.T, ctx context.Context, database *sql.DB, dialect kdb.Dialect, dt *doctype.DocType, site string, wantRecords, wantOutbox int) {
	t.Helper()
	var records, events int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+dialect.QuoteIdent(dt.RawTableName())).Scan(&records); err != nil {
		t.Fatal("count nested domain records:", err)
	}
	if err := database.QueryRowContext(ctx, dbRebindForHook(dialect, `SELECT COUNT(*) FROM _kora_outbox WHERE site = ?`), site).Scan(&events); err != nil {
		t.Fatal("count nested domain outbox events:", err)
	}
	if records != wantRecords || events != wantOutbox {
		t.Fatalf("nested domain state counts = (%d records, %d events), want (%d, %d)", records, events, wantRecords, wantOutbox)
	}
}

func readDurableInvocationStatus(t *testing.T, ctx context.Context, database *sql.DB, dialect kdb.Dialect, tenant contract.TenantID, source contract.OperationID) string {
	t.Helper()
	var status string
	if err := database.QueryRowContext(ctx, dbRebindForHook(dialect, `SELECT status FROM _kora_script_invocation WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = 1`), string(tenant), string(source)).Scan(&status); err != nil {
		t.Fatal("read durable hook status:", err)
	}
	return status
}

func dbRebindForHook(dialect kdb.Dialect, query string) string { return kdb.Rebind(dialect, query) }
