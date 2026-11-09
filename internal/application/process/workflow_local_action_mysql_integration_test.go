//go:build integration

package process

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/asenawritescode/kora/internal/workflow/sqlstore"
	_ "github.com/go-sql-driver/mysql"
)

type integrationWorkflowSchemas struct{}

func (integrationWorkflowSchemas) Registered(contract.SchemaRef) bool { return true }
func (integrationWorkflowSchemas) Validate(_ contract.SchemaRef, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	return nil
}

type failAfterWorkflowCommitStore struct{ store *sqlstore.ExecutionStore }

func (s failAfterWorkflowCommitStore) ValidateClaimInTx(ctx context.Context, tx *sql.Tx, claim workflow.Claim) error {
	return s.store.ValidateClaimInTx(ctx, tx, claim)
}
func (s failAfterWorkflowCommitStore) CommitStepInTx(ctx context.Context, tx *sql.Tx, change workflow.StepCommit) error {
	if err := s.store.CommitStepInTx(ctx, tx, change); err != nil {
		return err
	}
	return errors.New("injected failure after workflow rows were staged")
}

func TestWorkflowLocalActionKeepsDomainAndProgressInOneTransaction(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable MySQL:", err)
	}
	dialect := db.Resolve("mysql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap Engine tables:", err)
	}
	workflowDDL, err := sqlstore.ExecutionSchemaStatements(dialect)
	if err != nil {
		t.Fatal("workflow schema:", err)
	}
	for _, statement := range workflowDDL {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("apply workflow schema: %v\n%s", err, statement)
		}
	}
	var suffix string
	if err := database.QueryRowContext(ctx, "SELECT REPLACE(UUID(), '-', '')").Scan(&suffix); err != nil {
		t.Fatal("integration suffix:", err)
	}
	siteName := "workflow-local-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	doctypeName := "WorkflowInvoice" + suffix[:12]
	dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "status", Fieldtype: "Data"}}}
	moduleInvoice := &doctype.DocType{Name: "ModuleInvoice" + suffix[:8], ModuleOwnerID: "finance.ar", ModuleOwnershipMode: string(contract.MutationCommands), BusinessScopePolicy: contract.ScopeTenantShared, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt, moduleInvoice}, []*doctype.Role{{Name: "Manager"}}, []*doctype.Permission{{Doctype: doctypeName, Role: "Manager", Read: true, Create: true, Write: true, Submit: true}, {Doctype: moduleInvoice.Name, Role: "Manager", Read: true, Create: true, Write: true}})
	var databaseName string
	if err := database.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal("read test database:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatal("migrate workflow subject doctype:", err)
	}
	engine := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	engine.CommandSchemas = integrationWorkflowSchemas{}
	moduleCommand, err := kernel.ParseCommandResource([]byte("name: issue\nnamespace: finance.invoice\nversion: 1\ninput:\n  record: " + moduleInvoice.Name + "\nmodule_owner_id: finance.ar\ncapability: finance.invoice.issue\noperation: issue\ninput_schema:\n  id: finance.invoice.issue.input\n  version: 1\noutput_schema:\n  id: finance.invoice.issue.output\n  version: 1\ntransaction:\n  - create:\n      record: " + moduleInvoice.Name + "\n      values:\n        title: $input.title\nemit:\n  - finance.invoice.issued\n"))
	if err != nil {
		t.Fatal("parse registered owner command:", err)
	}
	engine.Commands = kernel.NewCommandRegistry()
	if err := engine.Commands.Register(moduleCommand); err != nil {
		t.Fatal("register owner command:", err)
	}
	serviceGrant := policy.Grant{Resource: doctypeName, Operation: "create"}
	moduleCommandGrant := policy.Grant{Resource: moduleInvoice.Name, Operation: "create"}
	// A published module command is first authorised as module.<owner>.operation.<name>.
	moduleOperationGrant := policy.Grant{Resource: "module.finance.ar.operation.issue", Operation: "write"}
	servicePrincipal := "workflow-service-" + suffix[:8]
	execution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: servicePrincipal, PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{serviceGrant, moduleCommandGrant, moduleOperationGrant})
	if err != nil {
		t.Fatal("verified workflow execution:", err)
	}
	createPayload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: json.RawMessage(`{"title":"local action subject","status":"Draft"}`)})
	created, commandErr := engine.Execute(ctx, database, registry, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: createPayload, Context: kernel.OperationContext{Site: siteName, TenantID: tenant, Execution: execution, Source: kernel.SourceIntegration}})
	if commandErr != nil {
		t.Fatal("create workflow subject:", commandErr)
	}
	var doc kernel.ResultData
	if err := json.Unmarshal(created.Data, &doc); err != nil {
		t.Fatal("decode created subject:", err)
	}
	admission, err := dbaccess.NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal("active-work admission:", err)
	}
	begin, err := NewTenantTransactionOpener(tenant, database, admission)
	if err != nil {
		t.Fatal("tenant transaction opener:", err)
	}
	store, err := sqlstore.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("workflow execution store:", err)
	}
	moduleStore := configstore.NewModuleStore(database, dialect)
	if err := moduleStore.EnsureSchema(); err != nil {
		t.Fatal("ensure capability lease schema:", err)
	}
	// Transactional capability admission requires the caller and provider modules
	// to be active installations in the tenant's module snapshot.
	activeModules, err := json.Marshal(contract.ModuleSnapshot{TenantID: tenant, Revision: 1, Installations: []contract.ModuleInstallation{
		{TenantID: tenant, ModuleID: "sales.orders", State: string(contract.ModuleActive), Revision: 1},
		{TenantID: tenant, ModuleID: "finance.ar", State: string(contract.ModuleActive), Revision: 1},
	}})
	if err != nil {
		t.Fatal("encode module snapshot:", err)
	}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`), string(tenant), 1, string(activeModules)); err != nil {
		t.Fatal("seed active module snapshot:", err)
	}
	key := workflow.ActionKey{Name: "invoice.create-follow-up", Version: 1}
	moduleCommandKey := workflow.ActionKey{Name: "invoice.issue-through-owner-command", Version: 1}
	inputSchema, outputSchema := contract.SchemaRef{ID: "workflow-input", Version: 1}, contract.SchemaRef{ID: "workflow-output", Version: 1}
	builder := func(_ context.Context, action workflow.ActionContext, _ workflow.ActionInput) (kernel.Operation, error) {
		payload, err := json.Marshal(kernel.RecordCreatePayload{Doctype: action.Subject.Kind, Data: json.RawMessage(`{"title":"follow-up:` + string(action.InstanceID) + `","status":"Draft"}`)})
		if err != nil {
			return kernel.Operation{}, err
		}
		// Deliberately incorrect caller fields prove the adapter replaces identity
		// from the verified execution and not from builder-supplied values.
		return kernel.Operation{Command: kernel.CommandRecordCreate, Payload: payload, Context: kernel.OperationContext{Site: "attacker", User: "attacker", IdempotencyKey: "attacker-key"}}, nil
	}
	moduleCommandBuilder := func(_ context.Context, action workflow.ActionContext, _ workflow.ActionInput) (kernel.Operation, error) {
		payload, err := json.Marshal(map[string]any{"data": map[string]any{"title": "issued:" + string(action.InstanceID)}})
		if err != nil {
			return kernel.Operation{}, err
		}
		moduleExecution, err := contract.NewCommandModuleExecution(action.BusinessScope, "sales.orders", "finance.ar", 1, moduleCommand.FullName(), moduleCommand.FullName())
		if err != nil {
			return kernel.Operation{}, err
		}
		return kernel.Operation{Command: moduleCommand.FullName(), Payload: payload, Context: kernel.OperationContext{ModuleExecution: &moduleExecution}}, nil
	}
	registration := workflow.ActionRegistration{Key: key, Class: workflow.ActionLocal, InputSchema: inputSchema, OutputSchema: outputSchema, RequiredGrant: serviceGrant}
	moduleLease := contract.CapabilityLease{ID: "workflow-module-lease-" + suffix[:12], TenantID: tenant, Scope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderModuleID: "finance.ar", ProviderID: "engine.internal.finance.ar", Operation: moduleCommand.FullName(), BindingRevision: 1, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	if err := moduleStore.CreateCapabilityLease(ctx, moduleLease); err != nil {
		t.Fatal("create workflow module capability lease:", err)
	}
	moduleCommandRegistration := workflow.ActionRegistration{Key: moduleCommandKey, Class: workflow.ActionLocal, InputSchema: inputSchema, OutputSchema: outputSchema, RequiredGrant: moduleCommandGrant, LocalCapabilityLease: &moduleLease}
	makeExecutor := func(stepStore WorkflowLocalStepStore) *WorkflowLocalActionExecutor {
		executor, err := NewWorkflowLocalActionExecutor(engine, registry, begin, stepStore, integrationWorkflowSchemas{}, []workflow.ActionRegistration{registration, moduleCommandRegistration}, map[workflow.ActionKey]WorkflowOperationBuilder{key: builder, moduleCommandKey: moduleCommandBuilder}, moduleStore)
		if err != nil {
			t.Fatal("construct workflow local executor:", err)
		}
		return executor
	}
	instanceID := contract.InstanceID(contract.NewID())
	insertRunningInstance := func(id contract.InstanceID) {
		t.Helper()
		_, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,lease_owner,lease_fence,lease_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?,?,DATE_ADD(CURRENT_TIMESTAMP(6),INTERVAL 2 MINUTE),CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))`, string(id), string(tenant), "invoice-flow", 1, doctypeName, doc.Name, "1", string(contract.InstanceRunning), "approve", "workflow-worker", 4)
		if err != nil {
			t.Fatal("insert running workflow:", err)
		}
	}
	insertRunningInstance(instanceID)
	claim := workflow.Claim{Tenant: tenant, BusinessScope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}, InstanceID: instanceID, StepID: "approve", Attempt: 1, Owner: "workflow-worker", Fence: 4, Revision: "1"}
	actionContext := workflow.ActionContext{Execution: execution, BusinessScope: claim.BusinessScope, RequiredGrant: serviceGrant, InstanceID: instanceID, StepID: "approve", ExecutionID: contract.NewOperationID(), Subject: contract.SubjectRef{Kind: doctypeName, ID: doc.Name, Revision: "1"}, IdempotencyKey: contract.IdempotencyKey("workflow-local-" + suffix[:12])}
	result, err := makeExecutor(store).ExecuteLocal(ctx, claim, actionContext, key, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done")
	if err != nil {
		t.Fatal("execute transaction-bound workflow action:", err)
	}
	if result.Schema != outputSchema || !json.Valid(result.Data) {
		t.Fatalf("local action result is not schema-bound: %+v", result)
	}
	var status string
	if err := database.QueryRowContext(ctx, "SELECT status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name=?", doc.Name).Scan(&status); err != nil {
		t.Fatal("read workflow subject:", err)
	}
	if status != "Draft" {
		t.Fatalf("workflow service unexpectedly changed the source subject: status=%q", status)
	}
	followUpTitle := "follow-up:" + string(instanceID)
	var followUpCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE title=?", followUpTitle).Scan(&followUpCount); err != nil || followUpCount != 1 {
		t.Fatalf("authorized follow-up record count=%d err=%v, want one", followUpCount, err)
	}
	var workflowStatus, currentStep string
	if err := database.QueryRowContext(ctx, `SELECT status,current_step FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, string(tenant), string(instanceID)).Scan(&workflowStatus, &currentStep); err != nil {
		t.Fatal("read workflow progress:", err)
	}
	if workflowStatus != "ready" || currentStep != "done" {
		t.Fatalf("workflow progress (%s,%s), want (ready,done)", workflowStatus, currentStep)
	}
	var receiptCount, stepEventCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name=? AND actor_user=?`, string(tenant), kernel.CommandRecordCreate, execution.Actor().PrincipalID).Scan(&receiptCount); err != nil {
		t.Fatal("read domain receipt:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_type='workflow_instance' AND aggregate_id=? AND event_type='kora.workflow.advanced'`, string(instanceID)).Scan(&stepEventCount); err != nil {
		t.Fatal("read workflow event:", err)
	}
	if receiptCount != 1 || stepEventCount != 1 {
		t.Fatalf("coordinated effects receipt=%d workflow_event=%d", receiptCount, stepEventCount)
	}

	moduleCommandInstance := contract.InstanceID(contract.NewID())
	insertRunningInstance(moduleCommandInstance)
	moduleCommandAction := actionContext
	moduleCommandAction.InstanceID = moduleCommandInstance
	moduleCommandAction.ExecutionID = contract.NewOperationID()
	moduleCommandAction.IdempotencyKey = contract.IdempotencyKey("workflow-module-command-" + suffix[:12])
	moduleCommandAction.RequiredGrant = moduleCommandGrant
	moduleCommandClaim := claim
	moduleCommandClaim.InstanceID = moduleCommandInstance
	if _, err := makeExecutor(store).ExecuteLocal(ctx, moduleCommandClaim, moduleCommandAction, moduleCommandKey, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done"); err != nil {
		t.Fatal("execute trusted module owner command through workflow:", err)
	}
	if err := moduleStore.RevokeCapabilityLease(ctx, moduleLease, "security-admin", "test workflow revocation", time.Now().UTC()); err != nil {
		t.Fatal("revoke workflow module capability:", err)
	}
	deniedInstance := contract.InstanceID(contract.NewID())
	insertRunningInstance(deniedInstance)
	deniedClaim := moduleCommandClaim
	deniedClaim.InstanceID = deniedInstance
	deniedAction := moduleCommandAction
	deniedAction.InstanceID = deniedInstance
	deniedAction.ExecutionID = contract.NewOperationID()
	deniedAction.IdempotencyKey = contract.IdempotencyKey("workflow-module-revoked-" + suffix[:12])
	if _, err := makeExecutor(store).ExecuteLocal(ctx, deniedClaim, deniedAction, moduleCommandKey, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done"); err == nil {
		t.Fatal("revoked workflow module capability executed")
	}
	var issuedCount, issuedEventCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(moduleInvoice.RawTableName())+" WHERE title=?", "issued:"+string(moduleCommandInstance)).Scan(&issuedCount); err != nil || issuedCount != 1 {
		t.Fatalf("trusted workflow module command created %d invoices, err=%v", issuedCount, err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='finance.invoice.issued'`).Scan(&issuedEventCount); err != nil || issuedEventCount != 1 {
		t.Fatalf("trusted workflow module command emitted %d events, err=%v", issuedEventCount, err)
	}

	// A failure after both the domain command and workflow writes are staged
	// still rolls every effect back because the local executor owns one commit.
	rollbackID := contract.InstanceID(contract.NewID())
	insertRunningInstance(rollbackID)
	rollbackAction := actionContext
	rollbackAction.InstanceID = rollbackID
	rollbackAction.ExecutionID = contract.NewOperationID()
	rollbackAction.IdempotencyKey = contract.IdempotencyKey("workflow-rollback-" + suffix[:12])
	rollbackPrincipal := "workflow-rollback-" + suffix[:8]
	rollbackExecution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: rollbackPrincipal, PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{serviceGrant})
	if err != nil {
		t.Fatal("verified rollback workflow execution:", err)
	}
	rollbackAction.Execution = rollbackExecution
	rollbackClaim := claim
	rollbackClaim.InstanceID = rollbackID
	failed := makeExecutor(failAfterWorkflowCommitStore{store: store})
	if _, err := failed.ExecuteLocal(ctx, rollbackClaim, rollbackAction, key, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done"); err == nil {
		t.Fatal("injected staged workflow failure unexpectedly committed")
	}
	if err := database.QueryRowContext(ctx, "SELECT status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name=?", doc.Name).Scan(&status); err != nil {
		t.Fatal("read subject after rollback:", err)
	}
	if status != "Draft" {
		t.Fatalf("rollback fixture changed shared subject unexpectedly: %q", status)
	}
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE title=?", "follow-up:"+string(rollbackID)).Scan(&followUpCount); err != nil || followUpCount != 0 {
		t.Fatalf("rollback retained follow-up records=%d err=%v", followUpCount, err)
	}
	if err := database.QueryRowContext(ctx, `SELECT status,current_step FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, string(tenant), string(rollbackID)).Scan(&workflowStatus, &currentStep); err != nil {
		t.Fatal("read workflow after rollback:", err)
	}
	if workflowStatus != string(contract.InstanceRunning) || currentStep != "approve" {
		t.Fatalf("failed transaction changed workflow state: (%s,%s)", workflowStatus, currentStep)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name=? AND actor_user=?`, string(tenant), kernel.CommandRecordCreate, rollbackExecution.Actor().PrincipalID).Scan(&receiptCount); err != nil {
		t.Fatal("read rolled-back domain receipt:", err)
	}
	if receiptCount != 0 {
		t.Fatalf("rolled-back domain receipt count=%d", receiptCount)
	}

	staleID := contract.InstanceID(contract.NewID())
	insertRunningInstance(staleID)
	if _, err := database.ExecContext(ctx, `UPDATE _kora_workflow_instance SET lease_fence=5 WHERE tenant_id=? AND id=?`, string(tenant), string(staleID)); err != nil {
		t.Fatal("replace stale workflow fence:", err)
	}
	staleAction := rollbackAction
	staleAction.InstanceID = staleID
	staleAction.ExecutionID = contract.NewOperationID()
	staleAction.IdempotencyKey = contract.IdempotencyKey("workflow-stale-" + suffix[:12])
	staleClaim := rollbackClaim
	staleClaim.InstanceID = staleID
	if _, err := makeExecutor(store).ExecuteLocal(ctx, staleClaim, staleAction, key, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done"); err == nil {
		t.Fatal("stale fenced claim executed a local domain action")
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=? AND command_name=? AND actor_user=?`, string(tenant), kernel.CommandRecordCreate, rollbackExecution.Actor().PrincipalID).Scan(&receiptCount); err != nil {
		t.Fatal("read stale-claim domain receipt:", err)
	}
	if receiptCount != 0 {
		t.Fatalf("stale-claim domain receipt count=%d", receiptCount)
	}
}
