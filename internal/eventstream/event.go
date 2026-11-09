// Package eventstream defines the runtime change-event model and the bus
// contract shared by persistence, routing, and analytics adapters.
package eventstream

import (
	"time"

	"github.com/asenawritescode/kora/contract"
)

// EventOp describes the type of document change.
type EventOp string

const (
	EventInsert       EventOp = "insert"
	EventUpdate       EventOp = "update"
	EventDelete       EventOp = "delete"
	EventSubmit       EventOp = "submit"
	EventCancel       EventOp = "cancel"
	EventNotification EventOp = "notification"
)

// ChangeEvent carries a document mutation or a recipient-scoped realtime
// notification between the persistence and delivery layers.
type ChangeEvent struct {
	ID            string                 `json:"id,omitempty"`
	Sequence      string                 `json:"sequence,omitempty"`
	Site          string                 `json:"site"`
	BusinessScope contract.BusinessScope `json:"business_scope,omitempty"`
	Doctype       string                 `json:"doctype"`
	DocName       string                 `json:"doc_name"`
	Operation     EventOp                `json:"operation"`
	Timestamp     time.Time              `json:"timestamp"`
	ModifiedBy    string                 `json:"modified_by"`
	Data          map[string]any         `json:"data"`
	OldData       map[string]any         `json:"old_data,omitempty"`
}

// Bus publishes and consumes durable change events for one site.
type Bus interface {
	Publish(ChangeEvent) error
	Subscribe() (<-chan ChangeEvent, error)
	DrainWAL(func(ChangeEvent)) (int, error)
	RotateWAL() (string, error)
	CommitWALRotation(string) error
	Dropped() int64
	Close() error
}

// WALAcknowledger acknowledges only event IDs included in a committed batch.
type WALAcknowledger interface {
	AcknowledgeWAL(eventIDs []string) error
}
