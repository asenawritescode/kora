//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/email"
	"github.com/asenawritescode/kora/internal/expression"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

type recordTriggerIntegrationSchemas struct{}

func (recordTriggerIntegrationSchemas) Registered(contract.SchemaRef) bool { return false }
func (recordTriggerIntegrationSchemas) Validate(contract.SchemaRef, json.RawMessage) error {
	return fmt.Errorf("no action schemas are registered")
}
func (recordTriggerIntegrationSchemas) RecordTableExists(table string) bool { return table == "Task" }
func (recordTriggerIntegrationSchemas) RecordFieldExists(table, field string) bool {
	return table == "Task" && field == "title"
}
func (recordTriggerIntegrationSchemas) RecordFieldType(table, field string) (string, bool) {
	if table == "Task" && field == "title" {
		return "Data", true
	}
	return "", false
}

type recordTriggerIntegrationWaits struct{}

func (recordTriggerIntegrationWaits) RegisteredCorrelationField(string, string) bool { return false }

type recordTriggerIntegrationScope struct{}

func (recordTriggerIntegrationScope) Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error) {
	return recordTriggerEmptyScope{}, nil
}

type recordTriggerEmptyScope struct{}

func (recordTriggerEmptyScope) Lookup(string) (expression.Value, bool) {
	return expression.NullValue(), false
}
func (recordTriggerEmptyScope) Aggregate(string, string, string) (expression.Value, error) {
	return expression.NullValue(), nil
}

type recordTriggerIntegrationAuthorizer struct{}

type recordTriggerObservedAction struct {
	inner workflow.Action
	err   error
}

func (a *recordTriggerObservedAction) Execute(ctx context.Context, actionContext workflow.ActionContext, input workflow.ActionInput) (workflow.ActionResult, error) {
	result, err := a.inner.Execute(ctx, actionContext, input)
	a.err = err
	return result, err
}

func (recordTriggerIntegrationAuthorizer) CanManageWorkflowDefinitions(context.Context, policy.Execution) (bool, error) {
	return true, nil
}
func (recordTriggerIntegrationAuthorizer) CanStartWorkflow(context.Context, policy.Execution, contract.DefinitionID, contract.SubjectRef) (bool, error) {
	return true, nil
}
func (recordTriggerIntegrationAuthorizer) CanReadWorkflow(context.Context, policy.Execution, contract.SubjectRef) (bool, error) {
	return true, nil
}
func (recordTriggerIntegrationAuthorizer) CanReconcileWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error) {
	return true, nil
}

// TestGridHTTPRecordCreateStartsWorkflowAndDeliversEmail verifies the actual
// grid HTTP mutation -> transactional outbox -> trigger dispatcher -> durable
// workflow worker -> Mailpit delivery and run history path against isolated
// MySQL.
func TestGridHTTPRecordCreateStartsWorkflowAndDeliversEmail(t *testing.T) {
	mailpitURL := strings.TrimRight(envOr("KORA_WORKFLOW_MAILPIT_API", "http://127.0.0.1:8025"), "/")
	mailpitClient := &http.Client{Timeout: time.Second}
	if _, err := getMailpitMessages(t.Context(), mailpitClient, mailpitURL); err != nil {
		t.Skipf("Mailpit is unavailable: %v", err)
	}
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open MySQL root connection:", err)
	}
	if err := root.PingContext(t.Context()); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_record_trigger_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.ExecContext(t.Context(), "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
		_ = root.Close()
	})
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse MySQL DSN:", err)
	}
	config.DBName = name
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	if err := site.BootstrapSystemTablesContext(ctx, database, dialect); err != nil {
		t.Fatal("bootstrap Engine system tables:", err)
	}
	for _, statement := range append(append([]string{}, db.OutboxTablesMySQL()...), db.KernelTablesMySQL()...) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("bootstrap mutation/outbox tables:", err)
		}
	}
	tenant := contract.TenantID("tenant-" + contract.NewID())
	siteName := "workflow-trigger.example.test"
	doctypeDefinition := &doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "email", Fieldtype: "Email"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{doctypeDefinition}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
		Doctype: "Task", Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
	}})
	if err := schema.MigrateSiteFromRegistry(database, name, registry, dialect); err != nil {
		t.Fatal("create Task table:", err)
	}

	definitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("create workflow definition store:", err)
	}
	if err := definitionStore.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow tables:", err)
	}
	instanceStore, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal("create workflow instance store:", err)
	}
	recipient := "workflow-trigger-" + strings.ToLower(contract.NewID()) + "@example.test"
	definition := contract.WorkflowDefinition{
		ID: "task-created-notification", Version: 1, Entry: "send-email",
		Trigger: &contract.RecordTrigger{Kind: "record.created", Table: "Task"},
		Steps: []contract.StepDefinition{
			{ID: "send-email", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "email.send", Version: 1, InputSchema: emailWorkflowInputSchema.ID, Input: json.RawMessage(`{"recipient_field":"email","subject":"Kora workflow trigger integration","message":"An Engine record mutation started this workflow and delivered this email."}`), Next: "finish"}},
			{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
		},
	}
	actor, err := contract.ParsePrincipalID("workflow-integration-admin")
	if err != nil {
		t.Fatal("parse definition publisher:", err)
	}
	if _, err := definitionStore.PublishDefinition(ctx, tenant, definition, actor, "publish-triggered-task", time.Now().UTC()); err != nil {
		t.Fatal("publish record-triggered workflow:", err)
	}
	if _, _, err := definitionStore.GetDefinition(ctx, tenant, definition.ID, definition.Version); err != nil {
		t.Fatal("read published workflow definition:", err)
	}
	mailer := email.NewSender(&email.Config{Host: envOr("KORA_WORKFLOW_TEST_SMTP_HOST", "127.0.0.1"), Port: 1025, TLSMode: "plain", From: "no-reply@kora.local"})
	registration := emailWorkflowRegistration(mailer, database, registry, dialect)
	observedAction := &recordTriggerObservedAction{inner: registration.External}
	registration.External = observedAction
	catalog := workflow.NewCatalog()
	if err := catalog.Register(registration); err != nil {
		t.Fatal("register workflow email action:", err)
	}
	if err := catalog.Freeze(); err != nil {
		t.Fatal("freeze workflow action catalog:", err)
	}
	workflowSchemas := tenantWorkflowSchemas{base: recordTriggerIntegrationSchemas{}, registry: registry, mailer: true}
	instances, err := workflow.NewInstanceService(definitionStore, instanceStore, recordTriggerIntegrationAuthorizer{}, catalog,
		workflowSchemas, recordTriggerIntegrationWaits{}, time.Now, nil)
	if err != nil {
		t.Fatal("construct workflow instance service:", err)
	}
	executionStore, err := workflowsql.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("create workflow execution store:", err)
	}
	identity, err := newEngineWorkflowIdentity(tenant, siteName, []policy.Grant{registration.RequiredGrant})
	if err != nil {
		t.Fatal("create workflow service identity:", err)
	}
	claimHandler, err := workflow.NewDefinitionClaimHandler(executionStore, instanceStore, definitionStore, catalog, workflowSchemas,
		identity, recordTriggerIntegrationScope{}, expression.NewTreeEvaluator(), nil, workflow.HumanWaitRuntime{})
	if err != nil {
		t.Fatal("construct workflow execution handler:", err)
	}
	worker, err := workflow.NewExecutionWorker(executionStore, claimHandler, workflow.ExecutionWorkerConfig{
		Tenant: tenant, Owner: contract.NewID(), Concurrency: 1, Batch: 1, Lease: 10 * time.Second, PollInterval: time.Second,
	}, nil)
	if err != nil {
		t.Fatal("construct workflow execution worker:", err)
	}

	principal := "workflow-trigger-creator"
	actorContext := contract.ActorContext{PrincipalID: principal, PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecution(tenant, actorContext)
	if err != nil {
		t.Fatal("create verified record actor:", err)
	}
	outboxWriter := outbox.NewSQLWriter(dialect)
	apiHandler := api.NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect, Outbox: outboxWriter, SiteName: siteName})
	apiHandler.SiteOutboxes = map[string]outbox.Writer{siteName: outboxWriter}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/api/resource/:doctype", func(c *gin.Context) {
		c.Set("site_name", siteName)
		c.Set("site_id", string(tenant))
		c.Set("site_db", database)
		c.Set("site_registry", registry)
		c.Set("site_db_type", "mysql")
		c.Set("policy_execution", execution)
		c.Set("user", principal)
		c.Set("user_role", doctype.AdminRole)
		c.Set("user_roles", []string{doctype.AdminRole})
		c.Next()
	}, apiHandler.HandleCreate)
	request := httptest.NewRequest(http.MethodPost, "/api/resource/Task", strings.NewReader(`{"title":"trigger this workflow","email":"`+recipient+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-triggered-task")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("grid HTTP record create failed: status=%d body=%s", response.Code, response.Body.String())
	}
	var recordID string
	var created struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal("decode grid HTTP create response:", err)
	}
	recordID, _ = created.Data["name"].(string)
	if recordID == "" {
		t.Fatalf("grid HTTP create response omitted the record identity: %s", response.Body.String())
	}

	dispatcher := outbox.NewDispatcher(database, dialect)
	var triggerErr error
	if err := registerRecordTriggerConsumer(dispatcher, func(eventCtx context.Context, event contract.EventEnvelope) error {
		_, startErr := instances.StartFromRecordEvent(eventCtx, tenant, siteName, event)
		triggerErr = startErr
		return startErr
	}); err != nil {
		t.Fatal("register durable workflow trigger consumer:", err)
	}
	processed, err := dispatcher.Run(ctx, 10)
	if err != nil || triggerErr != nil || processed != 1 {
		t.Fatalf("dispatch committed record event: processed=%d err=%v trigger_err=%v", processed, err, triggerErr)
	}
	var instanceID string
	if err := database.QueryRowContext(ctx, "SELECT id FROM _kora_workflow_instance WHERE tenant_id = ? AND definition_id = ? AND subject_id = ?", string(tenant), string(definition.ID), recordID).Scan(&instanceID); err != nil {
		t.Fatal("load started workflow instance:", err)
	}
	var storedRecipient string
	if err := database.QueryRowContext(ctx, "SELECT email FROM `tabTask` WHERE name = ?", recordID).Scan(&storedRecipient); err != nil {
		t.Fatal("verify record recipient:", err)
	}
	if storedRecipient != recipient {
		t.Fatalf("stored recipient=%q, want %q", storedRecipient, recipient)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal("execute workflow email step:", err)
	}
	var historyDebug string
	_ = database.QueryRowContext(ctx, "SELECT CONCAT(kind, ':', status, ':', detail) FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ? ORDER BY sequence DESC LIMIT 1", string(tenant), instanceID).Scan(&historyDebug)
	if historyDebug == "" {
		t.Fatal("workflow action did not record a history entry")
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal("execute workflow completion step:", err)
	}
	instance, err := instanceStore.GetInstance(ctx, tenant, contract.InstanceID(instanceID))
	if err != nil || instance.Status != contract.InstanceCompleted {
		t.Fatalf("workflow did not complete after email action: instance=%+v err=%v action_err=%v", instance, err, observedAction.err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := getMailpitMessages(ctx, mailpitClient, mailpitURL)
		if err != nil {
			t.Fatal("read Mailpit after workflow execution:", err)
		}
		for _, message := range messages {
			for _, to := range message.To {
				if strings.EqualFold(to.Address, recipient) {
					goto emailDelivered
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Mailpit did not receive email emitted by the triggered workflow")

emailDelivered:
	var historyCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?", string(tenant), instanceID).Scan(&historyCount); err != nil {
		t.Fatal("read workflow run history:", err)
	}
	if historyCount < 3 {
		t.Fatalf("workflow run history rows=%d, want start, email, and completion entries", historyCount)
	}
	if processedAgain, err := dispatcher.Run(ctx, 10); err != nil || processedAgain != 0 {
		t.Fatalf("record event replay dispatched again: processed=%d err=%v", processedAgain, err)
	}
	var instancesCount int
	if err := database.QueryRowContext(ctx, "SELECT COUNT(*) FROM _kora_workflow_instance WHERE tenant_id = ? AND definition_id = ? AND subject_id = ?", string(tenant), string(definition.ID), recordID).Scan(&instancesCount); err != nil {
		t.Fatal("verify trigger idempotency:", err)
	}
	if instancesCount != 1 {
		t.Fatalf("workflow instances for one record event=%d, want one", instancesCount)
	}
}
