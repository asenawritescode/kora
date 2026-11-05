//go:build integration

package api

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
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/workflow"
	workflowsql "github.com/asenawritescode/kora/workflow/sqlstore"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
)

type apiWorkflowIntegrationSchemas struct{}

func (apiWorkflowIntegrationSchemas) Registered(contract.SchemaRef) bool                 { return true }
func (apiWorkflowIntegrationSchemas) Validate(contract.SchemaRef, json.RawMessage) error { return nil }

type apiWorkflowIntegrationWaits struct{}

func (apiWorkflowIntegrationWaits) RegisteredCorrelationField(string, string) bool { return true }

type apiWorkflowIntegrationAuthorizer struct{}

func (apiWorkflowIntegrationAuthorizer) CanManageWorkflowDefinitions(context.Context, policy.Execution) (bool, error) {
	return true, nil
}
func (apiWorkflowIntegrationAuthorizer) CanStartWorkflow(context.Context, policy.Execution, contract.DefinitionID, contract.SubjectRef) (bool, error) {
	return true, nil
}
func (apiWorkflowIntegrationAuthorizer) CanReadWorkflow(context.Context, policy.Execution, contract.SubjectRef) (bool, error) {
	return true, nil
}

func TestGridHTTPRecordCreateStartsWorkflowFromDurableOutbox(t *testing.T) {
	handler, database, registry, siteName := newResourceIntegrationHandler(t)
	dialect := db.Resolve("mysql")
	definitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("create workflow definition store:", err)
	}
	if err := definitionStore.Ensure(t.Context()); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	instanceStore, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal("create workflow instance store:", err)
	}
	catalog := workflow.NewCatalog()
	if err := catalog.Freeze(); err != nil {
		t.Fatal("freeze workflow catalog:", err)
	}
	authorizer := apiWorkflowIntegrationAuthorizer{}
	instances, err := workflow.NewInstanceService(definitionStore, instanceStore, authorizer, catalog,
		apiWorkflowIntegrationSchemas{}, apiWorkflowIntegrationWaits{}, time.Now, nil)
	if err != nil {
		t.Fatal("create workflow instance service:", err)
	}
	tenant, err := contract.ParseTenantID(siteName)
	if err != nil {
		t.Fatal("parse test tenant:", err)
	}
	definition := contract.WorkflowDefinition{
		ID: "grid-record-created", Version: 1, Entry: "finish",
		Trigger: &contract.RecordTrigger{Kind: "record.created", Table: "TestDoc"},
		Steps:   []contract.StepDefinition{{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}},
	}
	publisher, err := contract.ParsePrincipalID("grid-workflow-publisher")
	if err != nil {
		t.Fatal("parse workflow publisher:", err)
	}
	if _, err := definitionStore.PublishDefinition(t.Context(), tenant, definition, publisher, "grid-publish-trigger", time.Now().UTC()); err != nil {
		t.Fatal("publish record-triggered workflow:", err)
	}
	updateDefinition := contract.WorkflowDefinition{
		ID: "grid-record-updated", Version: 1, Entry: "finish",
		Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: "TestDoc", WatchedFields: []string{"title"}},
		Steps:   []contract.StepDefinition{{ID: "finish", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}},
	}
	if _, err := definitionStore.PublishDefinition(t.Context(), tenant, updateDefinition, publisher, "grid-publish-update-trigger", time.Now().UTC()); err != nil {
		t.Fatal("publish record-updated workflow:", err)
	}
	dispatcher := outbox.NewDispatcher(database, dialect)
	var triggerErr error
	if err := dispatcher.RegisterForPatterns("workflow-record-triggers", workflow.RecordTriggerEventPatterns, func(ctx context.Context, event contract.EventEnvelope) error {
		trigger, ok := workflow.RecordTriggerFromChangeEvent(event)
		if !ok {
			return nil
		}
		_, triggerErr = instances.StartFromRecordEvent(ctx, tenant, siteName, trigger)
		return triggerErr
	}); err != nil {
		t.Fatal("register workflow trigger consumer:", err)
	}
	created := serveResourceMutationBody(t, handler, database, registry, siteName, httpMethodPost,
		"/api/resource/TestDoc", `{"title":"created from grid HTTP"}`, "grid-create-triggered-record", []string{doctype.AdminRole})
	if created.Code != http.StatusCreated {
		t.Fatalf("grid create status=%d body=%s", created.Code, created.Body.String())
	}
	createdDoc := requireRESTDocument(t, created.Body.Bytes(), "TestDoc")
	recordID, ok := createdDoc["name"].(string)
	if !ok || recordID == "" {
		t.Fatalf("grid create response omitted record identity: %#v", createdDoc)
	}
	processed, err := dispatcher.Run(t.Context(), 10)
	if err != nil || triggerErr != nil || processed != 1 {
		t.Fatalf("dispatch grid record event: processed=%d err=%v trigger_err=%v", processed, err, triggerErr)
	}
	var instanceID string
	if err := database.QueryRowContext(t.Context(), "SELECT id FROM _kora_workflow_instance WHERE tenant_id=? AND definition_id=? AND subject_id=?", string(tenant), string(definition.ID), recordID).Scan(&instanceID); err != nil {
		t.Fatal("load workflow instance created from grid event:", err)
	}
	var started int
	if err := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='started'", string(tenant), instanceID).Scan(&started); err != nil {
		t.Fatal("load grid workflow history:", err)
	}
	if started != 1 {
		t.Fatalf("grid-triggered workflow start history count=%d, want 1", started)
	}
	updated := serveResourceMutationBody(t, handler, database, registry, siteName, httpMethodPut,
		"/api/resource/TestDoc/"+recordID, `{"title":"updated from grid HTTP"}`, "grid-update-triggered-record", []string{doctype.AdminRole})
	if updated.Code != http.StatusOK {
		t.Fatalf("grid update status=%d body=%s", updated.Code, updated.Body.String())
	}
	processed, err = dispatcher.Run(t.Context(), 10)
	if err != nil || triggerErr != nil || processed != 1 {
		t.Fatalf("dispatch grid record update: processed=%d err=%v trigger_err=%v", processed, err, triggerErr)
	}
	var updateStarts int
	if err := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM _kora_workflow_history h JOIN _kora_workflow_instance i ON i.tenant_id=h.tenant_id AND i.id=h.instance_id WHERE i.tenant_id=? AND i.definition_id=? AND i.subject_id=? AND h.kind='started'", string(tenant), string(updateDefinition.ID), recordID).Scan(&updateStarts); err != nil {
		t.Fatal("load grid update workflow history:", err)
	}
	if updateStarts != 1 {
		t.Fatalf("grid update workflow start history count=%d, want 1", updateStarts)
	}
	unwatched := serveResourceMutationBody(t, handler, database, registry, siteName, httpMethodPut,
		"/api/resource/TestDoc/"+recordID, `{"serial":"SERIAL-1"}`, "grid-unwatched-update-record", []string{doctype.AdminRole})
	if unwatched.Code != http.StatusOK {
		t.Fatalf("grid unwatched-field update status=%d body=%s", unwatched.Code, unwatched.Body.String())
	}
	processed, err = dispatcher.Run(t.Context(), 10)
	if err != nil || triggerErr != nil || processed != 1 {
		t.Fatalf("dispatch unwatched-field record update: processed=%d err=%v trigger_err=%v", processed, err, triggerErr)
	}
	if err := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM _kora_workflow_instance WHERE tenant_id=? AND definition_id=? AND subject_id=?", string(tenant), string(updateDefinition.ID), recordID).Scan(&updateStarts); err != nil {
		t.Fatal("verify watched-field filtering:", err)
	}
	if updateStarts != 1 {
		t.Fatalf("workflow starts after an unwatched update=%d, want 1", updateStarts)
	}
	if replayed, err := dispatcher.Run(t.Context(), 10); err != nil || replayed != 0 {
		t.Fatalf("grid event replay dispatched again: processed=%d err=%v", replayed, err)
	}
}
func (apiWorkflowIntegrationAuthorizer) CanReconcileWorkflow(context.Context, policy.Execution, contract.WorkflowInstance) (bool, error) {
	return true, nil
}

func TestWorkflowHTTPPublishStartReplayAndReadAgainstMySQL(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_api_workflow_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.DBName = name
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dialect := db.Resolve("mysql")
	definitionStore, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitionStore.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("ensure outbox schema:", err)
		}
	}
	instanceStore, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	catalog := workflow.NewCatalog()
	if err := catalog.Freeze(); err != nil {
		t.Fatal(err)
	}
	authorizer := apiWorkflowIntegrationAuthorizer{}
	schemas, waits := apiWorkflowIntegrationSchemas{}, apiWorkflowIntegrationWaits{}
	definitions, err := workflow.NewService(definitionStore, authorizer, catalog, schemas, waits, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := workflow.NewInstanceService(definitionStore, instanceStore, authorizer, catalog, schemas, waits, time.Now, nil)
	if err != nil {
		t.Fatal(err)
	}
	tenantID := contract.TenantID("api-workflow-tenant")
	execution, err := policy.NewVerifiedExecution(tenantID, contract.ActorContext{PrincipalID: "api-workflow-operator", PrincipalType: contract.PrincipalHuman, Site: "workflow.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("workflow.example.test", SiteRuntimeService{WorkflowDefinitions: definitions, WorkflowInstances: instances})
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("policy_execution", execution)
		c.Set("site_name", "workflow.example.test")
		c.Set("site_id", string(tenantID))
		c.Next()
	})
	RegisterWorkflowRoutes(router.Group("/api/v2"), services)

	definition := `{"id":"api-process","version":1,"entry":"finish","steps":[{"id":"finish","kind":"end","end":{"outcome":"complete"}}]}`
	publish := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-definitions", strings.NewReader(definition))
	publish.Header.Set("Idempotency-Key", "api-publish-process-1")
	publishResponse := httptest.NewRecorder()
	router.ServeHTTP(publishResponse, publish)
	if publishResponse.Code != http.StatusCreated {
		t.Fatalf("publish status=%d body=%s", publishResponse.Code, publishResponse.Body.String())
	}
	var published struct {
		Data contract.DefinitionVersionRef `json:"data"`
	}
	if err := json.Unmarshal(publishResponse.Body.Bytes(), &published); err != nil {
		t.Fatal("decode publish response:", err)
	}
	if published.Data.DefinitionID != "api-process" || published.Data.Version != 1 || len(published.Data.Digest) != 64 {
		t.Fatalf("unexpected published definition reference: %+v", published.Data)
	}

	startBody := `{"meta":{"idempotency_key":"api-start-process-1"},"definition_id":"api-process","definition_version":1,"subject":{"kind":"WorkItem","id":"WI-1","revision":"1"}}`
	start := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-instances", strings.NewReader(startBody))
	start.Header.Set("Idempotency-Key", "api-start-process-1")
	startResponse := httptest.NewRecorder()
	router.ServeHTTP(startResponse, start)
	if startResponse.Code != http.StatusCreated {
		t.Fatalf("start status=%d body=%s", startResponse.Code, startResponse.Body.String())
	}
	var started struct {
		Data contract.WorkflowInstance `json:"data"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatal("decode start response:", err)
	}
	if started.Data.ID == "" || started.Data.Status != contract.InstanceReady || started.Data.Subject.ID != "WI-1" {
		t.Fatalf("unexpected started instance: %+v", started.Data)
	}

	replay := httptest.NewRequest(http.MethodPost, "/api/v2/workflow-instances", strings.NewReader(startBody))
	replay.Header.Set("Idempotency-Key", "api-start-process-1")
	replayResponse := httptest.NewRecorder()
	router.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusCreated {
		t.Fatalf("start replay status=%d body=%s", replayResponse.Code, replayResponse.Body.String())
	}
	var replayed struct {
		Data contract.WorkflowInstance `json:"data"`
	}
	if err := json.Unmarshal(replayResponse.Body.Bytes(), &replayed); err != nil {
		t.Fatal("decode replay response:", err)
	}
	if replayed.Data.ID != started.Data.ID {
		t.Fatalf("replay returned instance %s, want original %s", replayed.Data.ID, started.Data.ID)
	}

	read := httptest.NewRequest(http.MethodGet, "/api/v2/workflow-instances/"+string(started.Data.ID), nil)
	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, read)
	if readResponse.Code != http.StatusOK {
		t.Fatalf("instance read status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}
	var loaded struct {
		Data contract.WorkflowInstance `json:"data"`
	}
	if err := json.Unmarshal(readResponse.Body.Bytes(), &loaded); err != nil {
		t.Fatal("decode instance response:", err)
	}
	if loaded.Data.ID != started.Data.ID || loaded.Data.Subject.ID != started.Data.Subject.ID {
		t.Fatalf("persisted instance read differs from start: %+v", loaded.Data)
	}
}
