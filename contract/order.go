package contract

import "errors"

// SiteAggregateKey is the ordering key for durable events (DURABLE-001).
// Providers must preserve per-key FIFO; events with the same key are delivered
// in publish order.
//
//contract:wire
type SiteAggregateKey struct {
	Site          string
	AggregateType string
	AggregateID   string
}

// OrderKey returns the stable ordering key for an event envelope. Events for
// the same (site, aggregate) must be delivered in publish order.
func (e EventEnvelope) OrderKey() SiteAggregateKey {
	return SiteAggregateKey{
		Site:          e.Site,
		AggregateType: e.AggregateType,
		AggregateID:   e.AggregateID,
	}
}

// OrderKeyString renders the ordering key as a stable string.
func (k SiteAggregateKey) String() string {
	return k.Site + "/" + k.AggregateType + "/" + k.AggregateID
}

// Typed durable-delivery sentinels (DURABLE-001). Callers match via errors.Is,
// never string matching.
var (
	ErrOrderViolation   = errors.New("contract: per-key ordering violated")
	ErrDuplicateReceipt = errors.New("contract: duplicate delivery receipt")
)
