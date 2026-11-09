package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
)

func TestEnvelopeOp(t *testing.T) {
	tests := []struct {
		eventType string
		want      EventOp
	}{
		{"kora.sales_invoice.after_insert", EventInsert},
		{"kora.sales_invoice.after_delete", EventDelete},
		{"kora.sales_invoice.after_submit", EventSubmit},
		{"kora.sales_invoice.after_cancel", EventCancel},
		{"kora.sales_invoice.after_save", EventUpdate},
		{"kora.sales_invoice.unknown", EventUpdate},
	}
	for _, tt := range tests {
		if got := envelopeOp(tt.eventType); got != tt.want {
			t.Errorf("envelopeOp(%q) = %q, want %q", tt.eventType, got, tt.want)
		}
	}
}

func TestLocalProviderPublish(t *testing.T) {
	tmpDir := t.TempDir()
	bus := NewChannelBus(10, tmpDir+"/wal")
	defer bus.Close()

	provider := NewLocalProvider(bus)

	now := time.Now().UTC()
	err := provider.Publish(context.Background(), contract.EventEnvelope{
		ID:            contract.NewEventID(),
		Type:          "kora.customer.after_insert",
		Version:       1,
		Source:        "kora.kernel",
		Site:          "test.local",
		AggregateType: "Customer",
		AggregateID:   "CUST-0001",
		OccurredAt:    now,
		Data:          contract.MustEncodeData(map[string]any{"data": map[string]any{"name": "A"}, "old_data": nil}),
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	// WAL-first durability means zero dropped.
	if d := bus.Dropped(); d != 0 {
		t.Errorf("dropped = %d, want 0", d)
	}
}

type capturingBus struct {
	EventBus
	published []ChangeEvent
}

func (b *capturingBus) Publish(event ChangeEvent) error {
	b.published = append(b.published, event)
	return nil
}

func TestLocalProviderPublishCarriesBusinessScope(t *testing.T) {
	bus := &capturingBus{}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "le-nairobi"}
	err := NewLocalProvider(bus).Publish(context.Background(), contract.EventEnvelope{
		ID:            contract.NewEventID(),
		Type:          "kora.sale.after_insert",
		Version:       1,
		Source:        "kora.kernel",
		Site:          "tenant-a",
		BusinessScope: scope,
		AggregateType: "Sale",
		AggregateID:   "SALE-0001",
		OccurredAt:    time.Now().UTC(),
		Data:          contract.MustEncodeData(map[string]any{"data": map[string]any{"name": "SALE-0001"}}),
	})
	if err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if len(bus.published) != 1 {
		t.Fatalf("published %d change events, want 1", len(bus.published))
	}
	if got := bus.published[0].BusinessScope; got != scope {
		t.Fatalf("change event scope = %#v, want %#v", got, scope)
	}
}
