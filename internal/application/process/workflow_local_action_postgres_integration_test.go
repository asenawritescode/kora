//go:build integration

package process

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/workflow"
	"github.com/asenawritescode/kora/workflow/sqlstore"
	_ "github.com/lib/pq"
)

func TestPostgresWorkflowLocalActionCommitsDomainAndProgressAtomically(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_POSTGRES_DSN to a disposable PostgreSQL database")
	}
	ctx := context.Background()
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL:", err)
	}
	database.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable PostgreSQL:", err)
	}
	dialect := db.Resolve("postgres")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal("bootstrap Engine tables:", err)
	}
	definitions, err := sqlstore.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure durable workflow schema:", err)
	}
	var suffix string
	if err := database.QueryRowContext(ctx, "SELECT replace(gen_random_uuid()::text, '-', '')").Scan(&suffix); err != nil {
		t.Fatal("generate integration suffix:", err)
	}
	siteName := "workflow-local-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	doctypeName := "WorkflowInvoice" + suffix[:12]
	dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "status", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Manager"}}, []*doctype.Permission{{Doctype: doctypeName, Role: "Manager", Read: true, Create: true, Write: true, Submit: true}})
	var databaseName string
	if err := database.QueryRowContext(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		t.Fatal("read disposable database name:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatal("migrate workflow subject doctype:", err)
	}
	engine := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	actorID := "workflow-service-" + suffix[:8]
	serviceGrant := policy.Grant{Resource: doctypeName, Operation: "create"}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: actorID, PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{serviceGrant})
	if err != nil {
		t.Fatal("verified workflow execution:", err)
	}
	createPayload, err := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: json.RawMessage(`{"title":"postgres workflow subject","status":"Draft"}`)})
	if err != nil {
		t.Fatal(err)
	}
	created, commandErr := engine.Execute(ctx, database, registry, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: createPayload, Context: kernel.OperationContext{Site: siteName, TenantID: tenant, Execution: execution, Source: kernel.SourceIntegration}})
	if commandErr != nil {
		t.Fatal("create workflow subject:", commandErr)
	}
	var document kernel.ResultData
	if err := json.Unmarshal(created.Data, &document); err != nil {
		t.Fatal("decode created subject:", err)
	}
	admission, err := dbaccess.NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := NewTenantTransactionOpener(tenant, database, admission)
	if err != nil {
		t.Fatal(err)
	}
	executionStore, err := sqlstore.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	key := workflow.ActionKey{Name: "invoice.create-follow-up", Version: 1}
	inputSchema, outputSchema := contract.SchemaRef{ID: "input", Version: 1}, contract.SchemaRef{ID: "output", Version: 1}
	builder := func(_ context.Context, action workflow.ActionContext, _ workflow.ActionInput) (kernel.Operation, error) {
		payload, err := json.Marshal(kernel.RecordCreatePayload{Doctype: action.Subject.Kind, Data: json.RawMessage(`{"title":"follow-up:` + string(action.InstanceID) + `","status":"Draft"}`)})
		if err != nil {
			return kernel.Operation{}, err
		}
		return kernel.Operation{Command: kernel.CommandRecordCreate, Payload: payload, Context: kernel.OperationContext{Site: "forged-site", User: "forged-user", IdempotencyKey: "forged-key"}}, nil
	}
	registration := workflow.ActionRegistration{Key: key, Class: workflow.ActionLocal, InputSchema: inputSchema, OutputSchema: outputSchema, RequiredGrant: serviceGrant}
	local, err := NewWorkflowLocalActionExecutor(engine, registry, begin, executionStore, integrationWorkflowSchemas{}, []workflow.ActionRegistration{registration}, map[workflow.ActionKey]WorkflowOperationBuilder{key: builder})
	if err != nil {
		t.Fatal("construct local workflow action:", err)
	}
	instanceID := contract.InstanceID(contract.NewID())
	claim := workflow.Claim{Tenant: tenant, BusinessScope: contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}, InstanceID: instanceID, StepID: "approve", Attempt: 1, Owner: "workflow-worker", Fence: 2, Revision: "1"}
	if _, err := database.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,lease_owner,lease_fence,lease_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`), string(instanceID), string(tenant), "invoice-flow", 1, doctypeName, document.Name, "1", string(contract.InstanceRunning), "approve", claim.Owner, uint64(claim.Fence), time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatal("insert claimed workflow:", err)
	}
	actx := workflow.ActionContext{Execution: execution, RequiredGrant: serviceGrant, InstanceID: instanceID, StepID: claim.StepID, ExecutionID: contract.NewOperationID(), Subject: contract.SubjectRef{Kind: doctypeName, ID: document.Name, Revision: "1"}, IdempotencyKey: contract.IdempotencyKey("workflow-local-" + suffix[:12])}
	if _, err := local.ExecuteLocal(ctx, claim, actx, key, workflow.ActionInput{Schema: inputSchema, Data: json.RawMessage(`{}`)}, "done"); err != nil {
		t.Fatal("execute transaction-bound PostgreSQL action:", err)
	}
	var subjectStatus, workflowStatus, nextStep string
	if err := database.QueryRowContext(ctx, "SELECT status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name=$1", document.Name).Scan(&subjectStatus); err != nil {
		t.Fatal("read workflow subject:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT status,current_step FROM _kora_workflow_instance WHERE tenant_id=$1 AND id=$2`, string(tenant), string(instanceID)).Scan(&workflowStatus, &nextStep); err != nil {
		t.Fatal("read workflow progress:", err)
	}
	if subjectStatus != "Draft" || workflowStatus != string(contract.InstanceReady) || nextStep != "done" {
		t.Fatalf("atomic PostgreSQL result: subject=%s workflow=(%s,%s)", subjectStatus, workflowStatus, nextStep)
	}
	var followUpCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE title=$1", "follow-up:"+string(instanceID)).Scan(&followUpCount); err != nil || followUpCount != 1 {
		t.Fatalf("PostgreSQL authorized follow-up count=%d err=%v", followUpCount, err)
	}
	var receiptCount, eventCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site=$1 AND command_name=$2 AND actor_user=$3`, string(tenant), kernel.CommandRecordCreate, actorID).Scan(&receiptCount); err != nil {
		t.Fatal("read domain command receipt:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_id=$1 AND event_type='kora.workflow.advanced'`, string(instanceID)).Scan(&eventCount); err != nil {
		t.Fatal("read workflow event:", err)
	}
	if receiptCount != 1 || eventCount != 1 {
		t.Fatalf("coordinated PostgreSQL effects receipt=%d event=%d", receiptCount, eventCount)
	}
}
