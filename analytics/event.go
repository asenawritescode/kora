// Package analytics provides the dynamic, per-site analytics engine for Kora.
// It uses a rollup-based approach: document writes emit events, a worker
// pre-computes aggregates into _kora_analytics_* tables in the operational DB.
package analytics

import "time"

// EventOp describes the type of document change.
type EventOp string

const (
	EventInsert EventOp = "insert"
	EventUpdate EventOp = "update"
	EventDelete EventOp = "delete"
	EventSubmit EventOp = "submit"
	EventCancel EventOp = "cancel"
)

// ChangeEvent captures a document write for analytics ingestion.
// Emitted by the ORM after every write — async, non-blocking.
type ChangeEvent struct {
	Site       string         `json:"site"`
	Doctype    string         `json:"doctype"`
	DocName    string         `json:"doc_name"`
	Operation  EventOp        `json:"operation"`
	Timestamp  time.Time      `json:"timestamp"`
	ModifiedBy string         `json:"modified_by"`
	Data       map[string]any `json:"data"`                 // full document fields after write
	OldData    map[string]any `json:"old_data,omitempty"`   // previous state (update/delete only)
}

// EventBus is the interface for publishing and subscribing to change events.
// The default implementation is an in-process buffered channel with WAL spill.
type EventBus interface {
	// Publish sends an event. Never blocks — drops to WAL if the channel is full.
	Publish(event ChangeEvent) error

	// Subscribe returns a channel that receives events. Only one subscriber per bus.
	Subscribe() (<-chan ChangeEvent, error)

	// Dropped returns the count of events spilled to WAL due to a full channel.
	Dropped() int64

	// Close shuts down the event bus and flushes any pending WAL.
	Close() error
}
