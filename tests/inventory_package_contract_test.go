package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/workflow"
)

// TestInventoryPackageIsDeclarative proves the reference package is consumed
// as configuration by generic loaders. This test intentionally contains no
// inventory execution logic.
func TestInventoryPackageIsDeclarative(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(source))
	packageRoot := filepath.Join(root, "config", "v0", "inventory")

	doctypes, err := doctype.ParseConfigTree(packageRoot)
	if err != nil {
		t.Fatalf("parse package doctypes: %v", err)
	}
	wantDoctypes := map[string]bool{
		"Product": true, "Warehouse": true, "Supplier": true, "Stock Balance": true,
		"Stock Movement": true, "Stock Reservation": true, "Purchase Request": true,
		"Purchase Order": true, "Goods Receipt": true, "Supplier Notification": true,
	}
	got := make(map[string]bool, len(doctypes))
	for _, d := range doctypes {
		got[d.Name] = true
	}
	for name := range wantDoctypes {
		if !got[name] {
			t.Errorf("package missing doctype %q", name)
		}
	}

	commands, err := kernel.LoadCommandDir(filepath.Join(packageRoot, "commands"))
	if err != nil {
		t.Fatalf("parse package commands: %v", err)
	}
	wantCommands := []string{"add_stock", "issue_stock", "transfer_stock", "adjust_stock", "reserve_stock", "release_stock", "create_purchase_request", "approve_purchase_request", "create_purchase_order", "receive_goods", "notify_supplier"}
	if len(commands.List()) != len(wantCommands) {
		t.Fatalf("command count = %d, want %d", len(commands.List()), len(wantCommands))
	}
	for _, name := range wantCommands {
		if _, ok := commands.Lookup("inventory." + name); !ok {
			t.Errorf("package missing command %q", name)
		}
	}
	if _, err := os.Stat(filepath.Join(packageRoot, "package.yaml")); err != nil {
		t.Fatalf("package manifest missing: %v", err)
	}
	for _, path := range []string{
		filepath.Join(packageRoot, "rules", "low_stock.yaml"),
		filepath.Join(packageRoot, "workflows", "low_stock_procurement.yaml"),
		filepath.Join(packageRoot, "views", "inventory_workspace.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("package resource missing %q: %v", path, err)
		}
	}
}

type packageStepExecutor struct{ steps []string }

type packageRuleEvaluator struct{ result bool }

func (e packageRuleEvaluator) Evaluate(context.Context, string, any) (bool, error) {
	return e.result, nil
}

func (e *packageStepExecutor) Execute(_ context.Context, capability string, _ contract.ActorContext, _ any) error {
	e.steps = append(e.steps, capability)
	return nil
}

func TestInventoryPackageWorkflowRunsThroughGenericExecutor(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(source))
	packageRoot := filepath.Join(root, "config", "v0", "inventory")
	raw, err := os.ReadFile(filepath.Join(packageRoot, "workflows", "low_stock_procurement.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := workflow.ParseDeclarativeWorkflow(raw)
	if err != nil {
		t.Fatalf("parse procurement workflow: %v", err)
	}
	executor := &packageStepExecutor{}
	steps, err := workflow.RunDeclarativeWorkflow(context.Background(), wf, contract.ActorContext{PrincipalID: "operator", PrincipalType: contract.PrincipalHuman}, map[string]any{"item": "A", "quantity": 0}, executor)
	if err != nil {
		t.Fatalf("run procurement workflow: %v", err)
	}
	if len(steps) != 5 || len(executor.steps) != 5 {
		t.Fatalf("workflow executed %v capability steps %v", steps, executor.steps)
	}
	raw, err = os.ReadFile(filepath.Join(packageRoot, "rules", "low_stock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rule, err := workflow.ParseDeclarativeRule(raw)
	if err != nil {
		t.Fatalf("parse low-stock rule: %v", err)
	}
	matched, err := workflow.EvaluateDeclarativeRule(context.Background(), rule, "inventory.stock_movement.recorded", map[string]any{"quantity": 0, "reorder_level": 10}, packageRuleEvaluator{result: true})
	if err != nil || !matched {
		t.Fatalf("low-stock rule did not match through generic evaluator: matched=%v err=%v", matched, err)
	}
}
