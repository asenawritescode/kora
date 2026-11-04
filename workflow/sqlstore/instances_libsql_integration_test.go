//go:build integration

package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
	requestsqlstore "github.com/asenawritescode/kora/requests/sqlstore"
	"github.com/asenawritescode/kora/workflow"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestLibSQLWorkflowStartAndClaim exercises the real LibSQL HTTP/local
// transport when an operator supplies an isolated disposable database.
func TestLibSQLWorkflowStartAndClaim(t *testing.T) {
	dsn := os.Getenv("KORA_WORKFLOW_DISPOSABLE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_WORKFLOW_DISPOSABLE_LIBSQL_DSN to an isolated disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatal("ping disposable LibSQL:", err)
	}
	dialect := db.Resolve("libsql")
	requestSchema, err := requestsqlstore.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	for _, statement := range requestSchema {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("ensure request schema:", err)
		}
	}
	defs, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := defs.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schema:", err)
	}
	for _, statement := range db.OutboxTablesLibSQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("ensure disposable outbox schema:", err)
		}
	}
	tenant := contract.TenantID("workflow-test-" + contract.NewID())
	definitionID := contract.DefinitionID("definition-" + contract.NewID())
	instanceID := contract.InstanceID(contract.NewID())
	abandonedID := contract.InstanceID(contract.NewID())
	timedWaitID := contract.InstanceID(contract.NewID())
	t.Cleanup(func() {
		for _, table := range []string{"_kora_workflow_attempt", "_kora_workflow_process_timer", "_kora_workflow_signal", "_kora_workflow_subscription", "_kora_workflow_signal_receipt", "_kora_workflow_history", "_kora_workflow_start_receipt", "_kora_workflow_reconcile_receipt", "_kora_workflow_cancel_receipt", "_kora_workflow_instance", "_kora_workflow_publish_receipt", "_kora_workflow_definition"} {
			_, _ = database.ExecContext(context.Background(), "DELETE FROM "+table+" WHERE tenant_id = ?", string(tenant))
		}
		_, _ = database.ExecContext(context.Background(), `DELETE FROM _kora_outbox WHERE tenant_id = ?`, string(tenant))
	})
	now := time.Now().UTC().Truncate(time.Second)
	definition := contract.WorkflowDefinition{ID: definitionID, Version: 1, Entry: "end", Steps: []contract.StepDefinition{{ID: "end", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "complete"}}}}
	ref, err := defs.PublishDefinition(ctx, tenant, definition, "user-test", contract.IdempotencyKey("publish-"+contract.NewID()), now)
	if err != nil {
		t.Fatal("publish definition:", err)
	}
	instances, err := NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	businessScope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-accounts-receivable"}
	started, err := instances.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, BusinessScope: businessScope, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-TEST", Revision: "1"}, Actor: "user-test", IdempotencyKey: contract.IdempotencyKey("start-" + contract.NewID()), Site: "tenant-route", Now: now})
	if err != nil {
		t.Fatal("start instance:", err)
	}
	if started.ID != instanceID || started.Status != contract.InstanceReady || started.CreatedAt.IsZero() || started.BusinessScope != businessScope {
		t.Fatalf("unexpected LibSQL workflow instance: %+v", started)
	}
	executor, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := executor.ClaimDue(ctx, tenant, "libsql-worker", 1, 5*time.Second)
	if err != nil {
		t.Fatal("claim due workflow:", err)
	}
	if len(claims) != 1 || claims[0].InstanceID != instanceID || claims[0].BusinessScope != businessScope {
		t.Fatalf("LibSQL claims = %+v", claims)
	}
	if claims[0].Attempt != 1 {
		t.Fatalf("LibSQL initial claim attempt = %d, want 1", claims[0].Attempt)
	}
	staleClaim := claims[0]
	staleClaim.Fence++
	staleTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin stale LibSQL workflow transaction:", err)
	}
	if err := executor.ValidateClaimInTx(ctx, staleTx, staleClaim); err == nil {
		_ = staleTx.Rollback()
		t.Fatal("LibSQL local action accepted a stale fence")
	}
	_ = staleTx.Rollback()
	localTx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin transaction-bound LibSQL workflow commit:", err)
	}
	if err := executor.ValidateClaimInTx(ctx, localTx, claims[0]); err != nil {
		_ = localTx.Rollback()
		t.Fatal("validate LibSQL workflow claim in transaction:", err)
	}
	if err := executor.CommitStepInTx(ctx, localTx, workflow.StepCommit{Claim: claims[0], ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: contract.IdempotencyKey("action-" + contract.NewID()), Status: contract.InstanceCompleted}); err != nil {
		_ = localTx.Rollback()
		t.Fatal("stage transaction-bound LibSQL workflow commit:", err)
	}
	if err := localTx.Commit(); err != nil {
		t.Fatal("commit transaction-bound LibSQL workflow:", err)
	}
	loaded, err := instances.GetInstance(ctx, tenant, instanceID)
	if err != nil {
		t.Fatal("read completed instance:", err)
	}
	if loaded.Status != contract.InstanceCompleted || loaded.Revision != "2" {
		t.Fatalf("LibSQL workflow status/revision = %s/%s", loaded.Status, loaded.Revision)
	}

	// A generic timeout wait is recovered from its durable timer row and advances
	// the pinned workflow to the declared timeout edge using the LibSQL transport.
	timedDefinition := contract.WorkflowDefinition{ID: contract.DefinitionID("definition-" + contract.NewID()), Version: 1, Entry: "pause", Steps: []contract.StepDefinition{
		{ID: "pause", Kind: contract.StepWait, Wait: &contract.WaitSpec{TimeoutSeconds: 1, OnTimeout: "timeout"}},
		{ID: "timeout", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "timed_out"}},
	}}
	timedRef, err := defs.PublishDefinition(ctx, tenant, timedDefinition, "user-test", contract.IdempotencyKey("publish-"+contract.NewID()), now)
	if err != nil {
		t.Fatal("publish timed wait definition:", err)
	}
	if _, err := instances.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, ID: timedWaitID, Definition: timedDefinition, DefinitionDigest: timedRef.Digest, Subject: contract.SubjectRef{Kind: "Invoice", ID: "INV-TIMER", Revision: "1"}, Actor: "user-test", IdempotencyKey: contract.IdempotencyKey("start-" + contract.NewID()), Site: "tenant-route", Now: now}); err != nil {
		t.Fatal("start timed wait workflow:", err)
	}
	timedClaims, err := executor.ClaimDue(ctx, tenant, "libsql-timer-worker", 1, 5*time.Second)
	if err != nil || len(timedClaims) != 1 || timedClaims[0].InstanceID != timedWaitID {
		t.Fatalf("claim timed wait = %+v, %v", timedClaims, err)
	}
	due := time.Now().UTC().Add(-time.Second)
	if err := executor.CommitStep(ctx, workflow.StepCommit{Claim: timedClaims[0], ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: contract.IdempotencyKey("wait-" + contract.NewID()), Status: contract.InstanceWaiting, NextStep: "pause", NextWakeAt: &due}); err != nil {
		t.Fatal("persist timed wait:", err)
	}
	resumed, err := executor.ResumeDueTimedWaits(ctx, tenant, 10)
	if err != nil || resumed != 1 {
		t.Fatalf("resume due timed wait = %d, %v", resumed, err)
	}
	resumedInstance, err := instances.GetInstance(ctx, tenant, timedWaitID)
	if err != nil || resumedInstance.Status != contract.InstanceReady || resumedInstance.CurrentStep != "timeout" || resumedInstance.Revision != "3" {
		t.Fatalf("timed wait continuation = %+v, %v", resumedInstance, err)
	}
	timeoutClaims, err := executor.ClaimDue(ctx, tenant, "libsql-timeout-worker", 1, 5*time.Second)
	if err != nil || len(timeoutClaims) != 1 || timeoutClaims[0].InstanceID != timedWaitID || timeoutClaims[0].StepID != "timeout" {
		t.Fatalf("claim timed wait timeout edge = %+v, %v", timeoutClaims, err)
	}
	if err := executor.CommitStep(ctx, workflow.StepCommit{Claim: timeoutClaims[0], ExecutionID: contract.NewID(), Attempt: 1, IdempotencyKey: contract.IdempotencyKey("timeout-end-" + contract.NewID()), Status: contract.InstanceCompleted}); err != nil {
		t.Fatal("complete timed wait timeout edge:", err)
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_id = ? AND event_type = 'kora.workflow.started'`, string(instanceID)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("start event count=%d", count)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, string(abandonedID), string(tenant), string(definitionID), 1, "Invoice", "INV-UNCERTAIN", "1", "ready", "charge", 1, now, now); err != nil {
		t.Fatal("insert abandoned workflow fixture:", err)
	}
	abandonedClaim, err := executor.ClaimDue(ctx, tenant, "libsql-external-worker", 1, 5*time.Second)
	if err != nil || len(abandonedClaim) != 1 || abandonedClaim[0].InstanceID != abandonedID {
		t.Fatalf("LibSQL external claim = %+v, %v", abandonedClaim, err)
	}
	executionID := contract.NewOperationID()
	key := contract.IdempotencyKey("external-" + contract.NewID())
	if _, err := executor.BeginExternalAttempt(ctx, abandonedClaim[0], executionID, key, nil); err != nil {
		t.Fatal("persist LibSQL external attempt:", err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_workflow_instance SET lease_until=datetime('now','-1 second') WHERE tenant_id=? AND id=?`, string(tenant), string(abandonedID)); err != nil {
		t.Fatal("expire LibSQL external lease:", err)
	}
	recoveredClaims, err := executor.ClaimDue(ctx, tenant, "libsql-recovery-worker", 1, 5*time.Second)
	if err != nil || len(recoveredClaims) != 0 {
		t.Fatalf("LibSQL recovery returned replacement claim %+v, %v", recoveredClaims, err)
	}
	var recoveredStatus, attemptStatus string
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, string(tenant), string(abandonedID)).Scan(&recoveredStatus); err != nil {
		t.Fatal("read LibSQL recovered instance:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND execution_id=?`, string(tenant), string(abandonedID), executionID).Scan(&attemptStatus); err != nil {
		t.Fatal("read LibSQL recovered attempt:", err)
	}
	if recoveredStatus != string(contract.InstanceUncertain) || attemptStatus != "outcome_unknown" {
		t.Fatalf("LibSQL recovered state instance=%s attempt=%s", recoveredStatus, attemptStatus)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_id=? AND event_type='kora.workflow.outcome_unknown'`, string(abandonedID)).Scan(&count); err != nil {
		t.Fatal("read LibSQL recovery event:", err)
	}
	if count != 1 {
		t.Fatalf("LibSQL recovery event count=%d, want 1", count)
	}
	uncertain, err := instances.GetInstance(ctx, tenant, abandonedID)
	if err != nil || uncertain.Status != contract.InstanceUncertain || uncertain.CurrentStep != "charge" {
		t.Fatalf("read uncertain instance before operator reconciliation = %+v, %v", uncertain, err)
	}
	reconcile := workflow.ReconcileCommit{
		Tenant: tenant, InstanceID: abandonedID, Actor: "operator-test", ExpectedRevision: uncertain.Revision,
		StepID: "charge", Outcome: contract.ReconcileSucceeded, EvidenceRef: "case:LIBSQL-42",
		Reason: "provider audit confirms the effect was applied", NextStep: "done", Status: contract.InstanceReady,
		IdempotencyKey: contract.IdempotencyKey("reconcile-" + contract.NewID()),
	}
	reconciled, err := instances.Reconcile(ctx, reconcile)
	if err != nil || reconciled.Status != contract.InstanceReady || reconciled.CurrentStep != "done" {
		t.Fatalf("reconcile LibSQL uncertain workflow = %+v, %v", reconciled, err)
	}
	replayed, err := instances.Reconcile(ctx, reconcile)
	if err != nil || replayed.Revision != reconciled.Revision {
		t.Fatalf("replay LibSQL reconciliation = %+v, %v", replayed, err)
	}
	var reconciliationEvents, reconciliationHistory int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_id=? AND event_type='kora.workflow.reconciled'`, string(abandonedID)).Scan(&reconciliationEvents); err != nil {
		t.Fatal("read LibSQL reconciliation event:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='reconciled' AND actor_id=?`, string(tenant), string(abandonedID), "operator-test").Scan(&reconciliationHistory); err != nil {
		t.Fatal("read LibSQL reconciliation history:", err)
	}
	if reconciliationEvents != 1 || reconciliationHistory != 1 {
		t.Fatalf("LibSQL reconciliation event/history = %d/%d, want 1/1", reconciliationEvents, reconciliationHistory)
	}
	finishClaim, err := executor.ClaimDue(ctx, tenant, "libsql-reconciliation-finish", 1, 5*time.Second)
	if err != nil || len(finishClaim) != 1 || finishClaim[0].InstanceID != abandonedID || finishClaim[0].StepID != "done" {
		t.Fatalf("claim reconciled success continuation = %+v, %v", finishClaim, err)
	}
	if err := executor.CommitStep(ctx, workflow.StepCommit{Claim: finishClaim[0], ExecutionID: contract.NewOperationID(), Attempt: finishClaim[0].Attempt, IdempotencyKey: contract.IdempotencyKey("finish-reconciled-" + contract.NewID()), Status: contract.InstanceCompleted}); err != nil {
		t.Fatal("complete reconciled success continuation:", err)
	}
	verifyExternalHTTPReconciliation(ctx, t, database, tenant, definitionID, executor, instances)
}

type libSQLWorkflowSchemas struct{}

func (libSQLWorkflowSchemas) Registered(ref contract.SchemaRef) bool {
	return (ref.ID == "input" || ref.ID == "output") && ref.Version == 1
}

func (libSQLWorkflowSchemas) Validate(ref contract.SchemaRef, data json.RawMessage) error {
	if !json.Valid(data) {
		return errors.New("invalid JSON")
	}
	return nil
}

type libSQLResponseLossAction struct {
	client   *http.Client
	endpoint string
}

func (a libSQLResponseLossAction) Execute(ctx context.Context, action workflow.ActionContext, input workflow.ActionInput) (workflow.ActionResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(input.Data))
	if err != nil {
		return workflow.ActionResult{}, err
	}
	request.Header.Set("Idempotency-Key", string(action.IdempotencyKey))
	response, err := a.client.Do(request)
	if err != nil {
		return workflow.ActionResult{}, err
	}
	defer response.Body.Close()
	return workflow.ActionResult{}, errors.New("provider unexpectedly returned a response")
}

func verifyExternalHTTPReconciliation(ctx context.Context, t *testing.T, database *sql.DB, tenant contract.TenantID, definitionID contract.DefinitionID, store *ExecutionStore, instances *InstanceStore) {
	t.Helper()
	instanceID := contract.InstanceID(contract.NewID())
	stepID := contract.StepID("http-effect")
	now := time.Now().UTC()
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, string(instanceID), string(tenant), string(definitionID), 1, "Invoice", "INV-HTTP", "1", string(contract.InstanceReady), string(stepID), 1, now, now); err != nil {
		t.Fatal("insert controlled HTTP workflow fixture:", err)
	}
	claims, err := store.ClaimDue(ctx, tenant, "libsql-http-worker", 1, 5*time.Second)
	if err != nil || len(claims) != 1 || claims[0].InstanceID != instanceID {
		t.Fatalf("claim controlled HTTP workflow = %+v, %v", claims, err)
	}
	var providerEffects atomic.Int32
	providerKey := make(chan string, 4)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerEffects.Add(1)
		providerKey <- r.Header.Get("Idempotency-Key")
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("test provider transport does not support hijacking")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("drop controlled provider response: %v", err)
			return
		}
		_ = connection.Close()
	}))
	defer provider.Close()
	grant := policy.Grant{Resource: "Invoice", Operation: "submit"}
	identity, err := workflow.NewTenantServiceIdentityProvider(tenant, contract.ActorContext{PrincipalID: "workflow-runtime", PrincipalType: contract.PrincipalService, Site: "tenant-route", AuthenticatedAt: now}, []policy.Grant{grant})
	if err != nil {
		t.Fatal("construct verified workflow identity:", err)
	}
	actionKey := workflow.ActionKey{Name: "invoice.http-submit", Version: 1}
	inputSchema := contract.SchemaRef{ID: "input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "output", Version: 1}
	action := libSQLResponseLossAction{client: provider.Client(), endpoint: provider.URL}
	registration := workflow.ActionRegistration{Key: actionKey, Class: workflow.ActionExternal, InputSchema: inputSchema, OutputSchema: outputSchema, RequiredGrant: grant, External: action}
	definition := contract.WorkflowDefinition{ID: definitionID, Version: 1}
	step := contract.StepDefinition{ID: stepID, Kind: contract.StepAction, Action: &contract.ActionSpec{Name: actionKey.Name, Version: actionKey.Version, InputSchema: inputSchema.ID, Input: json.RawMessage(`{"order":"INV-HTTP"}`), Next: "done"}}
	actionExecutor, err := workflow.NewExternalActionExecutor(store, identity, libSQLWorkflowSchemas{}, nil)
	if err != nil {
		t.Fatal("construct external workflow executor:", err)
	}
	change, err := actionExecutor.Execute(ctx, claims[0], definition, step, registration)
	if err != nil {
		t.Fatal("execute controlled external workflow action:", err)
	}
	if change.Status != contract.InstanceUncertain || len(change.Events) != 1 || change.Events[0].Type != "kora.workflow.outcome_unknown" {
		t.Fatalf("lost provider reply transition = %+v", change)
	}
	if err := store.CommitStep(ctx, change); err != nil {
		t.Fatal("persist uncertain workflow action:", err)
	}
	key := <-providerKey
	if providerEffects.Load() != 1 || key == "" || key != string(change.IdempotencyKey) {
		t.Fatalf("provider effects/key = %d/%q, workflow idempotency key=%q", providerEffects.Load(), key, change.IdempotencyKey)
	}
	uncertain, err := instances.GetInstance(ctx, tenant, instanceID)
	if err != nil || uncertain.Status != contract.InstanceUncertain || uncertain.Revision != "2" {
		t.Fatalf("persisted provider uncertainty = %+v, %v", uncertain, err)
	}
	reconcile := workflow.ReconcileCommit{Tenant: tenant, InstanceID: instanceID, Actor: "operator-http", ExpectedRevision: uncertain.Revision, StepID: stepID, Outcome: contract.ReconcileSucceeded, EvidenceRef: "case:HTTP-UNCERTAIN", Reason: "provider audit confirms the accepted effect", NextStep: "done", Status: contract.InstanceReady, IdempotencyKey: contract.IdempotencyKey("reconcile-http-" + contract.NewID())}
	resolved, err := instances.Reconcile(ctx, reconcile)
	if err != nil || resolved.Status != contract.InstanceReady || resolved.CurrentStep != "done" {
		t.Fatalf("resolve HTTP uncertain effect = %+v, %v", resolved, err)
	}
	replayed, err := instances.Reconcile(ctx, reconcile)
	if err != nil || replayed.Revision != resolved.Revision || providerEffects.Load() != 1 {
		t.Fatalf("replay did not preserve one provider effect and original resolution: %+v, %v", replayed, err)
	}
}
