//go:build integration

package kernel_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
)

func TestExecuteInTxJoinsCallerTransactionAndReplaysReceipt(t *testing.T) {
	site := newSite(t, "tx-bound")
	defer site.DB.Close()
	engine := newKernel(site)
	execution, err := policy.NewVerifiedExecution("stable-tenant-tx", contract.ActorContext{
		PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", Roles: []string{"Administrator"},
	})
	if err != nil {
		t.Fatal("verified execution:", err)
	}
	payload, err := json.Marshal(kernel.RecordCreatePayload{Doctype: "Task", Data: json.RawMessage(`{"title":"outer transaction"}`)})
	if err != nil {
		t.Fatal("encode command:", err)
	}
	operation := kernel.Operation{Command: kernel.CommandRecordCreate, Payload: payload}
	operation.Context = kernel.OperationContext{Site: "tenant-a", TenantID: "stable-tenant-tx", Execution: execution,
		Source: kernel.SourceWorker, IdempotencyKey: "workflow-task-create"}

	rollbackTx, err := site.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin rollback transaction:", err)
	}
	defer rollbackTx.Rollback()
	prepared, afterRollback, prepareErr := engine.ExecuteInTx(context.Background(), site.DB, rollbackTx, site.Registry, operation)
	if prepareErr != nil {
		t.Fatalf("prepare command in caller transaction: result=%#v error=%#v", prepared, prepareErr)
	}
	if afterRollback == nil {
		t.Fatal("transaction-bound execution returned no post-commit callback")
	}
	if err := rollbackTx.Rollback(); err != nil {
		t.Fatal("roll back caller transaction:", err)
	}
	var count int
	if err := site.DB.QueryRow(`SELECT COUNT(*) FROM tabTask WHERE title = ?`, "outer transaction").Scan(&count); err != nil {
		t.Fatal("count after caller rollback:", err)
	}
	if count != 0 {
		t.Fatalf("command escaped caller rollback: document count=%d", count)
	}
	if err := site.DB.QueryRow(`SELECT COUNT(*) FROM _kora_idempotency_receipt WHERE site = ?`, "tenant-a").Scan(&count); err != nil {
		t.Fatal("count receipts after rollback:", err)
	}
	if count != 0 {
		t.Fatalf("receipt escaped caller rollback: count=%d", count)
	}

	commitTx, err := site.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin commit transaction:", err)
	}
	defer commitTx.Rollback()
	result, afterCommit, commitErr := engine.ExecuteInTx(context.Background(), site.DB, commitTx, site.Registry, operation)
	if commitErr != nil {
		_ = commitTx.Rollback()
		t.Fatalf("retry command in caller transaction: %#v", commitErr)
	}
	if result.Status != contract.StatusCompleted || afterCommit == nil {
		_ = commitTx.Rollback()
		t.Fatalf("prepared command = (%#v, callback %t)", result, afterCommit != nil)
	}
	if err := commitTx.Commit(); err != nil {
		t.Fatal("commit caller transaction:", err)
	}
	afterCommit()

	replayTx, err := site.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal("begin replay transaction:", err)
	}
	defer replayTx.Rollback()
	replayed, afterReplay, replayErr := engine.ExecuteInTx(context.Background(), site.DB, replayTx, site.Registry, operation)
	if replayErr != nil {
		_ = replayTx.Rollback()
		t.Fatalf("replay command in caller transaction: %#v", replayErr)
	}
	if !replayed.Replayed {
		_ = replayTx.Rollback()
		t.Fatalf("same transaction key did not replay: %#v", replayed)
	}
	if err := replayTx.Commit(); err != nil {
		t.Fatal("commit replay transaction:", err)
	}
	afterReplay()
	if err := site.DB.QueryRow(`SELECT COUNT(*) FROM tabTask WHERE title = ?`, "outer transaction").Scan(&count); err != nil {
		t.Fatal("count committed documents:", err)
	}
	if count != 1 {
		t.Fatalf("committed document count=%d, want one", count)
	}
	if err := site.DB.QueryRow(`SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_type = ? AND aggregate_id = ?`, "Task", "TASK-0001").Scan(&count); err != nil {
		t.Fatal("count outbox events:", err)
	}
	if count != 1 {
		t.Fatalf("outbox event count=%d, want one", count)
	}
}
