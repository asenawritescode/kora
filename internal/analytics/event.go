package analytics

import "github.com/asenawritescode/kora/internal/eventstream"

// Runtime event types are aliased here for source compatibility. New
// persistence, routing, and transport code should depend on eventstream.
type EventOp = eventstream.EventOp
type ChangeEvent = eventstream.ChangeEvent
type EventBus = eventstream.Bus
type EventWALAcknowledger = eventstream.WALAcknowledger

// WALBatchDrainer reads at most limit events from durable local storage.
// A non-positive limit means all available events. Workers use this optional
// capability to bound per-batch memory while retaining the EventBus contract.
type WALBatchDrainer interface {
	DrainWALBatch(limit int, handler func(ChangeEvent)) (int, error)
}

const (
	EventInsert       = eventstream.EventInsert
	EventUpdate       = eventstream.EventUpdate
	EventDelete       = eventstream.EventDelete
	EventSubmit       = eventstream.EventSubmit
	EventCancel       = eventstream.EventCancel
	EventNotification = eventstream.EventNotification
)
