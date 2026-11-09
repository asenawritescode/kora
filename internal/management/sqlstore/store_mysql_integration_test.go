//go:build integration

package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLManagementAcceptanceDurabilityAndIdempotency(t *testing.T) {
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
	name := fmt.Sprintf("kora_management_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
		t.Fatal("create disposable database:", err)
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
	store, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal("ensure operation schema:", err)
	}
	input := management.AcceptCommand{ID: "01K7OPERATION000000000000000000", Tenant: "tenant-1", Caller: "cloud-service", IdempotencyKey: "provision-key-1", Kind: management.CommandProvisionTenant, Command: []byte(`{"tenant_id":"tenant-1"}`), Now: time.Now().UTC()}
	accepted, err := store.Accept(ctx, input)
	if err != nil {
		t.Fatal("accept operation:", err)
	}
	restarted, err := New(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Accept(ctx, input)
	if err != nil {
		t.Fatal("replay after store restart:", err)
	}
	if replayed.ID != accepted.ID || replayed.State != contract.OperationRequested || management.CommandDigest(replayed.Kind, replayed.Command) != management.CommandDigest(input.Kind, input.Command) {
		t.Fatalf("durable replay changed operation: accepted=%+v replay=%+v", accepted, replayed)
	}
	firstClaims, err := store.ClaimDue(ctx, "worker-old", 1, time.Minute)
	if err != nil || len(firstClaims) != 1 {
		t.Fatalf("initial claim = %d, %v", len(firstClaims), err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE _kora_managed_operation SET lease_until = DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL 1 SECOND) WHERE id = ?`, string(accepted.ID)); err != nil {
		t.Fatal("expire old claim:", err)
	}
	secondClaims, err := restarted.ClaimDue(ctx, "worker-new", 1, time.Minute)
	if err != nil || len(secondClaims) != 1 {
		t.Fatalf("recovered claim = %d, %v", len(secondClaims), err)
	}
	if secondClaims[0].Fence <= firstClaims[0].Fence {
		t.Fatalf("recovery did not advance the fence: old=%d new=%d", firstClaims[0].Fence, secondClaims[0].Fence)
	}
	if err := store.Finish(ctx, firstClaims[0], management.FinishResult{State: contract.OperationSucceeded}); err == nil {
		t.Fatal("stale worker completed a recovered operation")
	}
	if err := restarted.Finish(ctx, secondClaims[0], management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: "2"}); err != nil {
		t.Fatal("finish recovered operation:", err)
	}
	completed, err := store.Get(ctx, input.Caller, accepted.ID)
	if err != nil || completed.State != contract.OperationSucceeded || completed.ObservedRevision != "2" {
		t.Fatalf("completed operation = %+v, %v", completed, err)
	}
	if _, err := store.Get(ctx, "other-caller", accepted.ID); err != nil {
		var value *contract.Error
		if !errors.As(err, &value) || value.Type != contract.CodeNotFound {
			t.Fatalf("cross-caller lookup should return not found, got %v", err)
		}
	} else {
		t.Fatal("cross-caller operation lookup succeeded")
	}
	changed := input
	changed.Command = []byte(`{"tenant_id":"tenant-2"}`)
	if _, err := store.Accept(ctx, changed); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("changed command retry error = %v", err)
	}

	stateInput := management.AcceptCommand{
		ID: "01K7OPERATION000000000000000001", Tenant: input.Tenant, Caller: input.Caller,
		IdempotencyKey: "state-operation-1", Kind: management.CommandSetTenantState,
		Command: []byte(`{"meta":{"idempotency_key":"state-operation-1"},"state":"suspended","expected_config_revision":"4","reason":"operator request"}`),
		Now:     time.Now().UTC(),
	}
	stateOperation, err := store.Accept(ctx, stateInput)
	if err != nil {
		t.Fatal("accept state operation:", err)
	}
	stateClaims, err := store.ClaimDue(ctx, "worker-state", 1, time.Minute)
	if err != nil || len(stateClaims) != 1 || stateClaims[0].Operation.ID != stateOperation.ID {
		t.Fatalf("claim state operation = %+v, %v", stateClaims, err)
	}
	if err := store.Finish(ctx, stateClaims[0], management.FinishResult{State: contract.OperationRecoveryRequired, Error: contract.NewError(contract.CodeDependencyUnavailable, "runtime convergence uncertain")}); err != nil {
		t.Fatal("mark uncertain state operation:", err)
	}
	key := contract.IdempotencyKey("reconcile-state-1")
	reason := "operator verified directory state and requests a safe convergence retry"
	reconciled, err := store.RetryStateOperation(ctx, input.Caller, stateOperation.ID, key, reason)
	if err != nil || reconciled.ID != stateOperation.ID || reconciled.State != contract.OperationRequested || reconciled.Error != nil {
		t.Fatalf("reconcile state operation = %+v, %v", reconciled, err)
	}
	replayed, err = restarted.RetryStateOperation(ctx, input.Caller, stateOperation.ID, key, reason)
	if err != nil || replayed.ID != reconciled.ID || replayed.State != reconciled.State {
		t.Fatalf("replay reconciliation = %+v, %v", replayed, err)
	}
	if _, err := restarted.RetryStateOperation(ctx, input.Caller, stateOperation.ID, key, reason+" changed"); !hasCode(err, contract.CodeIdempotencyKeyReused) {
		t.Fatalf("changed reconciliation retry error = %v", err)
	}
	var auditCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM _kora_managed_operation_recovery WHERE operation_id = ?`, string(stateOperation.ID)).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("reconciliation audit count = %d, %v", auditCount, err)
	}
	retryClaims, err := restarted.ClaimDue(ctx, "worker-reconcile", 1, time.Minute)
	if err != nil || len(retryClaims) != 1 || retryClaims[0].Operation.ID != stateOperation.ID || retryClaims[0].Fence <= stateClaims[0].Fence+1 {
		t.Fatalf("claim reconciled operation = %+v, %v", retryClaims, err)
	}
	if err := restarted.Finish(ctx, retryClaims[0], management.FinishResult{State: contract.OperationSucceeded, ObservedRevision: "5"}); err != nil {
		t.Fatal("finish reconciled state operation:", err)
	}
}
