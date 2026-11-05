package kernel_test

import (
	"testing"

	"github.com/asenawritescode/kora/kernel"
)

func TestInventoryStockCommands(t *testing.T) {
	defs, err := kernel.ParseCommandDir("../config/inventory/commands")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*kernel.CommandResource{}
	for _, d := range defs {
		by[d.Name] = d
	}
	recv, adj := by["receive_stock"], by["adjust_stock"]
	if recv == nil || adj == nil {
		t.Fatalf("missing commands: %v", by)
	}
	if got := recv.Steps[0].Update.Adjust["quantity"]; got.Increase == "" {
		t.Errorf("receive must increase balance: %+v", got)
	}
	got := adj.Steps[0].Update.Adjust["quantity"]
	if got.Decrease == "" || got.Min != "0" {
		t.Errorf("adjust must decrease with min 0 (no negative stock): %+v", got)
	}
	if adj.Steps[1].Create.Values["remarks"] != "$input.reason" {
		t.Errorf("adjust must record a reason: %v", adj.Steps[1].Create.Values)
	}
}
