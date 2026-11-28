//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	requestsql "github.com/asenawritescode/kora/internal/requests/sqlstore"
	"github.com/asenawritescode/kora/internal/workflow"
	workflowsql "github.com/asenawritescode/kora/internal/workflow/sqlstore"
	"github.com/go-sql-driver/mysql"
)

type noExpiredRequests struct{}

func (noExpiredRequests) ExpireDue(context.Context, contract.TenantID, uint16) (uint16, error) {
	return 0, nil
}

// TestWorkflowEventSignalResumesWaitThroughRecoveryWorker records a signal while
// the waiting instance row is locked by another transaction. The acceptance pass
// skips the locked instance, so the signal stays pending. The engine's human-wait
// recovery worker must then resume the instance exactly once.
func TestWorkflowEventSignalResumesWaitThroughRecoveryWorker(t *testing.T) {
	cases := []struct{ driver, key string }{
		{"mysql", "KORA_ENGINE_WORKFLOW_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_ENGINE_WORKFLOW_DISPOSABLE_POSTGRES_DSN"},
	}
	ran := false
	for _, tc := range cases {
		dsn := os.Getenv(tc.key)
		if dsn == "" {
			continue
		}
		ran = true
		t.Run(tc.driver, func(t *testing.T) {
			runEventSignalRecoveryCase(t, tc.driver, dsn)
		})
	}
	if !ran {
		t.Skip("set KORA_ENGINE_WORKFLOW_DISPOSABLE_{MYSQL,POSTGRES}_DSN to disposable servers")
	}
}

func runEventSignalRecoveryCase(t *testing.T, driver, dsn string) {
	ctx := t.Context()
	if driver == "mysql" {
		// Instance reads scan TIMESTAMP columns, which the driver returns as bytes without parseTime.
		config, err := mysql.ParseDSN(dsn)
		if err != nil {
			t.Fatal("parse MySQL DSN:", err)
		}
		config.ParseTime = true
		dsn = config.FormatDSN()
	}
	database := disposabledb.Open(t, driver, dsn).DB
	dialect := db.Resolve(driver)
	outboxTables := db.OutboxTablesPostgres()
	if driver == "mysql" {
		outboxTables = db.OutboxTablesMySQL()
	}
	schema, err := workflowsql.ExecutionSchemaStatements(dialect)
	if err != nil {
		t.Fatal("build workflow schema:", err)
	}
	// The recovery pass also selects request state, so the request tables must exist.
	requestSchema, err := requestsql.SchemaStatements(dialect)
	if err != nil {
		t.Fatal("build request schema:", err)
	}
	statements := append(append(outboxTables, schema...), requestSchema...)
	for _, statement := range statements {
		if _, err = database.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create workflow schema: %v\n%s", err, statement)
		}
	}
	definitions, err := workflowsql.NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err = definitions.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow definitions:", err)
	}
	instances, err := workflowsql.NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := workflowsql.NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}

	tenant := contract.TenantID("signal-" + contract.NewID())
	subject := contract.SubjectRef{Kind: "Invoice", ID: "INV-SIGNAL", Revision: "1"}
	definition := contract.WorkflowDefinition{ID: contract.DefinitionID("flow-" + contract.NewID()), Version: 1, Entry: "wait", Steps: []contract.StepDefinition{
		{ID: "wait", Kind: contract.StepWait, Wait: &contract.WaitSpec{EventType: "invoice.paid", CorrelationField: "invoice_id", TimeoutSeconds: 300, OnEvent: "paid", OnTimeout: "expired"}},
		{ID: "paid", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "paid"}},
		{ID: "expired", Kind: contract.StepEnd, End: &contract.EndSpec{Outcome: "expired"}},
	}}
	ref, err := definitions.PublishDefinition(ctx, tenant, definition, "operator-1", contract.IdempotencyKey("publish-"+contract.NewID()), time.Now().UTC())
	if err != nil {
		t.Fatal("publish event wait definition:", err)
	}
	instanceID := contract.InstanceID(contract.NewID())
	if _, err = instances.StartInstance(ctx, workflow.StartInstanceInput{Tenant: tenant, ID: instanceID, Definition: definition, DefinitionDigest: ref.Digest, Subject: subject, Actor: "operator-1", IdempotencyKey: contract.IdempotencyKey("start-" + contract.NewID()), Site: "tenant-route", Now: time.Now().UTC()}); err != nil {
		t.Fatal("start workflow:", err)
	}
	claims, err := execution.ClaimDue(ctx, tenant, "worker-1", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim wait step = (%d, %v)", len(claims), err)
	}
	wakeAt := time.Now().UTC().Add(5 * time.Minute)
	if err = execution.CommitStep(ctx, workflow.StepCommit{Claim: claims[0], ExecutionID: contract.NewID(), Attempt: claims[0].Attempt,
		IdempotencyKey: contract.IdempotencyKey("wait-" + contract.NewID()), Status: contract.InstanceWaiting, NextStep: claims[0].StepID,
		NextWakeAt: &wakeAt, EventWait: &workflow.EventWaitSubscription{EventType: "invoice.paid", CorrelationField: "invoice_id", CorrelationValue: subject.ID, OnEvent: "paid"}}); err != nil {
		t.Fatal("register event wait:", err)
	}
	waiting := mustEngineInstance(t, instances, tenant, instanceID)

	// Hold the instance row lock so the acceptance pass skips it and the signal stays pending.
	locker, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin instance lock:", err)
	}
	defer func() { _ = locker.Rollback() }()
	var lockedID string
	if err = locker.QueryRowContext(ctx, db.Rebind(dialect, `SELECT id FROM _kora_workflow_instance WHERE tenant_id=? AND id=? FOR UPDATE`), string(tenant), string(instanceID)).Scan(&lockedID); err != nil {
		t.Fatal("lock waiting instance:", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "invoice.paid", Version: contract.CurrentVersion, Source: "engine", Site: "tenant-route", OccurredAt: time.Now().UTC(), Data: json.RawMessage(`{"invoice_id":"INV-SIGNAL"}`)}
	advanced, err := execution.AcceptSignal(ctx, workflow.Signal{Tenant: tenant, Event: event})
	if err != nil || advanced != 0 {
		t.Fatalf("AcceptSignal under lock = (%d, %v), want pending signal and no advance", advanced, err)
	}
	if err = locker.Rollback(); err != nil {
		t.Fatal("release instance lock:", err)
	}
	if got := mustEngineInstance(t, instances, tenant, instanceID); got.Status != contract.InstanceWaiting || got.Revision != waiting.Revision {
		t.Fatalf("signal advanced the locked instance before recovery: %+v", got)
	}

	// The engine's tenant recovery worker must find the pending pair and resume it.
	store := tenantHumanWaitRecoveryStore{requests: noExpiredRequests{}, workflows: execution}
	worker, err := workflow.NewHumanWaitRecoveryWorker(store, tenant, 25, 100*time.Millisecond, func(passErr error) {
		t.Errorf("recovery pass failed: %v", passErr)
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(runCtx)
	}()
	defer func() { stop(); <-done }()
	ready := waitForEngineInstanceStatus(t, instances, tenant, instanceID, contract.InstanceReady)
	if ready.CurrentStep != "paid" || ready.Revision == waiting.Revision {
		t.Fatalf("recovered instance = %+v, want step paid at a new revision", ready)
	}

	// Several more passes run; the resumed instance must not advance again.
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	<-done
	if got := mustEngineInstance(t, instances, tenant, instanceID); got.Revision != ready.Revision || got.CurrentStep != "paid" {
		t.Fatalf("instance changed after recovery settled: %+v, want revision %s", got, ready.Revision)
	}
	advanced, err = execution.AcceptSignal(ctx, workflow.Signal{Tenant: tenant, Event: event})
	if err != nil || advanced != 0 {
		t.Fatalf("duplicate AcceptSignal after resume = (%d, %v), want idempotent no-op", advanced, err)
	}
	if n := countEngineRows(t, database, dialect, `SELECT COUNT(*) FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=? AND kind='signal'`, string(tenant), string(instanceID)); n != 1 {
		t.Fatalf("signal history rows = %d, want exactly 1", n)
	}
	if n := countEngineRows(t, database, dialect, `SELECT COUNT(*) FROM _kora_workflow_signal_receipt WHERE tenant_id=? AND instance_id=? AND event_id=?`, string(tenant), string(instanceID), event.ID); n != 1 {
		t.Fatalf("signal receipts = %d, want exactly 1", n)
	}
}

func mustEngineInstance(t *testing.T, instances *workflowsql.InstanceStore, tenant contract.TenantID, id contract.InstanceID) contract.WorkflowInstance {
	t.Helper()
	instance, err := instances.GetInstance(t.Context(), tenant, id)
	if err != nil {
		t.Fatal("read workflow instance:", err)
	}
	return instance
}

func waitForEngineInstanceStatus(t *testing.T, instances *workflowsql.InstanceStore, tenant contract.TenantID, id contract.InstanceID, status contract.InstanceStatus) contract.WorkflowInstance {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		instance := mustEngineInstance(t, instances, tenant, id)
		if instance.Status == status {
			return instance
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s status = %s after 10s, want %s", id, instance.Status, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countEngineRows(t *testing.T, database *sql.DB, dialect db.Dialect, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRowContext(t.Context(), db.Rebind(dialect, query), args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}
