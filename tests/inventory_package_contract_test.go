package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
)

// TestInventoryPackageIsDeclarative proves the reference package is consumed
// as configuration by generic loaders. This test intentionally contains no
// inventory execution logic.
func TestInventoryPackageIsDeclarative(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(source))
	packageRoot := filepath.Join(root, "config", "inventory")

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
