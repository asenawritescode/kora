package eventbus

import (
	"context"
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// LocalProvider adapts the in-process/WAL eventstream.Bus to the provider-neutral
// contract.EventPublisher interface (RFC §7). The WAL rotation/replay machinery
// remains behind this provider and is not part of the public EventPublisher
// contract.
type LocalProvider struct {
	bus eventstream.Bus
}

// NewLocalProvider wraps an eventstream.Bus as a contract.EventPublisher.
func NewLocalProvider(bus eventstream.Bus) contract.EventPublisher {
	return &LocalProvider{bus: bus}
}

// Publish converts an EventEnvelope into a eventstream.ChangeEvent and publishes it through
// the underlying in-process bus. Conversion is best-effort: fields that do not
// map cleanly (e.g., aggregate identity) are carried through the Data payload.
func (p *LocalProvider) Publish(ctx context.Context, event contract.EventEnvelope) error {
	_ = ctx // the in-process bus is non-cancellable by design

	change := eventstream.ChangeEvent{
		ID:            event.ID,
		Sequence:      string(event.Sequence),
		Site:          event.Site,
		BusinessScope: event.BusinessScope,
		Doctype:       event.AggregateType,
		DocName:       event.AggregateID,
		Operation:     envelopeOp(event.Type),
		Timestamp:     event.OccurredAt,
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

// envelopeOp maps a canonical event type back to an eventstream.EventOp for the in-process
// bus. Unknown types map to eventstream.EventUpdate as a safe default.
func envelopeOp(eventType string) eventstream.EventOp {
	switch {
	case hasSuffix(eventType, ".after_insert"):
		return eventstream.EventInsert
	case hasSuffix(eventType, ".after_delete"):
		return eventstream.EventDelete
	case hasSuffix(eventType, ".after_submit"):
		return eventstream.EventSubmit
	case hasSuffix(eventType, ".after_cancel"):
		return eventstream.EventCancel
	default:
		return eventstream.EventUpdate
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
