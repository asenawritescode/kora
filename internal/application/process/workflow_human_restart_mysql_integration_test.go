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
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/expression"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/asenawritescode/kora/internal/workflow/sqlstore"
)

type restartWorkflowRequestAuth struct{}

func (restartWorkflowRequestAuth) Submission(_ context.Context, _ policy.Execution, cmd contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	var subject *contract.SubjectRef
	if cmd.Subject != nil {
		copy := *cmd.Subject
		subject = &copy
	}
	return requests.SubmissionAuthorization{PolicyRevision: "1", MembershipRevision: "1", AssignmentMode: contract.AssignmentAny,
		ExpectedSubject: subject, SubjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Assignments: []requests.AssignmentRecord{{Principal: "reviewer-1", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "reviewer-1"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: []contract.ResponseKind{contract.ResponseComplete}, MembershipRevision: "1"}}}, nil
}
func (a restartWorkflowRequestAuth) WorkflowSubmission(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest, _ contract.RequestSource) (requests.SubmissionAuthorization, error) {
	return a.Submission(ctx, exec, cmd)
}
func (restartWorkflowRequestAuth) Response(_ context.Context, _ policy.Execution, record requests.RequestRecord, _ contract.RespondRequest) (requests.ResponseAuthorization, error) {
	return requests.ResponseAuthorization{PolicyRevision: record.PolicyRevision, MembershipRevision: "1", ExpectedSubject: record.Subject, SubjectDigest: record.SubjectDigest}, nil
}
func (restartWorkflowRequestAuth) Management(context.Context, policy.Execution, requests.RequestRecord, string, *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	return requests.ManagementAuthorization{}, errors.New("unexpected request management authorization")
}
func (restartWorkflowRequestAuth) Visibility(_ context.Context, execution policy.Execution) (requests.VisibilityScope, error) {
	return requests.VisibilityScope{Tenant: execution.Tenant(), Actor: contract.PrincipalID(execution.Actor().PrincipalID)}, nil
}

type restartWorkflowProjector struct{}

func (restartWorkflowProjector) Request(context.Context, policy.Execution, requests.RequestRecord) (contract.RequestView, error) {
	return contract.RequestView{}, errors.New("unexpected request projection")
}
func (restartWorkflowProjector) History(context.Context, policy.Execution, requests.RequestRecord, contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	return contract.RequestHistoryEntry{}, errors.New("unexpected history projection")
}
func (restartWorkflowProjector) Response(context.Context, policy.Execution, requests.RequestRecord, requests.DecisionRecord) (contract.ResponseDetail, error) {
	return contract.ResponseDetail{}, errors.New("unexpected response projection")
}

type restartWorkflowResponseSchemas struct{}

func (restartWorkflowResponseSchemas) Registered(context.Context, contract.SchemaRef) error {
	return nil
}
func (restartWorkflowResponseSchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	return nil
}

type restartWorkflowClock struct{ now time.Time }

func (c restartWorkflowClock) Now() time.Time { return c.now }
func (restartWorkflowClock) NewID() string    { return contract.NewID() }

type restartWorkflowScope struct{}

func (restartWorkflowScope) Lookup(string) (expression.Value, bool) {
	return expression.NullValue(), false
}
func (restartWorkflowScope) Aggregate(string, string, string) (expression.Value, error) {
	return expression.NullValue(), nil
}

type restartWorkflowScopes struct{}

func (restartWorkflowScopes) Scope(context.Context, policy.Execution, contract.WorkflowInstance) (expression.Scope, error) {
	return restartWorkflowScope{}, nil
}

type restartWorkflowWaits struct{}

func (restartWorkflowWaits) RegisteredCorrelationField(string, string) bool { return true }

func TestRolelessWorkflowServiceCannotSynthesizeTransitionRoleAfterWorkerRestart(t *testing.T) {
	dsn := os.Getenv("KORA_PROCESS_DISPOSABLE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_PROCESS_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL:", err)
	}
	database.SetMaxOpenConns(8)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		t.Fatal("ping disposable MySQL:", err)
	}
	dialect := db.Resolve("mysql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap Engine tables:", err)
	}
	var suffix string
	if err := database.QueryRowContext(ctx, "SELECT REPLACE(UUID(), '-', '')").Scan(&suffix); err != nil {
		t.Fatal("integration suffix:", err)
	}
	siteName := "workflow-human-" + suffix[:12]
	tenant := contract.TenantID("tenant-" + suffix)
	doctypeName := "WorkflowInvoice" + suffix[:12]
	dt := &doctype.DocType{Name: doctypeName, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}, {Fieldname: "status", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Manager"}}, []*doctype.Permission{{Doctype: doctypeName, Role: "Manager", Read: true, Create: true, Write: true, Submit: true}})
	var databaseName string
	if err := database.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal("read integration database:", err)
	}
	if err := schema.MigrateSiteFromRegistry(database, databaseName, registry, dialect); err != nil {
		t.Fatal("migrate workflow subject:", err)
	}
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("ensure outbox schema: %v\n%s", err, statement)
		}
	}
	for _, statement := range requestSchemaStatements(t, dialect) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("ensure request schema: %v\n%s", err, statement)
		}
	}
	engine := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	managerExecution, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: "manager-" + suffix[:8], PrincipalType: contract.PrincipalHuman, SubjectUserID: "manager-" + suffix[:8], Site: siteName, Roles: []string{"Manager"}, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("verified document creator execution:", err)
	}
	createPayload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: doctypeName, Data: json.RawMessage(`{"title":"restart subject","status":"Draft"}`)})
	created, commandErr := engine.Execute(ctx, database, registry, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: createPayload, Context: kernel.OperationContext{Site: siteName, TenantID: tenant, Execution: managerExecution, Actor: managerExecution.Actor(), User: managerExecution.Actor().PrincipalID, UserRole: "Manager", Roles: []string{"Manager"}, Source: kernel.SourceIntegration}})
	if commandErr != nil {
		t.Fatal("create subject:", commandErr)
	}
	var subjectRecord kernel.ResultData
	if err := json.Unmarshal(created.Data, &subjectRecord); err != nil {
		t.Fatal("decode subject:", err)
	}
	serviceGrant := policy.Grant{Resource: doctypeName, Operation: "submit"}
	serviceExecution, err := policy.NewVerifiedExecutionWithGrants(tenant, contract.ActorContext{PrincipalID: "kora-workflow-runtime", PrincipalType: contract.PrincipalService, Site: siteName, AuthenticatedAt: time.Now().UTC()}, []policy.Grant{serviceGrant})
	if err != nil {
		t.Fatal("verified roleless workflow service execution:", err)
	}

	inputSchema := contract.SchemaRef{ID: "workflow-human-input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "workflow-human-output", Version: 1}
	actionKey := workflow.ActionKey{Name: "invoice.approve", Version: 1}
	definition := contract.WorkflowDefinition{ID: contract.DefinitionID("invoice-human-flow-" + suffix[:8]), Version: 1, Entry: "human", Steps: []contract.StepDefinition{
		{ID: "human", Kind: contract.StepHuman, Human: &contract.HumanSpec{Request: contract.HumanRequestTemplate{Kind: contract.RequestTask, Subject: contract.SubjectInstance, Title: "Review invoice", Message: "Approve this invoice", Assignment: &contract.AssignmentSpec{Mode: contract.AssignmentAny, Assignees: []contract.Assignee{{Kind: contract.AssigneeUser, ID: "reviewer-1"}}}, DueAfterSeconds: 3600}, Outcomes: map[contract.ResponseKind]contract.StepID{contract.ResponseComplete: "approve"}, OnExpired: "ended", OnCancelled: "ended", OnStale: "ended"}},
		{ID: "approve", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: actionKey.Name, Version: actionKey.Version, InputSchema: inputSchema.ID, Input: json.RawMessage(`{}`), Next: "done"}},
		{ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "approved"}},
		{ID: "ended", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "not-approved"}},
	}}
	registrations := []workflow.ActionRegistration{{Key: actionKey, Class: workflow.ActionLocal, InputSchema: inputSchema, OutputSchema: outputSchema, RequiredGrant: serviceGrant}}
	builders := map[workflow.ActionKey]WorkflowOperationBuilder{actionKey: func(_ context.Context, action workflow.ActionContext, _ workflow.ActionInput) (kernel.Operation, error) {
		payload, err := json.Marshal(kernel.WorkflowStatusTransitionPayload{Doctype: action.Subject.Kind, Name: action.Subject.ID, Action: "approve", ExpectedRevision: 1})
		if err != nil {
			return kernel.Operation{}, err
		}
		return kernel.Operation{Command: kernel.CommandWorkflowStatusTransition, Payload: payload}, nil
	}}
	definitions, err := sqlstore.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	statusInput, err := json.Marshal(workflow.StatusActionConfig{StateField: "status", InitialState: "Draft", StatusTransition: workflow.StatusTransition{Action: "approve", From: "Draft", To: "Approved", AllowedRoles: []string{"Manager"}, DocStatus: 1}})
	if err != nil {
		t.Fatal(err)
	}
	statusDefinition := contract.WorkflowDefinition{ID: contract.DefinitionID("invoice-status-" + suffix[:8]), Version: 1, Entry: "transition",
		Trigger: &contract.RecordTrigger{Kind: "record.updated", Table: doctypeName, WatchedFields: []string{"status"}}, Steps: []contract.StepDefinition{
			{ID: "transition", Kind: contract.StepAction, Action: &contract.ActionSpec{Name: "record.update_status", Version: 1, InputSchema: "kora.record.update_status", Input: statusInput, Next: "done"}},
			{ID: "done", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}},
		}}
	if err := definitions.PublishImportedBatch(ctx, tenant, contract.PrincipalID(serviceExecution.Actor().PrincipalID), []contract.WorkflowDefinition{statusDefinition}, time.Now().UTC()); err != nil {
		t.Fatal("publish status policy workflow:", err)
	}
	engine.WorkflowStatusPolicies = definitions
	ref, err := definitions.PublishDefinition(ctx, tenant, definition, contract.PrincipalID(serviceExecution.Actor().PrincipalID), "publish-human-workflow-1", time.Now().UTC())
	if err != nil {
		t.Fatal("publish immutable workflow definition:", err)
	}
	instanceStore, err := sqlstore.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	instanceID := contract.InstanceID(contract.NewID())
	instance, err := instanceStore.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: contract.SubjectRef{Kind: doctypeName, ID: subjectRecord.Name, Revision: "1"}, Actor: contract.PrincipalID(serviceExecution.Actor().PrincipalID), IdempotencyKey: "start-human-workflow-1", Site: siteName, Now: time.Now().UTC()})
	if err != nil {
		t.Fatal("start durable workflow instance:", err)
	}

	composeHandler := func(database *sql.DB) (*workflow.DefinitionClaimHandler, *sqlstore.ExecutionStore, *requests.Service) {
		t.Helper()
		store, err := sqlstore.NewExecutionStore(database, dialect)
		if err != nil {
			t.Fatal("construct workflow execution store:", err)
		}
		requestStore, err := requestsql.New(database, dialect)
		if err != nil {
			t.Fatal("construct request store:", err)
		}
		requestService, err := requests.NewService(requestStore, restartWorkflowRequestAuth{}, restartWorkflowProjector{}, restartWorkflowResponseSchemas{}, restartWorkflowClock{now: time.Now().UTC()}, restartWorkflowClock{})
		if err != nil {
			t.Fatal("construct request service:", err)
		}
		admission, err := dbaccess.NewAdmission(1, 1, time.Second)
		if err != nil {
			t.Fatal("construct workflow admission:", err)
		}
		transaction, err := NewTenantTransactionOpener(tenant, database, admission)
		if err != nil {
			t.Fatal("construct workflow transaction opener:", err)
		}
		executor, err := NewWorkflowLocalActionExecutor(engine, registry, transaction, store, integrationWorkflowSchemas{}, registrations, builders)
		if err != nil {
			t.Fatal("construct transaction-bound workflow action executor:", err)
		}
		catalog := workflow.NewCatalog()
		for _, registration := range registrations {
			registration.Local = executor
			if err := catalog.Register(registration); err != nil {
				t.Fatal("register workflow action:", err)
			}
		}
		if err := catalog.Freeze(); err != nil {
			t.Fatal("freeze workflow action catalog:", err)
		}
		identity, err := workflow.NewTenantServiceIdentityProvider(tenant, serviceExecution.Actor(), []policy.Grant{serviceGrant})
		if err != nil {
			t.Fatal("construct workflow execution identity:", err)
		}
		handler, err := workflow.NewDefinitionClaimHandler(store, instanceStoreFor(database, dialect, t), definitionsFor(database, dialect, t), catalog, integrationWorkflowSchemas{}, identity, restartWorkflowScopes{}, expression.NewTreeEvaluator(), nil, workflow.HumanWaitRuntime{Requests: requestService, Store: store, Now: func() time.Time { return time.Now().UTC() }})
		if err != nil {
			t.Fatal("construct definition claim handler:", err)
		}
		return handler, store, requestService
	}

	// Start the human step, then close the process-owned DB handle before a
	// reviewer responds. A freshly composed worker must discover the durable
	// request and continue from its pinned definition.
	handler, executionStore, _ := composeHandler(database)
	claims, err := executionStore.ClaimDue(ctx, tenant, "worker-before-restart", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].InstanceID != instance.ID {
		t.Fatalf("claim human step = %#v, %v", claims, err)
	}
	if err := handler.HandleClaim(ctx, claims[0]); err != nil {
		t.Fatal("persist human request and pause workflow:", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal("close pre-restart database handle:", err)
	}
	database, err = sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("reopen database after worker restart:", err)
	}
	database.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping database after worker restart:", err)
	}
	restartedHandler, recoveredStore, requestService := composeHandler(database)
	var requestID, assignmentID string
	if err := database.QueryRowContext(ctx, `SELECT id FROM _kora_request WHERE tenant_id=? AND source_instance_id=? AND source_step_id='human'`, string(tenant), string(instanceID)).Scan(&requestID); err != nil {
		t.Fatal("find durable human request after restart:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT id FROM _kora_request_assignment WHERE tenant_id=? AND request_id=? AND assignee_id='reviewer-1'`, string(tenant), requestID).Scan(&assignmentID); err != nil {
		t.Fatal("find reviewer assignment:", err)
	}
	reviewer, err := policy.NewVerifiedExecution(tenant, contract.ActorContext{PrincipalID: "reviewer-1", PrincipalType: contract.PrincipalHuman, SubjectUserID: "reviewer-1", Site: siteName, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("verified reviewer execution:", err)
	}
	_, err = requestService.Respond(ctx, reviewer, contract.RequestID(requestID), contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: "review-human-workflow-1", ExpectedRevision: "1"}, AssignmentID: contract.AssignmentID(assignmentID), Response: contract.ResponseComplete})
	if err != nil {
		t.Fatal("authorized reviewer decision:", err)
	}
	resumed, err := recoveredStore.ResumeTerminalHumanWaits(ctx, tenant, 10)
	if err != nil || resumed != 1 {
		t.Fatalf("recover terminal human wait = %d, %v", resumed, err)
	}
	claims, err = recoveredStore.ClaimDue(ctx, tenant, "worker-after-restart", 1, time.Minute)
	if err != nil || len(claims) != 1 || claims[0].StepID != "approve" {
		t.Fatalf("claim resumed action = %#v, %v", claims, err)
	}
	if err := restartedHandler.HandleClaim(ctx, claims[0]); err != nil {
		t.Fatal("persist roleless service transition rejection:", err)
	}
	var status string
	if err := database.QueryRowContext(ctx, "SELECT status FROM "+dialect.QuoteIdent(dt.RawTableName())+" WHERE name=?", subjectRecord.Name).Scan(&status); err != nil {
		t.Fatal("read protected domain subject:", err)
	}
	if status != "Draft" {
		t.Fatalf("roleless workflow service changed domain state to %q, want Draft", status)
	}
	final, err := instanceStoreFor(database, dialect, t).GetInstance(ctx, tenant, instanceID)
	if err != nil {
		t.Fatal("read failed workflow:", err)
	}
	if final.Status != contract.InstanceFailed || final.CurrentStep != "approve" {
		t.Fatalf("workflow after roleless transition = (%s,%s), want (failed,approve)", final.Status, final.CurrentStep)
	}
}

func requestSchemaStatements(t *testing.T, dialect db.Dialect) []string {
	t.Helper()
	statements, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	return statements
}

func instanceStoreFor(database *sql.DB, dialect db.Dialect, t *testing.T) *sqlstore.InstanceStore {
	t.Helper()
	store, err := sqlstore.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func definitionsFor(database *sql.DB, dialect db.Dialect, t *testing.T) *sqlstore.DefinitionStore {
	t.Helper()
	store, err := sqlstore.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
