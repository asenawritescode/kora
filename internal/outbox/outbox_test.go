package outbox

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/analytics"
)

func TestEventTypeName(t *testing.T) {
	tests := []struct {
		doctype string
		op      analytics.EventOp
		want    string
	}{
		{"Sales Invoice", analytics.EventInsert, "kora.sales_invoice.after_insert"},
		{"Sales Invoice", analytics.EventUpdate, "kora.sales_invoice.after_save"},
		{"Sales Invoice", analytics.EventDelete, "kora.sales_invoice.after_delete"},
		{"Sales Invoice", analytics.EventSubmit, "kora.sales_invoice.after_submit"},
		{"Sales Invoice", analytics.EventCancel, "kora.sales_invoice.after_cancel"},
		{"Work Order", analytics.EventInsert, "kora.work_order.after_insert"},
	}
	for _, tt := range tests {
		if got := EventTypeName(tt.doctype, tt.op); got != tt.want {
			t.Errorf("EventTypeName(%q, %q) = %q, want %q", tt.doctype, tt.op, got, tt.want)
		}
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Sales Invoice", "sales_invoice"},
		{"Work Order", "work_order"},
		{"customer", "customer"},
		{"Already_Snake", "already_snake"},
	}
	for _, tt := range tests {
		if got := ToSnake(tt.in); got != tt.want {
			t.Errorf("ToSnake(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{-1, time.Second},
		{100, time.Minute}, // clamped at 1m
	}
	for _, tt := range tests {
		if got := backoff(tt.attempt); got != tt.want {
			t.Errorf("backoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestChangeEventToEnvelope(t *testing.T) {
	now := time.Now().UTC()
	e := ChangeEventToEnvelope(analytics.ChangeEvent{
		Site: "acme.example.com", Doctype: "Sales Invoice", DocName: "SINV-0001",
		Operation: analytics.EventInsert, Timestamp: now,
	})

	if e.Type != "kora.sales_invoice.after_insert" {
		t.Errorf("Type = %q, want after_insert", e.Type)
	}
	if e.Source != "kora.kernel" {
		t.Errorf("Source = %q, want kora.kernel", e.Source)
	}
	if e.AggregateType != "Sales Invoice" || e.AggregateID != "SINV-0001" {
		t.Errorf("aggregate mismatch: %q %q", e.AggregateType, e.AggregateID)
	}
	if e.Version != 1 {
		t.Errorf("Version = %d, want 1", e.Version)
	}
	if e.ID == "" {
		t.Errorf("ID should not be empty")
	}
}

func TestEventTypeNameAllOps(t *testing.T) {
	all := []analytics.EventOp{
		analytics.EventInsert, analytics.EventUpdate, analytics.EventDelete,
		analytics.EventSubmit, analytics.EventCancel,
	}
	for _, op := range all {
		name := EventTypeName("Customer", op)
		if name == "" {
			t.Errorf("EventTypeName(Customer, %q) returned empty", op)
		}
		if name[:5] != "kora." {
			t.Errorf("EventTypeName(Customer, %q) = %q, want kora. prefix", op, name)
		}
	}
}
