package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

func TestExecutionSchemaStatementsUseTenantFencesAndDurableQueues(t *testing.T) {
	for name, dialect := range map[string]db.Dialect{"mysql": &db.MySQLDialect{}, "postgres": &db.PostgresDialect{}, "libsql": &db.LibSQLDialect{}} {
		statements, err := ExecutionSchemaStatements(dialect)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		joined := ""
		for _, statement := range statements {
			joined += statement + "\n"
		}
		for _, fragment := range []string{"_kora_workflow_instance", "lease_fence", "_kora_workflow_attempt", "_kora_workflow_process_timer", "_kora_workflow_signal", "_kora_workflow_signal_capacity", "_kora_workflow_subscription", "_kora_workflow_signal_receipt", "_kora_workflow_history", "_kora_workflow_reconcile_receipt", "payload_json", "command_digest", "result_json", "tenant_id"} {
			if !containsAll(joined, fragment) {
				t.Errorf("%s schema missing %s", name, fragment)
			}
		}
	}
}

func TestParseLibSQLTimestampRepresentations(t *testing.T) {
	for _, raw := range []string{
		"2026-10-16 14:08:19+00:00",
		"2026-10-16 14:08:19.123456+03:00",
		"2026-10-16 14:08:19",
		"2026-10-16T14:08:19.123Z",
	} {
		parsed, err := parseTimeText(raw)
		if err != nil {
			t.Errorf("parse %q: %v", raw, err)
			continue
		}
		if parsed.Location() != time.UTC {
			t.Errorf("parsed %q location = %s, want UTC", raw, parsed.Location())
		}
	}
}

func TestCommitStepRejectsStaleFenceAndRollsBack(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	change := workflow.StepCommit{Claim: claim, ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 1, IdempotencyKey: "workflow-action-key-1", Status: contract.InstanceCompleted}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("completed", "approve", nil, "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := store.CommitStep(context.Background(), change); err == nil {
		t.Fatal("stale claim committed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitStepInTxLeavesTransactionOwnershipToCaller(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	change := workflow.StepCommit{Claim: claim, ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 1, IdempotencyKey: "workflow-action-key-1", Status: contract.InstanceCompleted}
	mock.ExpectBegin()
	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("completed", "approve", nil, "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := store.CommitStepInTx(context.Background(), tx, change); err == nil {
		t.Fatal("stale claim committed")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("caller could not roll back failed composed workflow step: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitStepWritesAttemptProgressAndTimerAtomically(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	wake := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	change := workflow.StepCommit{Claim: claim, ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 1, IdempotencyKey: "workflow-action-key-1", Status: contract.InstanceWaiting, NextStep: "wait", NextWakeAt: &wake, Result: workflow.ActionResult{Schema: contract.SchemaRef{ID: "output", Version: 1}, Data: []byte(`{"ok":true}`)}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("waiting", "wait", wake, "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), string(claim.StepID)).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_attempt (tenant_id, instance_id, step_id, execution_id, attempt, status, idempotency_key, result_json, failure, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6), CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), "approve", change.ExecutionID, uint8(1), "succeeded", "workflow-action-key-1", `{"schema":{"id":"output","version":1},"data":{"ok":true}}`, "").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_history (tenant_id, instance_id, sequence, step_id, kind, status, actor_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), uint64(2), "approve", "step_committed", "waiting", nil, "").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_process_timer WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_process_timer (tenant_id, instance_id, step_id, generation, due_at) VALUES (?, ?, ?, ?, ?)")).WithArgs("tenant-a", string(claim.InstanceID), "wait", uint64(3), wake).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := store.CommitStep(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitStepRejectsSkippedAttemptNumber(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 2, Owner: "worker-a", Fence: 3, Revision: "4"}
	change := workflow.StepCommit{Claim: claim, ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 2, IdempotencyKey: "workflow-action-key-1", Status: contract.InstanceCompleted}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("completed", "approve", nil, "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), string(claim.StepID)).WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	if err := store.CommitStep(context.Background(), change); err == nil {
		t.Fatal("skipped first workflow attempt was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExternalAttemptIsRecordedBeforeEffectAndFinalizedInStepTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	executionID := "01K7EXECUTION000000000000000000"
	key := contract.IdempotencyKey("workflow-action-key-1")
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6) FOR UPDATE")).WithArgs("tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(string(claim.InstanceID)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status,pinned_dispatch_json FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), "approve").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_attempt (tenant_id,instance_id,step_id,execution_id,attempt,status,idempotency_key,pinned_dispatch_json,result_json,failure,started_at,finished_at) VALUES (?,?,?,?,?,'running',?,?,NULL,'',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), "approve", executionID, uint8(1), string(key), nil).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if _, err := store.BeginExternalAttempt(context.Background(), claim, executionID, key, nil); err != nil {
		t.Fatal("begin external attempt:", err)
	}

	change := workflow.StepCommit{Claim: claim, ExecutionID: executionID, Attempt: 1, AttemptStarted: true, IdempotencyKey: key, Status: contract.InstanceReady, NextStep: "done", Result: workflow.ActionResult{Data: []byte(`{"ok":true}`)}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("ready", "done", nil, "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT idempotency_key,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? AND execution_id = ? AND attempt = ?")).WithArgs("tenant-a", string(claim.InstanceID), "approve", executionID, uint8(1)).WillReturnRows(sqlmock.NewRows([]string{"idempotency_key", "status"}).AddRow(string(key), "running"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_attempt SET status = ?, result_json = ?, failure = ?, finished_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND instance_id = ? AND step_id = ? AND execution_id = ? AND attempt = ? AND status = 'running' AND idempotency_key = ?")).WithArgs("succeeded", sqlmock.AnyArg(), "", "tenant-a", string(claim.InstanceID), "approve", executionID, uint8(1), string(key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_history (tenant_id, instance_id, sequence, step_id, kind, status, actor_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), uint64(2), "approve", "step_committed", "ready", nil, "").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_process_timer WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if err := store.CommitStep(context.Background(), change); err != nil {
		t.Fatal("finalize external attempt:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestExternalAttemptRetainsPinnedProviderAcrossBindingRevision(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: scope, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	executionID := "01K7EXECUTION000000000000000000"
	key := contract.IdempotencyKey("workflow-action-key-1")
	first := contract.PinnedDispatch{ExecutionID: executionID, Scope: scope, CallerModuleID: "sales.orders", ProviderModuleID: "billing", ProviderID: "billing.provider-a", BindingRevision: 8, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "create_obligation", InputSchema: contract.SchemaRef{ID: "input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "output", Version: 1}}, IdempotencyKey: key, InputHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	firstLease := contract.CapabilityLease{ID: "lease-a", TenantID: "tenant-a", Scope: scope, CallerModuleID: "sales.orders", ProviderModuleID: "billing", ProviderID: "billing.provider-a", Operation: "create_obligation", BindingRevision: 8, GrantEpoch: 3, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	firstJSON, _ := json.Marshal(workflow.PinnedCapabilityAttempt{Dispatch: first, Lease: firstLease})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6) FOR UPDATE")).WithArgs("tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(string(claim.InstanceID)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status,pinned_dispatch_json FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), "approve").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_attempt (tenant_id,instance_id,step_id,execution_id,attempt,status,idempotency_key,pinned_dispatch_json,result_json,failure,started_at,finished_at) VALUES (?,?,?,?,?,'running',?,?,NULL,'',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), "approve", executionID, uint8(1), string(key), string(firstJSON)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	stored, err := store.BeginPinnedExternalAttempt(context.Background(), claim, executionID, key, first, firstLease)
	if err != nil || stored == nil || stored.Dispatch.ProviderID != "billing.provider-a" {
		t.Fatalf("initial pinned attempt = (%+v,%v)", stored, err)
	}

	claim.Attempt = 2
	rebound := first
	rebound.ProviderID = "billing.provider-b"
	rebound.BindingRevision = 9
	reboundLease := firstLease
	reboundLease.ID = "lease-b"
	reboundLease.ProviderID = "billing.provider-b"
	reboundLease.BindingRevision = 9
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6) FOR UPDATE")).WithArgs("tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(string(claim.InstanceID)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status,pinned_dispatch_json FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), "approve").WillReturnRows(sqlmock.NewRows([]string{"attempt", "idempotency_key", "execution_id", "status", "pinned_dispatch_json"}).AddRow(1, string(key), executionID, "failed", string(firstJSON)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_attempt (tenant_id,instance_id,step_id,execution_id,attempt,status,idempotency_key,pinned_dispatch_json,result_json,failure,started_at,finished_at) VALUES (?,?,?,?,?,'running',?,?,NULL,'',CURRENT_TIMESTAMP(6),CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), "approve", executionID, uint8(2), string(key), string(firstJSON)).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	stored, err = store.BeginPinnedExternalAttempt(context.Background(), claim, executionID, key, rebound, reboundLease)
	if err != nil || stored == nil || stored.Dispatch.ProviderID != "billing.provider-a" || stored.Dispatch.BindingRevision != 8 || stored.Lease.ID != "lease-a" {
		t.Fatalf("retry pin after rebind = (%+v,%v)", stored, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRenewAndReleaseRequireCurrentLeaseFence(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "approve", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET lease_until = DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND), updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs(time.Second.Microseconds(), "tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 0))
	if _, err := store.Renew(context.Background(), claim, time.Second); err == nil {
		t.Fatal("expired claim renewed")
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = 'ready', lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("tenant-a", string(claim.InstanceID), "approve", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.Release(context.Background(), claim); err == nil {
		t.Fatal("stale claim released")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitStepRegistersEventWaitAndChecksDurableInbox(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store, _ := NewExecutionStore(database, &db.MySQLDialect{})
	claim := workflow.Claim{Tenant: "tenant-a", BusinessScope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, InstanceID: "01K7INSTANCE00000000000000000", StepID: "wait", Attempt: 1, Owner: "worker-a", Fence: 3, Revision: "4"}
	wake := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	change := workflow.StepCommit{Claim: claim, ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 1, IdempotencyKey: "workflow-wait-key-1", Status: contract.InstanceWaiting, NextStep: "wait", NextWakeAt: &wake, EventWait: &workflow.EventWaitSubscription{EventType: "invoice.paid", CorrelationField: "invoice_id", CorrelationValue: "INV-1", OnEvent: "paid"}}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)")).WithArgs("waiting", "wait", wake, "tenant-a", string(claim.InstanceID), "wait", "worker-a", uint64(3), uint64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT attempt,idempotency_key,execution_id,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1")).WithArgs("tenant-a", string(claim.InstanceID), "wait").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec("INSERT INTO _kora_workflow_attempt .*").WithArgs("tenant-a", string(claim.InstanceID), "wait", change.ExecutionID, uint8(1), "succeeded", "workflow-wait-key-1", `{"schema":{"id":"","version":0},"data":null}`, "").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_history (tenant_id, instance_id, sequence, step_id, kind, status, actor_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP(6))")).WithArgs("tenant-a", string(claim.InstanceID), uint64(1), "wait", "step_committed", "waiting", nil, "").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM _kora_workflow_process_timer WHERE tenant_id = ? AND instance_id = ?")).WithArgs("tenant-a", string(claim.InstanceID)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_process_timer (tenant_id, instance_id, step_id, generation, due_at) VALUES (?, ?, ?, ?, ?)")).WithArgs("tenant-a", string(claim.InstanceID), "wait", uint64(3), wake).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO _kora_workflow_subscription (tenant_id,instance_id,step_id,event_type,correlation_field,correlation_value,on_event) VALUES (?,?,?,?,?,?,?)")).WithArgs("tenant-a", string(claim.InstanceID), "wait", "invoice.paid", "invoice_id", "INV-1", "paid").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT s.event_id,s.event_type,s.payload_json,i.business_scope_json,i.id,i.current_step,sub.correlation_field,sub.correlation_value,sub.on_event,i.revision,i.definition_id,i.definition_version .*i.id=\\? .*FOR UPDATE SKIP LOCKED").WithArgs("tenant-a", string(claim.InstanceID), int64(maxWorkflowSignalBatch)).WillReturnRows(sqlmock.NewRows([]string{"event_id", "event_type", "payload_json", "business_scope_json", "instance_id", "step_id", "field", "expected", "on_event", "revision", "definition_id", "definition_version"}))
	mock.ExpectCommit()
	if err := store.CommitStep(context.Background(), change); err != nil {
		t.Fatal("CommitStep:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("event wait registration transaction:", err)
	}
}
