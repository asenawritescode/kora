package inventory

import (
	"testing"
	"time"
)

func movement(id string, qty int64) Movement {
	return Movement{ID: id, Item: "item-1", Location: "warehouse-1", Quantity: qty, Kind: MovementReceive, Actor: "user-1", Reason: "counted", OccurredAt: time.Now()}
}

func TestLedgerProjectsBalanceAndRejectsNegativeStock(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Append(movement("in-1", 10)); err != nil {
		t.Fatal(err)
	}
	out := movement("out-1", -12)
	out.Kind = MovementIssue
	if err := l.Append(out); err == nil {
		t.Fatal("negative stock was accepted")
	}
	if got := l.Balance("item-1", "warehouse-1"); got != 10 {
		t.Fatalf("balance = %d, want 10", got)
	}
}

func TestLedgerReversalAndLowStock(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Append(movement("in-1", 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reverse("in-1", "reverse-1", "user-1", "mistake"); err != nil {
		t.Fatal(err)
	}
	if got := l.Balance("item-1", "warehouse-1"); got != 0 {
		t.Fatalf("reversed balance = %d", got)
	}
	if _, low := l.LowStock("item-1", "warehouse-1", 5); !low {
		t.Fatal("expected low stock alert")
	}
}

func TestLedgerAllowsConfiguredNegativeStock(t *testing.T) {
	l := NewLedger(Policy{AllowNegative: true})
	out := movement("out-1", -3)
	out.Kind = MovementIssue
	if err := l.Append(out); err != nil {
		t.Fatal(err)
	}
	if l.Balance("item-1", "warehouse-1") != -3 {
		t.Fatal("configured negative balance not projected")
	}
}

func TestReservationsDoNotChangePhysicalBalance(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Append(movement("in-1", 10)); err != nil {
		t.Fatal(err)
	}
	if err := l.Reserve("reserve-1", "item-1", "warehouse-1", 6, "user-1", "purchase request"); err != nil {
		t.Fatal(err)
	}
	if l.Balance("item-1", "warehouse-1") != 10 {
		t.Fatalf("physical balance changed: %d", l.Balance("item-1", "warehouse-1"))
	}
	if l.Reserved("item-1", "warehouse-1") != 6 || l.Available("item-1", "warehouse-1") != 4 {
		t.Fatal("reservation projection incorrect")
	}
	if err := l.Release("release-1", "item-1", "warehouse-1", 6, "user-1", "cancelled"); err != nil {
		t.Fatal(err)
	}
	if l.Reserved("item-1", "warehouse-1") != 0 {
		t.Fatal("released reservation remained")
	}
}

func TestTransferProjectsBothLocations(t *testing.T) {
	l := NewLedger(Policy{})
	if err := l.Append(Movement{ID: "in-1", Item: "item-1", Location: "from", Quantity: 10, Kind: MovementReceive, Actor: "user-1", Reason: "opening"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Transfer("move-out", "move-in", "item-1", "from", "to", 4, "user-1", "replenishment"); err != nil {
		t.Fatal(err)
	}
	if l.Balance("item-1", "from") != 6 || l.Balance("item-1", "to") != 4 {
		t.Fatal("transfer projection incorrect")
	}
}
