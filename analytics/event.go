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
// The default implementation is an in-process buffered channel with WAL-first
// durability: every event is written to disk before entering the channel.
type EventBus interface {
	// Publish sends an event. Never blocks — writes to WAL first, then
	// best-effort channel send. Returns nil even on WAL failure (logs + counts).
	Publish(event ChangeEvent) error

	// Subscribe returns a channel that receives events. Only one subscriber per bus.
	Subscribe() (<-chan ChangeEvent, error)

	// DrainWAL replays any events in the WAL through the given handler.
	// Called once at startup before consuming live events to recover from crashes.
	DrainWAL(handler func(ChangeEvent)) (int, error)

	// RotateWAL atomically swaps the current WAL file with a fresh one.
	// Returns the path of the old WAL file. After a successful DB flush, the
	// caller should call CommitWALRotation() to delete the old file.
	// If CommitWALRotation is not called (crash), the old WAL is replayed on
	// next restart alongside the current WAL.
	RotateWAL() (oldWALPath string, err error)

	// CommitWALRotation deletes a rotated WAL file after its events have been
	// flushed to the database. This confirms the data is safely in the DB.
	CommitWALRotation(oldWALPath string) error

	// Dropped returns the count of events lost due to WAL write failures.
	Dropped() int64

	// Close shuts down the event bus and flushes any pending WAL.
	Close() error
}
