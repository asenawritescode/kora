//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/requests"
	requestsql "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/asenawritescode/kora/workflow"
	"github.com/go-sql-driver/mysql"
)

type mysqlHumanWaitAuthorizer struct{}

func (mysqlHumanWaitAuthorizer) Submission(_ context.Context, _ policy.Execution, cmd contract.SubmitRequest) (requests.SubmissionAuthorization, error) {
	var subject *contract.SubjectRef
	if cmd.Subject != nil {
		copy := *cmd.Subject
		subject = &copy
	}
	allowed := mysqlHumanWaitResponses(cmd.Kind)
	policyID := ""
	if cmd.Kind == contract.RequestApproval {
		policyID = "human-wait-approval"
	}
	return requests.SubmissionAuthorization{PolicyID: policyID, PolicyRevision: "1", MembershipRevision: "1", AssignmentMode: contract.AssignmentAny,
		ExpectedSubject: subject, SubjectDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Assignments: []requests.AssignmentRecord{{Principal: "reviewer-1", OriginalTarget: contract.Assignee{Kind: contract.AssigneeUser, ID: "reviewer-1"}, Stage: 1, Status: contract.AssignmentPending, AllowedResponses: allowed, MembershipRevision: "1"}}}, nil
}
func (a mysqlHumanWaitAuthorizer) WorkflowSubmission(ctx context.Context, exec policy.Execution, cmd contract.SubmitRequest, _ contract.RequestSource) (requests.SubmissionAuthorization, error) {
	return a.Submission(ctx, exec, cmd)
}
func (mysqlHumanWaitAuthorizer) Response(_ context.Context, _ policy.Execution, record requests.RequestRecord, _ contract.RespondRequest) (requests.ResponseAuthorization, error) {
	return requests.ResponseAuthorization{PolicyRevision: "1", MembershipRevision: "1", ExpectedSubject: record.Subject, SubjectDigest: record.SubjectDigest}, nil
}
func (mysqlHumanWaitAuthorizer) Management(context.Context, policy.Execution, requests.RequestRecord, string, *contract.ReassignRequest) (requests.ManagementAuthorization, error) {
	return requests.ManagementAuthorization{}, errors.New("unexpected management authorization")
}
func (mysqlHumanWaitAuthorizer) Visibility(context.Context, policy.Execution) (requests.VisibilityScope, error) {
	return requests.VisibilityScope{}, errors.New("unexpected visibility authorization")
}

type mysqlHumanWaitProjector struct{}

func (mysqlHumanWaitProjector) Request(context.Context, policy.Execution, requests.RequestRecord) (contract.RequestView, error) {
	return contract.RequestView{}, errors.New("unexpected request projection")
}
func (mysqlHumanWaitProjector) History(context.Context, policy.Execution, requests.RequestRecord, contract.RequestHistoryEntry) (contract.RequestHistoryEntry, error) {
	return contract.RequestHistoryEntry{}, errors.New("unexpected history projection")
}
func (mysqlHumanWaitProjector) Response(context.Context, policy.Execution, requests.RequestRecord, requests.DecisionRecord) (contract.ResponseDetail, error) {
	return contract.ResponseDetail{}, errors.New("unexpected response projection")
}

type mysqlHumanWaitSchemas struct{}

func (mysqlHumanWaitSchemas) Registered(context.Context, contract.SchemaRef) error { return nil }
func (mysqlHumanWaitSchemas) Validate(context.Context, contract.SchemaRef, contract.InformationResponse) error {
	return nil
}

type mysqlHumanWaitClock struct{ now time.Time }

func (c mysqlHumanWaitClock) Now() time.Time { return c.now }

type mysqlHumanWaitIDs struct{}

func (mysqlHumanWaitIDs) NewID() string { return contract.NewID() }

func mysqlHumanWaitResponses(kind contract.RequestKind) []contract.ResponseKind {
	switch kind {
	case contract.RequestApproval:
		return []contract.ResponseKind{contract.ResponseApprove, contract.ResponseReject, contract.ResponseReturn}
	case contract.RequestInformation:
		return []contract.ResponseKind{contract.ResponseProvide}
	case contract.RequestAcknowledgment:
		return []contract.ResponseKind{contract.ResponseAcknowledge}
	default:
		return []contract.ResponseKind{contract.ResponseComplete}
	}
}

func TestMySQLHumanWaitAtomicallyCreatesRequestAndFencedPause(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_human_wait_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create outbox schema: %v", err)
		}
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL)`); err != nil {
		t.Fatal("create authorization epoch table:", err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_auth_revision (id,policy_revision,membership_revision) VALUES (1,1,1)`); err != nil {
		t.Fatal("seed authorization epochs:", err)
	}
	for _, statement := range requestsqlSchema(dialect, t) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create request schema: %v", err)
		}
	}
	workflowSchema, err := ExecutionSchemaStatements(dialect)
	if err != nil {
		t.Fatal("workflow schema statements:", err)
	}
	for _, statement := range workflowSchema {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create workflow schema: %v", err)
		}
	}
	definitionStore, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("construct definition store:", err)
	}
	if err := definitionStore.Ensure(ctx); err != nil {
		t.Fatal("ensure immutable definition schema:", err)
	}
	definition := mysqlHumanWaitDefinition()
	if _, err := definitionStore.PublishDefinition(ctx, "tenant-a", definition, "workflow-starter", "publish-human-wait", time.Now().UTC()); err != nil {
		t.Fatal("publish immutable human wait definition:", err)
	}
	requestStore, err := requestsql.New(database, dialect)
	if err != nil {
		t.Fatal("construct request store:", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	requestService, err := requests.NewService(requestStore, mysqlHumanWaitAuthorizer{}, mysqlHumanWaitProjector{}, mysqlHumanWaitSchemas{}, mysqlHumanWaitClock{now}, mysqlHumanWaitIDs{})
	if err != nil {
		t.Fatal("construct request service:", err)
	}
	exec, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "workflow-starter", PrincipalType: contract.PrincipalHuman, Site: "tenant.example.test", AuthenticatedAt: now})
	if err != nil {
		t.Fatal("construct verified workflow actor:", err)
	}
	workflowStore, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("construct workflow store:", err)
	}

	instanceID := contract.InstanceID(contract.NewID())
	if err := insertRunningHumanWaitInstance(ctx, database, instanceID, "approve", "worker-a", 1, now); err != nil {
		t.Fatal("insert successful wait instance:", err)
	}
	claim := workflow.Claim{Tenant: "tenant-a", InstanceID: instanceID, StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 1, Revision: "1"}
	due := now.Add(time.Hour)
	cmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "human-wait-success-key"}, Kind: contract.RequestTask,
		Subject: &contract.SubjectRef{Kind: "workflow_instance", ID: string(instanceID), Revision: "1"},
		Title:   "Review workflow item", Message: "Please review this item", DueAt: &due}
	prepared, err := requestService.PrepareWorkflowSubmit(ctx, exec, cmd, contract.RequestSource{InstanceID: instanceID, StepID: "approve"})
	if err != nil {
		t.Fatal("prepare policy-authorized workflow request:", err)
	}
	change := workflow.StepCommit{Claim: claim, ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: "human-wait-attempt-success", Status: contract.InstanceWaiting, NextStep: "approve", NextWakeAt: &due}
	created, err := workflowStore.CreateHumanWait(ctx, prepared, change)
	if err != nil {
		t.Fatal("atomically create request and workflow wait:", err)
	}
	if created.Source == nil || created.Source.InstanceID != instanceID || created.Source.StepID != "approve" || created.DueAt == nil || !created.DueAt.Equal(due) {
		t.Fatalf("stored request lost workflow source/deadline: %+v", created)
	}
	var status, currentStep string
	var revision, fence int64
	if err := database.QueryRowContext(ctx, `SELECT status,current_step,revision FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(instanceID)).Scan(&status, &currentStep, &revision); err != nil {
		t.Fatal("read committed workflow pause:", err)
	}
	if status != string(contract.InstanceWaiting) || currentStep != "approve" || revision != 2 {
		t.Fatalf("workflow wait status=%s step=%s revision=%d", status, currentStep, revision)
	}
	var persistedRequest, persistedTimer, persistedAttempt, requestEvents int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request WHERE id=? AND source_instance_id=? AND source_step_id=?`, string(created.ID), string(instanceID), "approve").Scan(&persistedRequest); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=? AND due_at=?`, "tenant-a", string(instanceID), "approve", due).Scan(&persistedTimer); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND execution_id=? AND status='succeeded'`, "tenant-a", string(instanceID), change.ExecutionID).Scan(&persistedAttempt); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='request.created' AND aggregate_id=?`, string(created.ID)).Scan(&requestEvents); err != nil {
		t.Fatal(err)
	}
	if persistedRequest != 1 || persistedTimer != 1 || persistedAttempt != 1 || requestEvents != 1 {
		t.Fatalf("durable wait rows request=%d timer=%d attempt=%d outbox=%d", persistedRequest, persistedTimer, persistedAttempt, requestEvents)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_request SET status='resolved',outcome='completed',revision=revision+1,updated_at=CURRENT_TIMESTAMP(6) WHERE tenant_id=? AND id=?`, "tenant-a", string(created.ID)); err != nil {
		t.Fatal("mark request resolved for durable resume test:", err)
	}
	resumed, err := workflowStore.ResumeTerminalHumanWaits(ctx, "tenant-a", 20)
	if err != nil {
		t.Fatal("resume terminal human wait:", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed %d terminal human waits, want 1", resumed)
	}
	if err := database.QueryRowContext(ctx, `SELECT status,current_step,revision,lease_fence FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(instanceID)).Scan(&status, &currentStep, &revision, &fence); err != nil {
		t.Fatal("read resumed workflow:", err)
	}
	if status != string(contract.InstanceReady) || currentStep != "completed" || revision != 3 || fence != 2 {
		t.Fatalf("resumed workflow status=%s step=%s revision=%d fence=%d", status, currentStep, revision, fence)
	}
	resumed, err = workflowStore.ResumeTerminalHumanWaits(ctx, "tenant-a", 20)
	if err != nil || resumed != 0 {
		t.Fatalf("repeat recovery resumed=%d err=%v; want no duplicate advance", resumed, err)
	}
	assertMySQLHumanWaitOutcomes(t, ctx, database, workflowStore, requestService, now)

	expiringInstanceID := contract.InstanceID(contract.NewID())
	if err := insertRunningHumanWaitInstance(ctx, database, expiringInstanceID, "approve", "worker-expiry", 1, now); err != nil {
		t.Fatal("insert expiring wait instance:", err)
	}
	expiringClaim := workflow.Claim{Tenant: "tenant-a", InstanceID: expiringInstanceID, StepID: "approve", Attempt: 1, Owner: "worker-expiry", Fence: 1, Revision: "1"}
	expiringDue := now.Add(time.Hour)
	expiringCommand := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "human-wait-expiry-key"}, Kind: contract.RequestTask,
		Subject: &contract.SubjectRef{Kind: "workflow_instance", ID: string(expiringInstanceID), Revision: "1"},
		Title:   "Expire workflow request", Message: "This request should expire", DueAt: &expiringDue}
	expiringPrepared, err := requestService.PrepareWorkflowSubmit(ctx, exec, expiringCommand, contract.RequestSource{InstanceID: expiringInstanceID, StepID: "approve"})
	if err != nil {
		t.Fatal("prepare expiring workflow request:", err)
	}
	expiringChange := workflow.StepCommit{Claim: expiringClaim, ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: "human-wait-expiry-attempt", Status: contract.InstanceWaiting, NextStep: "approve", NextWakeAt: &expiringDue}
	expiringRequest, err := workflowStore.CreateHumanWait(ctx, expiringPrepared, expiringChange)
	if err != nil {
		t.Fatal("create expiring workflow wait:", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_request SET due_at=DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL 1 SECOND) WHERE tenant_id=? AND id=?`, "tenant-a", string(expiringRequest.ID)); err != nil {
		t.Fatal("make request deadline due:", err)
	}
	expired, err := requestStore.ExpireDue(ctx, "tenant-a", 20)
	if err != nil || expired != 1 {
		t.Fatalf("expire due workflow request count=%d err=%v", expired, err)
	}
	resumed, err = workflowStore.ResumeTerminalHumanWaits(ctx, "tenant-a", 20)
	if err != nil || resumed != 1 {
		t.Fatalf("resume expired workflow request count=%d err=%v", resumed, err)
	}
	if err := database.QueryRowContext(ctx, `SELECT status,current_step FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(expiringInstanceID)).Scan(&status, &currentStep); err != nil {
		t.Fatal("read expiry-resumed workflow:", err)
	}
	if status != string(contract.InstanceReady) || currentStep != "expired" {
		t.Fatalf("expiry-resumed workflow status=%s step=%s", status, currentStep)
	}

	uncertainID := contract.InstanceID(contract.NewID())
	if err := insertUncertainHumanActionInstance(ctx, database, uncertainID, now); err != nil {
		t.Fatal("insert uncertain action instance:", err)
	}
	uncertainExecutionID := contract.NewID()
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_attempt (tenant_id,instance_id,step_id,execution_id,attempt,status,idempotency_key,result_json,failure,started_at,finished_at) VALUES (?,?,?,?,1,'outcome_unknown',?,NULL,'external outcome unknown',?,?)`, "tenant-a", string(uncertainID), "approve", uncertainExecutionID, "stable-uncertain-key", now, now); err != nil {
		t.Fatal("insert uncertain action attempt:", err)
	}
	instanceStore, err := NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal("construct instance store:", err)
	}
	reconcile := workflow.ReconcileCommit{Tenant: "tenant-a", InstanceID: uncertainID, Actor: "operator-1", ExpectedRevision: "7", StepID: "approve", Outcome: contract.ReconcileSucceeded, EvidenceRef: "case:OPS-42", Reason: "Provider confirmed approval was applied", NextStep: "done", Status: contract.InstanceReady, IdempotencyKey: "reconcile-human-action-1"}
	reconciled, err := instanceStore.Reconcile(ctx, reconcile)
	if err != nil {
		t.Fatal("reconcile unknown external result:", err)
	}
	if reconciled.Status != contract.InstanceReady || reconciled.CurrentStep != "done" || reconciled.Revision != "8" {
		t.Fatalf("reconciled instance = %+v", reconciled)
	}
	replayed, err := instanceStore.Reconcile(ctx, reconcile)
	if err != nil || replayed.Revision != reconciled.Revision || replayed.CurrentStep != reconciled.CurrentStep {
		t.Fatalf("reconciliation replay = %+v, %v", replayed, err)
	}
	changedReconcile := reconcile
	changedReconcile.Reason = "different evidence interpretation"
	if _, err := instanceStore.Reconcile(ctx, changedReconcile); err == nil {
		t.Fatal("reconciliation idempotency key accepted changed content")
	}
	var reconcileEvents, reconcileHistory int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='kora.workflow.reconciled' AND aggregate_id=?`, string(uncertainID)).Scan(&reconcileEvents); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='reconciled' AND actor_id=? AND detail LIKE ?`, "tenant-a", string(uncertainID), "operator-1", `%case:OPS-42%`).Scan(&reconcileHistory); err != nil {
		t.Fatal(err)
	}
	if reconcileEvents != 1 || reconcileHistory != 1 {
		t.Fatalf("reconciliation durable event/history counts = %d/%d", reconcileEvents, reconcileHistory)
	}
	var reconcileReceipts, reconcileFence int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_reconcile_receipt WHERE tenant_id=? AND actor_id=? AND idempotency_key=?`, "tenant-a", "operator-1", "reconcile-human-action-1").Scan(&reconcileReceipts); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT lease_fence FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(uncertainID)).Scan(&reconcileFence); err != nil {
		t.Fatal(err)
	}
	if reconcileReceipts != 1 || reconcileFence != 5 {
		t.Fatalf("reconciliation receipt/fence = %d/%d, want 1/5", reconcileReceipts, reconcileFence)
	}
	var reconciledAttemptStatus string
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND execution_id=? AND attempt=1`, "tenant-a", string(uncertainID), uncertainExecutionID).Scan(&reconciledAttemptStatus); err != nil {
		t.Fatal("read reconciled action attempt:", err)
	}
	if reconciledAttemptStatus != "succeeded" {
		t.Fatalf("reconciled action attempt status = %q, want succeeded", reconciledAttemptStatus)
	}
	staleChange := workflow.StepCommit{Claim: workflow.Claim{Tenant: "tenant-a", InstanceID: uncertainID, StepID: "approve", Attempt: 1, Owner: "lost-worker", Fence: 4, Revision: "7"}, ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: "stale-action-key", Status: contract.InstanceReady, NextStep: "done"}
	if err := workflowStore.CommitStep(ctx, staleChange); err == nil {
		t.Fatal("pre-reconciliation worker committed after the operator transition")
	}

	// The request write happens first inside CreateHumanWait. A stale workflow
	// fence must roll that request, its receipt and outbox event back as well.
	staleInstanceID := contract.InstanceID(contract.NewID())
	if err := insertRunningHumanWaitInstance(ctx, database, staleInstanceID, "approve", "actual-owner", 2, now); err != nil {
		t.Fatal("insert rollback fixture instance:", err)
	}
	staleDue := now.Add(2 * time.Hour)
	staleCmd := contract.SubmitRequest{Meta: contract.CommandMeta{IdempotencyKey: "human-wait-rollback-key"}, Kind: contract.RequestTask,
		Subject: &contract.SubjectRef{Kind: "workflow_instance", ID: string(staleInstanceID), Revision: "1"},
		Title:   "Rollback check", Message: "This request must roll back", DueAt: &staleDue}
	stalePrepared, err := requestService.PrepareWorkflowSubmit(ctx, exec, staleCmd, contract.RequestSource{InstanceID: staleInstanceID, StepID: "approve"})
	if err != nil {
		t.Fatal("prepare rollback request:", err)
	}
	staleWaitChange := workflow.StepCommit{Claim: workflow.Claim{Tenant: "tenant-a", InstanceID: staleInstanceID, StepID: "approve", Attempt: 1, Owner: "stale-owner", Fence: 1, Revision: "1"},
		ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: "human-wait-attempt-rollback", Status: contract.InstanceWaiting, NextStep: "approve", NextWakeAt: &staleDue}
	if _, err := workflowStore.CreateHumanWait(ctx, stalePrepared, staleWaitChange); err == nil {
		t.Fatal("stale workflow claim committed human request")
	}
	var rolledBackRequests, rolledBackReceipts, rolledBackEvents int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request WHERE source_instance_id=?`, string(staleInstanceID)).Scan(&rolledBackRequests); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_request_idempotency WHERE idempotency_key=?`, "human-wait-rollback-key").Scan(&rolledBackReceipts); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='request.created' AND aggregate_id IN (SELECT id FROM _kora_request WHERE source_instance_id=?)`, string(staleInstanceID)).Scan(&rolledBackEvents); err != nil {
		t.Fatal(err)
	}
	if rolledBackRequests != 0 || rolledBackReceipts != 0 || rolledBackEvents != 0 {
		t.Fatalf("stale fence left partial request state request=%d receipt=%d outbox=%d", rolledBackRequests, rolledBackReceipts, rolledBackEvents)
	}
}

func mysqlHumanWaitDefinition() contract.WorkflowDefinition {
	outcomes := map[contract.ResponseKind]contract.StepID{
		contract.ResponseApprove: "approved", contract.ResponseReject: "rejected",
		contract.ResponseReturn: "returned", contract.ResponseProvide: "provided",
		contract.ResponseAcknowledge: "acknowledged", contract.ResponseComplete: "completed",
	}
	steps := []contract.StepDefinition{{ID: "approve", Kind: contract.StepHuman, Human: &contract.HumanSpec{
		Outcomes: outcomes, OnExpired: "expired", OnCancelled: "cancelled", OnStale: "stale",
	}}}
	for _, step := range []string{"approved", "rejected", "returned", "provided", "acknowledged", "completed", "expired", "cancelled", "stale"} {
		steps = append(steps, contract.StepDefinition{ID: contract.StepID(step), Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: step}})
	}
	return contract.WorkflowDefinition{ID: "human-wait", Version: 1, Entry: "approve", Steps: steps}
}

func assertMySQLHumanWaitOutcomes(t *testing.T, ctx context.Context, database *sql.DB, store *ExecutionStore, requestService *requests.Service, now time.Time) {
	t.Helper()
	cases := []struct {
		name       string
		kind       contract.RequestKind
		response   contract.ResponseKind
		want       contract.StepID
		wantResult contract.RequestOutcome
	}{
		{"approve", contract.RequestApproval, contract.ResponseApprove, "approved", contract.OutcomeApproved},
		{"reject", contract.RequestApproval, contract.ResponseReject, "rejected", contract.OutcomeRejected},
		{"return", contract.RequestApproval, contract.ResponseReturn, "returned", contract.OutcomeReturned},
		{"provide", contract.RequestInformation, contract.ResponseProvide, "provided", contract.OutcomeProvided},
		{"acknowledge", contract.RequestAcknowledgment, contract.ResponseAcknowledge, "acknowledged", contract.OutcomeAcknowledged},
		{"complete", contract.RequestTask, contract.ResponseComplete, "completed", contract.OutcomeCompleted},
	}
	ids := make(map[string]contract.InstanceID, len(cases))
	for _, tc := range cases {
		id := contract.InstanceID(contract.NewID())
		ids[tc.name] = id
		if err := insertRunningHumanWaitInstance(ctx, database, id, "approve", "outcome-worker-"+tc.name, 1, now); err != nil {
			t.Fatalf("insert %s wait instance: %v", tc.name, err)
		}
		due := now.Add(time.Hour)
		cmd := contract.SubmitRequest{
			Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("human-outcome-" + tc.name)},
			Kind: tc.kind, Subject: &contract.SubjectRef{Kind: "workflow_instance", ID: string(id), Revision: "1"},
			Title: "Test " + tc.name + " response", Message: "Exercise the durable human response path", DueAt: &due,
		}
		if tc.kind == contract.RequestApproval {
			cmd.PolicyID = "human-wait-approval"
		}
		if tc.kind == contract.RequestInformation {
			cmd.InformationSchema = &contract.SchemaRef{ID: "human-wait-info", Version: 1}
		}
		prepared, err := requestService.PrepareWorkflowSubmit(ctx, mysqlHumanWaitExecution(t, "workflow-starter", now), cmd,
			contract.RequestSource{InstanceID: id, StepID: "approve"})
		if err != nil {
			t.Fatalf("prepare %s workflow request: %v", tc.name, err)
		}
		claim := workflow.Claim{Tenant: "tenant-a", InstanceID: id, StepID: "approve", Attempt: 1, Owner: "outcome-worker-" + tc.name, Fence: 1, Revision: "1"}
		change := workflow.StepCommit{Claim: claim, ExecutionID: contract.NewID(), Attempt: 1,
			IdempotencyKey: contract.IdempotencyKey("human-outcome-attempt-" + tc.name), Status: contract.InstanceWaiting, NextStep: "approve", NextWakeAt: &due}
		request, err := store.CreateHumanWait(ctx, prepared, change)
		if err != nil {
			t.Fatalf("create %s durable human wait: %v", tc.name, err)
		}
		response := contract.RespondRequest{Meta: contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("human-response-" + tc.name), ExpectedRevision: "1"}, AssignmentID: request.Assignments[0].ID, Response: tc.response, Comment: "Integration test response"}
		if tc.response == contract.ResponseProvide {
			response.Information = &contract.InformationResponse{SchemaID: "human-wait-info", SchemaVersion: 1, Values: json.RawMessage(`{"answer":"provided"}`)}
		}
		receipt, err := requestService.Respond(ctx, mysqlHumanWaitExecution(t, "reviewer-1", now), request.ID, response)
		if err != nil {
			t.Fatalf("respond %s to workflow request: %v", tc.name, err)
		}
		if receipt.Status != contract.RequestResolved || receipt.Outcome == nil || *receipt.Outcome != tc.wantResult {
			t.Fatalf("%s response receipt=%+v; want resolved/%s", tc.name, receipt, tc.wantResult)
		}
	}
	wantCount := uint16(len(cases))
	if got, err := store.ResumeTerminalHumanWaits(ctx, "tenant-a", 20); err != nil || got != wantCount {
		t.Fatalf("resume response outcomes = (%d, %v), want (%d, nil)", got, err, wantCount)
	}
	for _, tc := range cases {
		var status, step string
		var revision, fence int64
		if err := database.QueryRowContext(ctx, `SELECT status,current_step,revision,lease_fence FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(ids[tc.name])).Scan(&status, &step, &revision, &fence); err != nil {
			t.Fatalf("read %s resumed instance: %v", tc.name, err)
		}
		if status != string(contract.InstanceReady) || contract.StepID(step) != tc.want || revision != 3 || fence != 2 {
			t.Errorf("%s resumed instance status=%s step=%s revision=%d fence=%d; want ready/%s/3/2", tc.name, status, step, revision, fence, tc.want)
		}
		var history int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='human_request_resolved'`, "tenant-a", string(ids[tc.name])).Scan(&history); err != nil || history != 1 {
			t.Errorf("%s history count=%d err=%v; want 1", tc.name, history, err)
		}
	}
	if got, err := store.ResumeTerminalHumanWaits(ctx, "tenant-a", 20); err != nil || got != 0 {
		t.Fatalf("replayed response outcomes = (%d, %v), want (0, nil)", got, err)
	}
}

func mysqlHumanWaitExecution(t *testing.T, principal string, now time.Time) policy.Execution {
	t.Helper()
	exec, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: principal, PrincipalType: contract.PrincipalHuman, Site: "tenant.example.test", AuthenticatedAt: now})
	if err != nil {
		t.Fatalf("construct %s verified human request actor: %v", principal, err)
	}
	return exec
}

func requestsqlSchema(dialect db.Dialect, t *testing.T) []string {
	t.Helper()
	statements, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("request schema statements:", err)
	}
	return statements
}

func insertRunningHumanWaitInstance(ctx context.Context, database *sql.DB, id contract.InstanceID, step, owner string, fence uint64, now time.Time) error {
	_, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,lease_owner,lease_fence,lease_until,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,?,?,DATE_ADD(CURRENT_TIMESTAMP(6),INTERVAL 5 MINUTE),?,?)`,
		string(id), "tenant-a", "human-wait", 1, "workflow_instance", string(id), "1", string(contract.InstanceRunning), step, owner, fence, now, now)
	return err
}

func insertUncertainHumanActionInstance(ctx context.Context, database *sql.DB, id contract.InstanceID, now time.Time) error {
	_, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,lease_fence,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(id), "tenant-a", "human-wait", 1, "Invoice", "INV-UNKNOWN", "3", string(contract.InstanceUncertain), "approve", 7, 4, now, now)
	return err
}

func TestMySQLWorkflowSignalBeforeWaitRegistrationIsRecovered(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_workflow_signal_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	for _, statement := range append(db.OutboxTablesMySQL(), mustWorkflowSignalSchema(t, dialect)...) {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create disposable workflow signal schema: %v", err)
		}
	}
	definitions, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow definitions:", err)
	}
	instances, err := NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	tenant := contract.TenantID("signal-" + contract.NewID())
	subject := contract.SubjectRef{Kind: "Invoice", ID: "INV-SIGNAL", Revision: "1"}
	definition := contract.WorkflowDefinition{ID: contract.DefinitionID("flow-" + contract.NewID()), Version: 1, Entry: "wait", Steps: []contract.StepDefinition{
		{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{EventType: "invoice.paid", CorrelationField: "invoice_id", TimeoutSeconds: 30, OnEvent: "paid", OnTimeout: "expired"}},
		{ID: "paid", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "paid"}},
		{ID: "expired", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "expired"}},
	}}
	ref, err := definitions.PublishDefinition(ctx, tenant, definition, "operator-1", contract.IdempotencyKey("publish-"+contract.NewID()), time.Now().UTC())
	if err != nil {
		t.Fatal("publish event wait definition:", err)
	}
	instanceID := contract.InstanceID(contract.NewID())
	if _, err := instances.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: subject, Actor: "operator-1", IdempotencyKey: contract.IdempotencyKey("start-" + contract.NewID()), Site: "tenant-route", Now: time.Now().UTC()}); err != nil {
		t.Fatal("start workflow:", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "invoice.paid", Version: contract.CurrentVersion, Source: "engine", Site: "tenant-route", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"invoice_id":"INV-SIGNAL"}`)}
	if advanced, err := execution.AcceptSignal(ctx, workflow.Signal{Tenant: tenant, Event: event}); err != nil || advanced != 0 {
		t.Fatalf("pre-registration AcceptSignal = (%d, %v), want durable inbox only", advanced, err)
	}
	if advanced, err := execution.AcceptSignal(ctx, workflow.Signal{Tenant: tenant, Event: event}); err != nil || advanced != 0 {
		t.Fatalf("duplicate pre-registration AcceptSignal = (%d, %v), want idempotent inbox replay", advanced, err)
	}
	claims, err := execution.ClaimDue(ctx, tenant, "worker-1", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim wait step = (%d, %v)", len(claims), err)
	}
	claim := claims[0]
	if err := execution.CommitStep(ctx, workflow.StepCommit{Claim: claim, ExecutionID: contract.NewID(), Attempt: claim.Attempt,
		IdempotencyKey: contract.IdempotencyKey("wait-" + contract.NewID()), Status: contract.InstanceWaiting, NextStep: claim.StepID,
		NextWakeAt: ptrTime(time.Now().UTC().Add(30 * time.Second)), EventWait: &workflow.EventWaitSubscription{EventType: event.Type, CorrelationField: "invoice_id", CorrelationValue: subject.ID, OnEvent: "paid"}}); err != nil {
		t.Fatal("register wait and consume prior signal:", err)
	}
	instance, err := instances.GetInstance(ctx, tenant, instanceID)
	if err != nil {
		t.Fatal("read resumed workflow:", err)
	}
	if instance.Status != contract.InstanceReady || instance.CurrentStep != "paid" || instance.Revision != "3" {
		t.Fatalf("pre-registration signal did not resume once: %+v", instance)
	}
}

func TestMySQLWorkflowSignalInboxCapacitySerializesConcurrentAccepts(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL server:", err)
	}
	if err := root.Ping(); err != nil {
		_ = root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_signal_capacity_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(32)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	definitionStore, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal("construct definition store:", err)
	}
	if err := definitionStore.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	for i := int64(0); i < maxWorkflowSignalInboxRows-1; i++ {
		event := contract.EventEnvelope{ID: fmt.Sprintf("seed-%03d", i), Type: "test.signal", Version: 1, Source: "test", Site: "tenant-route", OccurredAt: stamp, Data: json.RawMessage(`{"subject":"unused"}`)}
		body, err := canonicalSignalEnvelope(event)
		if err != nil {
			t.Fatal("encode seeded signal:", err)
		}
		if _, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_signal (tenant_id,event_id,event_type,correlation_value,payload_json,received_at) VALUES (?,?,?,?,?,?)`, "tenant-capacity", event.ID, event.Type, "", string(body), stamp); err != nil {
			t.Fatalf("seed inbox row %d: %v", i, err)
		}
	}
	store, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal("construct workflow signal store:", err)
	}
	const contenders = 20
	type result struct{ err error }
	results := make(chan result, contenders)
	var started sync.WaitGroup
	started.Add(contenders)
	for i := 0; i < contenders; i++ {
		go func(i int) {
			started.Done()
			event := contract.EventEnvelope{ID: fmt.Sprintf("candidate-%03d", i), Type: "test.signal", Version: 1, Source: "test", Site: "tenant-route", OccurredAt: stamp, Data: json.RawMessage(`{"subject":"unused"}`)}
			_, err := store.AcceptSignal(ctx, workflow.Signal{Tenant: "tenant-capacity", Event: event})
			results <- result{err: err}
		}(i)
	}
	started.Wait()
	accepted, rejected := 0, 0
	for i := 0; i < contenders; i++ {
		res := <-results
		if res.err == nil {
			accepted++
			continue
		}
		var apiErr *contract.Error
		if !errors.As(res.err, &apiErr) || apiErr.Type != contract.CodeResourceExhausted {
			t.Fatalf("concurrent signal acceptance failed unexpectedly: %v", res.err)
		}
		rejected++
	}
	if accepted != 1 || rejected != contenders-1 {
		t.Fatalf("at capacity accepted=%d rejected=%d; want 1 and %d", accepted, rejected, contenders-1)
	}
	var count int64
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_signal WHERE tenant_id=?`, "tenant-capacity").Scan(&count); err != nil {
		t.Fatal("count signal inbox rows:", err)
	}
	if count != maxWorkflowSignalInboxRows {
		t.Fatalf("inbox row count=%d, exceeds or misses bound %d", count, maxWorkflowSignalInboxRows)
	}
}

func mustWorkflowSignalSchema(t *testing.T, dialect db.Dialect) []string {
	t.Helper()
	statements, err := ExecutionSchemaStatements(dialect)
	if err != nil {
		t.Fatal("workflow signal schema:", err)
	}
	return statements
}

func ptrTime(value time.Time) *time.Time { return &value }
