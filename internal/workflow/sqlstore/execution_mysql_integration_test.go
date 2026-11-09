//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLExpiredWorkflowClaimCannotCommitProgress(t *testing.T) {
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
	name := fmt.Sprintf("kora_workflow_exec_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	definitionStore, err := NewDefinitionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	if err := definitionStore.Ensure(ctx); err != nil {
		t.Fatal("ensure workflow schemas:", err)
	}
	for _, statement := range db.OutboxTablesMySQL() {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal("ensure workflow outbox schema:", err)
		}
	}
	store, err := NewExecutionStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	instanceStore, err := NewInstanceStore(database, dialect)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	instanceID := contract.InstanceID("01K7INSTANCE00000000000000000")
	_, err = database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, string(instanceID), "tenant-a", "purchase", 1, "Invoice", "INV-1", "1", "ready", "approve", 1, now, now)
	if err != nil {
		t.Fatal("insert workflow fixture:", err)
	}
	first, err := store.ClaimDue(ctx, "tenant-a", "worker-a", 1, 10*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	if first[0].Attempt != 1 {
		t.Fatalf("first claim attempt = %d, want 1", first[0].Attempt)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_workflow_instance SET lease_until = DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL 1 SECOND) WHERE id = ?`, string(instanceID)); err != nil {
		t.Fatal("expire old lease:", err)
	}
	second, err := store.ClaimDue(ctx, "tenant-a", "worker-b", 1, 10*time.Second)
	if err != nil || len(second) != 1 {
		t.Fatalf("replacement claim = %#v, %v", second, err)
	}
	if second[0].Fence <= first[0].Fence {
		t.Fatalf("fencing token did not advance: old=%d new=%d", first[0].Fence, second[0].Fence)
	}
	if second[0].Attempt != 1 {
		t.Fatalf("replacement claim attempt = %d, want 1 before any commit", second[0].Attempt)
	}
	stale := workflow.StepCommit{Claim: first[0], ExecutionID: "01K7EXECUTION000000000000000000", Attempt: 1, IdempotencyKey: "stable-action-key-01", Status: contract.InstanceCompleted}
	if err := store.CommitStep(ctx, stale); err == nil {
		t.Fatal("expired worker committed workflow progress")
	}
	if _, err := store.Renew(ctx, first[0], 10*time.Second); err == nil {
		t.Fatal("expired worker renewed its lease")
	}
	if err := store.Release(ctx, first[0]); err == nil {
		t.Fatal("expired worker released the replacement claim")
	}
	change := stale
	change.Claim = second[0]
	change.Status = contract.InstanceRetrying
	change.NextStep = "approve"
	change.Failure = "temporary provider error"
	wake := time.Now().UTC().Add(-time.Second)
	change.NextWakeAt = &wake
	if err := store.CommitStep(ctx, change); err != nil {
		t.Fatal("first durable retry attempt:", err)
	}
	third, err := store.ClaimDue(ctx, "tenant-a", "worker-c", 1, 10*time.Second)
	if err != nil || len(third) != 1 {
		t.Fatalf("retry claim = %#v, %v", third, err)
	}
	change.Claim = third[0]
	if third[0].Attempt != 2 {
		t.Fatalf("retry claim attempt = %d, want 2", third[0].Attempt)
	}
	change.Attempt = third[0].Attempt
	change.Status = contract.InstanceCompleted
	change.NextStep = ""
	change.NextWakeAt = nil
	wrongIdentity := change
	wrongIdentity.ExecutionID = contract.NewID()
	wrongIdentity.IdempotencyKey = "different-business-key"
	if err := store.CommitStep(ctx, wrongIdentity); err == nil {
		t.Fatal("retry changed its execution and business idempotency identities")
	}
	change.Failure = ""
	if err := store.CommitStep(ctx, change); err != nil {
		t.Fatal("second durable attempt:", err)
	}
	var status string
	var attempts, histories int
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_instance WHERE id = ?`, string(instanceID)).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_attempt WHERE instance_id = ?`, string(instanceID)).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_workflow_history WHERE instance_id = ?`, string(instanceID)).Scan(&histories); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || attempts != 2 || histories != 2 {
		t.Fatalf("persisted state status=%s attempts=%d history=%d", status, attempts, histories)
	}
	rows, err := database.QueryContext(ctx, `SELECT attempt,idempotency_key FROM _kora_workflow_attempt WHERE instance_id = ? ORDER BY attempt`, string(instanceID))
	if err != nil {
		t.Fatal("read attempts:", err)
	}
	defer rows.Close()
	for want := uint8(1); want <= 2; want++ {
		if !rows.Next() {
			t.Fatalf("missing durable attempt %d", want)
		}
		var got uint8
		var key string
		if err := rows.Scan(&got, &key); err != nil {
			t.Fatal("scan durable attempt:", err)
		}
		if got != want || key != string(change.IdempotencyKey) {
			t.Fatalf("attempt %d has attempt/key %d/%q", want, got, key)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected extra workflow attempt")
	}
	if err := rows.Err(); err != nil {
		t.Fatal("iterate durable attempts:", err)
	}

	// A crashed external action remains running after its lease expires. The
	// next due scan must atomically fence it into outcome_unknown instead of
	// returning a claim that could repeat the side effect.
	abandonedID := contract.InstanceID("01K7INSTANCE00000000000000001")
	if _, err := database.ExecContext(ctx, `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, string(abandonedID), "tenant-a", "purchase", 1, "Invoice", "INV-2", "1", "ready", "charge", 1, now, now); err != nil {
		t.Fatal("insert abandoned workflow fixture:", err)
	}
	claimed, err := store.ClaimDue(ctx, "tenant-a", "worker-external", 1, 10*time.Second)
	if err != nil || len(claimed) != 1 || claimed[0].InstanceID != abandonedID {
		t.Fatalf("abandoned workflow initial claim = %#v, %v", claimed, err)
	}
	executionID := "01K7EXECUTION000000000000000001"
	key := contract.IdempotencyKey("stable-external-key-01")
	pinned := contract.PinnedDispatch{ExecutionID: executionID, Scope: claimed[0].BusinessScope, CallerModuleID: "sales.orders", ProviderModuleID: "billing.ar", ProviderID: "billing.provider-a", BindingRevision: 6, Operation: contract.CapabilityAction{CapabilityName: "billing.invoice", OperationName: "issue", InputSchema: contract.SchemaRef{ID: "billing.input", Version: 1}, OutputSchema: contract.SchemaRef{ID: "billing.output", Version: 1}}, IdempotencyKey: key, InputHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	capabilityLease := contract.CapabilityLease{ID: "lease-a", TenantID: "tenant-a", Scope: claimed[0].BusinessScope, CallerModuleID: "sales.orders", ProviderModuleID: "billing.ar", ProviderID: "billing.provider-a", Operation: "issue", BindingRevision: 6, GrantEpoch: 3, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active", CredentialRef: "billing-token@fixture-version"}
	stored, err := store.BeginPinnedExternalAttempt(ctx, claimed[0], executionID, key, pinned, capabilityLease)
	if err != nil || stored == nil || stored.Dispatch.ProviderID != "billing.provider-a" {
		t.Fatal("persist external action start:", err)
	}
	reloaded, err := instanceStore.LoadPinnedCapabilityAttempt(ctx, "tenant-a", abandonedID, claimed[0].StepID)
	if err != nil || reloaded.Lease.CredentialRef != capabilityLease.CredentialRef || reloaded.Lease.GrantEpoch != capabilityLease.GrantEpoch || reloaded.Dispatch.ProviderID != pinned.ProviderID {
		t.Fatalf("reloaded durable provider grant = (%+v,%v)", reloaded, err)
	}
	var pinnedJSON string
	if err := database.QueryRowContext(ctx, `SELECT pinned_dispatch_json FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=?`, "tenant-a", string(abandonedID), string(claimed[0].StepID)).Scan(&pinnedJSON); err != nil || !strings.Contains(pinnedJSON, "billing.provider-a") {
		t.Fatalf("persisted provider pin = %q, %v", pinnedJSON, err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_workflow_instance SET lease_until = DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL 1 SECOND) WHERE id = ?`, string(abandonedID)); err != nil {
		t.Fatal("expire external action lease:", err)
	}
	claims, err := store.ClaimDue(ctx, "tenant-a", "worker-recovery", 1, 10*time.Second)
	if err != nil || len(claims) != 0 {
		t.Fatalf("recovery returned retry claim %#v, %v", claims, err)
	}
	var instanceStatus, attemptStatus string
	var recoveredFence uint64
	if err := database.QueryRowContext(ctx, `SELECT status,lease_fence FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`, "tenant-a", string(abandonedID)).Scan(&instanceStatus, &recoveredFence); err != nil {
		t.Fatal("read recovered workflow:", err)
	}
	if err := database.QueryRowContext(ctx, `SELECT status FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND execution_id=? AND attempt=1`, "tenant-a", string(abandonedID), executionID).Scan(&attemptStatus); err != nil {
		t.Fatal("read recovered external attempt:", err)
	}
	if instanceStatus != string(contract.InstanceUncertain) || attemptStatus != "outcome_unknown" || recoveredFence <= uint64(claimed[0].Fence) {
		t.Fatalf("recovered state instance=%s attempt=%s fence=%d (old %d)", instanceStatus, attemptStatus, recoveredFence, claimed[0].Fence)
	}
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_outbox WHERE event_type='kora.workflow.outcome_unknown' AND aggregate_id=?`, string(abandonedID)).Scan(&attempts); err != nil {
		t.Fatal("read uncertain workflow outbox event:", err)
	}
	if attempts != 1 {
		t.Fatalf("uncertain workflow events=%d, want 1", attempts)
	}
}
