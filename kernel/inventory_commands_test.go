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
	if e := recv.Steps[0].Ensure; e == nil || e.Record != "Stock Balance" || recv.Steps[0].As == "" {
		t.Errorf("receive must ensure a Stock Balance exists first: %+v", recv.Steps[0])
	}
	if len(recv.Require) != 1 || recv.Require[0].Lock == nil || recv.Require[0].Lock.Record != "Item" {
		t.Errorf("receive must lock the Item so two first receipts cannot both create a balance: %+v", recv.Require)
	}
	if got := recv.Steps[1].Update.Adjust["quantity"]; got.Increase == "" {
		t.Errorf("receive must increase balance: %+v", got)
	}
	if adj.Steps[0].Ensure != nil {
		t.Errorf("adjust must not create a missing balance")
	}
	got := adj.Steps[0].Update.Adjust["quantity"]
	if got.Decrease == "" || got.Min != "0" {
		t.Errorf("adjust must decrease with min 0 (no negative stock): %+v", got)
	}
	if adj.Steps[1].Create.Values["remarks"] != "$input.reason" {
		t.Errorf("adjust must record a reason: %v", adj.Steps[1].Create.Values)
	}
}
