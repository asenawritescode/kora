package doctype

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInventoryPackageValidatesAndInstalls(t *testing.T) {
	root := filepath.Join("..", "config", "inventory")
	doctypes, err := ParseConfigTree(root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := ValidateAll(doctypes); len(errs) > 0 {
		t.Fatalf("cross-file validation: %v", errs)
	}
	reg := NewRegistry()
	for _, dt := range doctypes {
		if dt.Module != "Inventory" {
			t.Errorf("%s module = %q, want Inventory", dt.Name, dt.Module)
		}
		reg.Register(dt)
	}
	for _, name := range []string{"Item", "Location", "Stock Entry", "Stock Balance"} {
		if reg.Get(name) == nil {
			t.Errorf("registry missing %s", name)
		}
	}
	item := reg.Get("Item")
	for _, f := range []string{"price", "cost", "barcode", "uom"} {
		if item.GetField(f) == nil {
			t.Errorf("Item missing field %s", f)
		}
	}
	if item.GetField("reorder_level") != nil {
		t.Error("Item must not have reorder_level")
	}
	if reg.Get("Stock Balance").GetField("reorder_level") == nil {
		t.Error("Stock Balance missing reorder_level")
	}
	for _, f := range []string{"roles.yaml", "permissions.yaml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
}
