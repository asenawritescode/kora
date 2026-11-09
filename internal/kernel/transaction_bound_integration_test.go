//go:build integration

package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

type rejectingModuleExtensionRunner struct{}

func (rejectingModuleExtensionRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return &script.ExecuteResult{Result: map[string]any{"valid": false}}, nil
}
func (rejectingModuleExtensionRunner) Validate(string) error { return nil }
func (rejectingModuleExtensionRunner) Close() error          { return nil }

type moduleExtensionIntegrationSchemas struct{}

func (moduleExtensionIntegrationSchemas) Registered(contract.SchemaRef) bool { return true }
func (moduleExtensionIntegrationSchemas) Validate(ref contract.SchemaRef, raw json.RawMessage) error {
	if ref.ID == "validation-result" {
		var result struct {
			Valid bool `json:"valid"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		if !result.Valid {
			return errors.New("owner validation extension rejected document")
		}
	}
	return nil
}

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

func TestOwnerValidationExtensionRejectionRollsBackKernelCommand(t *testing.T) {
	site := newSite(t, "module-extension-rollback")
	defer site.DB.Close()
	for _, ddl := range kdb.ExtensibilityTablesMySQL()[:2] {
		if _, err := site.DB.Exec(ddl); err != nil {
			t.Fatal("create lifecycle tables:", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(site.Dialect) {
		if _, err := site.DB.Exec(ddl); err != nil {
			t.Fatal("create config revision table:", err)
		}
	}
	if err := configstore.SeedConfigRevision(context.Background(), site.DB, site.Dialect); err != nil {
		t.Fatal("seed config revision:", err)
	}
	store := &script.Store{DB: site.DB, Dialect: site.Dialect}
	handler := contract.ResourceRef{Namespace: "module", Name: "vat-validation", Version: 1}
	if err := store.Insert(script.ScriptRecord{Name: handler.Name, Site: site.Name, ScriptType: script.TypeDocEvent,
		DocType: "Task", Event: script.EventValidate, IsActive: true, TimeoutMs: 500, Script: "return {result: {valid: false}}"}); err != nil {
		t.Fatal("insert extension handler:", err)
	}
	target := contract.ResourceRef{Namespace: "tenant", Name: "Task", Version: 1}
	owner := contract.ModuleDescriptor{ID: "work.tasks", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: target}}, ExtensionPoints: []contract.ExtensionPoint{{
		OwnerModuleID: "work.tasks", ID: "task.validation", Version: 1, Target: target,
		AllowedKinds: []string{string(contract.ExtensionValidation)}, InputSchema: contract.SchemaRef{ID: "task-input", Version: 1},
		OutputSchema: contract.SchemaRef{ID: "validation-result", Version: 1}, ExecutionPhase: "before_commit", FailurePolicy: "reject",
	}}}
	extender := contract.ModuleDescriptor{ID: "tax.vat", Version: "1.0.0", Resources: []contract.OwnedResource{{Ref: handler}}, Extensions: []contract.ExtensionRegistration{{
		ID: "vat-check", ExtenderModuleID: "tax.vat", PointID: "work.tasks/task.validation", PointVersion: 1,
		Kind: contract.ExtensionValidation, ArtifactDigest: strings.Repeat("a", 64), HandlerRef: handler, Order: 1,
	}}}
	modules := []contract.ModuleDescriptor{owner, extender}
	plan, err := contract.OrderModuleExtensions(modules, moduleExtensionIntegrationSchemas{}.Registered)
	if err != nil {
		t.Fatal("plan validation extension:", err)
	}
	engine := newKernel(site)
	engine.CommandSchemas = moduleExtensionIntegrationSchemas{}
	engine.TxManager = &orm.TxManager{ScriptRunner: rejectingModuleExtensionRunner{}, ScriptStore: store,
		ModuleDescriptors: modules, ModuleExtensions: plan, ModuleExtensionSchemas: moduleExtensionIntegrationSchemas{}}
	result := exec(t, site, engine, createOp(map[string]any{"title": "must-not-commit"}, withUser("Administrator"), withRoles("Administrator")))
	if result.Error == nil || result.Error.Type != contract.CodeValidationFailed {
		t.Fatalf("extension rejection result = %+v, want validation failure", result.Error)
	}
	if count := site.count(t, "SELECT COUNT(*) FROM `tabTask` WHERE title = 'must-not-commit'"); count != 0 {
		t.Fatalf("rejected extension left %d business rows", count)
	}
	if count := site.count(t, "SELECT COUNT(*) FROM _kora_outbox WHERE aggregate_type = 'Task'"); count != 0 {
		t.Fatalf("rejected extension left %d outbox events", count)
	}
}
