package analytics

import (
	"context"
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
)

// LocalProvider adapts the in-process/WAL EventBus to the provider-neutral
// contract.EventPublisher interface (RFC §7). The WAL rotation/replay machinery
// remains behind this provider and is not part of the public EventPublisher
// contract.
type LocalProvider struct {
	bus EventBus
}

// NewLocalProvider wraps an EventBus as a contract.EventPublisher.
func NewLocalProvider(bus EventBus) contract.EventPublisher {
	return &LocalProvider{bus: bus}
}

// Publish converts an EventEnvelope into a ChangeEvent and publishes it through
// the underlying in-process bus. Conversion is best-effort: fields that do not
// map cleanly (e.g., aggregate identity) are carried through the Data payload.
func (p *LocalProvider) Publish(ctx context.Context, event contract.EventEnvelope) error {
	_ = ctx // the in-process bus is non-cancellable by design

	change := ChangeEvent{
		ID:        event.ID,
		Site:      event.Site,
		Doctype:   event.AggregateType,
		DocName:   event.AggregateID,
		Operation: envelopeOp(event.Type),
		Timestamp: event.OccurredAt,
	}

	// Recover Data and old_data best-effort from the envelope payload.
	var payload struct {
		Data    map[string]any `json:"data"`
		OldData map[string]any `json:"old_data"`
	}
	if len(event.Data) > 0 {
		_ = json.Unmarshal(event.Data, &payload)
	}
	change.Data = payload.Data
	change.OldData = payload.OldData

	return p.bus.Publish(change)
}

// envelopeOp maps a canonical event type back to an EventOp for the in-process
// bus. Unknown types map to EventUpdate as a safe default.
func envelopeOp(eventType string) EventOp {
	switch {
	case hasSuffix(eventType, ".after_insert"):
		return EventInsert
	case hasSuffix(eventType, ".after_delete"):
		return EventDelete
	case hasSuffix(eventType, ".after_submit"):
		return EventSubmit
	case hasSuffix(eventType, ".after_cancel"):
		return EventCancel
	default:
		return EventUpdate
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
