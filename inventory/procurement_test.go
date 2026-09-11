package inventory

import "testing"

func TestLowStockCreatesApprovedProcurementFlow(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Append(movement("in-1", 4)); err != nil {
		t.Fatal(err)
	}
	alert, low := l.LowStock("item-1", "warehouse-1", 10)
	if !low {
		t.Fatal("expected low-stock alert")
	}
	p := NewProcurement()
	request, err := p.CreateFromLowStock("pr-1", "item-1", "warehouse-1", 20, "operator-1", "reorder threshold", alert)
	if err != nil || request.State != PurchaseDraft {
		t.Fatalf("create request: %+v %v", request, err)
	}
	for _, state := range []PurchaseRequestState{PurchaseSubmitted, PurchaseApproved, PurchaseOrdered, PurchaseReceived} {
		actor := "operator-1"
		if state == PurchaseApproved {
			actor = "manager-1"
		}
		if _, err := p.Transition("pr-1", state, actor); err != nil {
			t.Fatalf("transition %s: %v", state, err)
		}
	}
	final, _ := p.Get("pr-1")
	if final.ApprovedBy != "manager-1" || final.State != PurchaseReceived {
		t.Fatalf("unexpected final request: %+v", final)
	}
}
