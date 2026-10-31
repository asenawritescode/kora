package contract

import (
	"encoding/json"
	"time"
)

// OfflineOperationStatus is the local and server-side lifecycle of an offline
// operation. The RFC requires rejected operations to remain visible as conflict
// records rather than disappear.
//
//contract:enum
type OfflineOperationStatus string

const (
	OfflineOperationQueued    OfflineOperationStatus = "queued"
	OfflineOperationSent      OfflineOperationStatus = "sent"
	OfflineOperationAcked     OfflineOperationStatus = "acked"
	OfflineOperationConflict  OfflineOperationStatus = "conflict"
	OfflineOperationRejected  OfflineOperationStatus = "rejected"
	OfflineOperationDiscarded OfflineOperationStatus = "discarded"
)

// OfflineSchemaGate records whether a client schema bundle is accepted.
//
//contract:enum
type OfflineSchemaGate string

const (
	OfflineSchemaGateAccepted OfflineSchemaGate = "accepted"
	OfflineSchemaGateRejected OfflineSchemaGate = "rejected"
	OfflineSchemaGateStale    OfflineSchemaGate = "stale"
)

// OfflineOperation records one device/branch operation for later sync.
//
//contract:wire
type OfflineOperation struct {
	ID            string                 `json:"id"`
	DeviceID      string                 `json:"device_id"`
	BranchID      string                 `json:"branch_id"`
	Site          string                 `json:"site"`
	EntityType    string                 `json:"entity_type"`
	EntityID      string                 `json:"entity_id,omitempty"`
	OperationType string                 `json:"operation_type"`
	BaseVersion   int                    `json:"base_version"`
	OccurredAt    time.Time              `json:"occurred_at"`
	SchemaVersion string                 `json:"schema_version"`
	Status        OfflineOperationStatus `json:"status"`
	//contract:json
	Payload  json.RawMessage   `json:"payload,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// OfflineConflict captures a rejected or stale operation and the server-side
// reason that must stay auditable.
//
//contract:wire
type OfflineConflict struct {
	ID             string    `json:"id"`
	OperationID    string    `json:"operation_id"`
	DeviceID       string    `json:"device_id"`
	BranchID       string    `json:"branch_id"`
	Site           string    `json:"site"`
	EntityType     string    `json:"entity_type"`
	EntityID       string    `json:"entity_id,omitempty"`
	ReasonCode     Code      `json:"reason_code"`
	ResolutionMode string    `json:"resolution_mode"`
	ServerVersion  int       `json:"server_version"`
	RecordedAt     time.Time `json:"recorded_at"`
	//contract:json
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

// OfflineTombstone records a retained delete marker. Tombstones remain visible
// for the supported offline window so deletes can be replayed, retried, and
// rejected deterministically.
//
//contract:wire
type OfflineTombstone struct {
	ID            string    `json:"id"`
	DeviceID      string    `json:"device_id"`
	BranchID      string    `json:"branch_id"`
	Site          string    `json:"site"`
	EntityType    string    `json:"entity_type"`
	EntityID      string    `json:"entity_id"`
	DeletedAt     time.Time `json:"deleted_at"`
	RetainUntil   time.Time `json:"retain_until"`
	SchemaVersion string    `json:"schema_version"`
}

// SyncCursor is the opaque cursor shared by pull and acknowledgement flows.
//
//contract:wire
type SyncCursor struct {
	Token    string    `json:"token"`
	BranchID string    `json:"branch_id"`
	DeviceID string    `json:"device_id"`
	Version  int       `json:"version"`
	At       time.Time `json:"at"`
}

// OfflineSyncBatch groups operations under one schema gate result.
//
//contract:wire
type OfflineSyncBatch struct {
	SchemaVersion string             `json:"schema_version"`
	Gate          OfflineSchemaGate  `json:"gate"`
	Operations    []OfflineOperation `json:"operations"`
	Conflicts     []OfflineConflict  `json:"conflicts,omitempty"`
	Cursor        SyncCursor         `json:"cursor"`
	NextCursor    SyncCursor         `json:"next_cursor,omitempty"`
}

// AcceptsOfflineSchema reports whether the server should accept a bundle.
func AcceptsOfflineSchema(gate OfflineSchemaGate) bool {
	return gate == OfflineSchemaGateAccepted
}

// ConflictResolutionRetryable reports whether a rejected operation should stay
// available for another sync attempt or manual resolution.
func ConflictResolutionRetryable(mode string) bool {
	switch mode {
	case "retry", "merge", "manual_review":
		return true
	default:
		return false
	}
}

// TombstoneRetained reports whether the tombstone still falls within the
// supported offline retention window.
func TombstoneRetained(t OfflineTombstone, now time.Time) bool {
	if t.RetainUntil.IsZero() {
		return false
	}
	return !now.After(t.RetainUntil)
}

// PruneExpiredTombstones removes tombstones that have aged past their
// retention window so GC can keep the retained delete set bounded.
func PruneExpiredTombstones(tombstones []OfflineTombstone, now time.Time) []OfflineTombstone {
	kept := make([]OfflineTombstone, 0, len(tombstones))
	for _, t := range tombstones {
		if TombstoneRetained(t, now) {
			kept = append(kept, t)
		}
	}
	return kept
}

// RetainConflict reports whether a rejected operation should remain visible to
// operators or the retry loop.
func RetainConflict(conflict OfflineConflict) bool {
	return ConflictResolutionRetryable(conflict.ResolutionMode)
}

// StaleWrite reports whether the incoming offline write is based on an older
// version than the server currently accepts.
func StaleWrite(baseVersion, serverVersion int) bool {
	return baseVersion > 0 && baseVersion < serverVersion
}

// DuplicateAck reports whether a cursor acknowledgement is a repeat of a
// previously acknowledged version.
func DuplicateAck(previous, next SyncCursor) bool {
	return previous.Token != "" && previous.Token == next.Token && previous.Version == next.Version
}

// SkippedSchemaVersion reports whether the client jumped over a required
// intermediate schema version.
func SkippedSchemaVersion(client, current string, accepted []string) bool {
	if client == "" || current == "" {
		return true
	}
	if client == current {
		return false
	}
	for _, version := range accepted {
		if version == client {
			return false
		}
	}
	return true
}

// OperationCanTransition reports whether an offline operation can move to the
// next local lifecycle state without losing audit visibility.
func OperationCanTransition(from, to OfflineOperationStatus) bool {
	switch from {
	case OfflineOperationQueued:
		return to == OfflineOperationSent || to == OfflineOperationRejected || to == OfflineOperationDiscarded
	case OfflineOperationSent:
		return to == OfflineOperationAcked || to == OfflineOperationConflict || to == OfflineOperationRejected
	case OfflineOperationAcked:
		return false
	case OfflineOperationConflict:
		return to == OfflineOperationQueued || to == OfflineOperationDiscarded
	case OfflineOperationRejected:
		return to == OfflineOperationDiscarded
	case OfflineOperationDiscarded:
		return false
	default:
		return false
	}
}

// GenerationInfo identifies the projection generation a client holds
// (OFFLINE-001). ConfigVersion bumps on every activated config change;
// ProjectionHash identifies the derived local schema.
//
//contract:wire
type GenerationInfo struct {
	Site           string `json:"site"`
	ConfigVersion  int64  `json:"config_version"`
	ProjectionHash string `json:"projection_hash"`
}

// StaleProjectionError is a typed rejection when a client's projection is stale
// or missing a required capability. It is never a bare string error.
type StaleProjectionError struct {
	Kind              string `json:"kind"` // "stale_generation" | "capability_missing"
	ClientGeneration  int64  `json:"client_generation"`
	ServerGeneration  int64  `json:"server_generation"`
	MissingCapability string `json:"missing_capability,omitempty"`
}

func (e *StaleProjectionError) Error() string {
	switch e.Kind {
	case "capability_missing":
		return "offline projection missing capability " + e.MissingCapability
	default:
		return "offline projection stale generation"
	}
}

// CheckProjectionGate compares a client's generation and capability set against
// the server and returns a typed *StaleProjectionError on any mismatch. A nil
// return means the projection is accepted.
func CheckProjectionGate(client, server GenerationInfo, requiredCaps, clientCaps []string) *StaleProjectionError {
	if client.ConfigVersion < server.ConfigVersion {
		return &StaleProjectionError{
			Kind:             "stale_generation",
			ClientGeneration: client.ConfigVersion,
			ServerGeneration: server.ConfigVersion,
		}
	}
	if client.ProjectionHash != "" && server.ProjectionHash != "" && client.ProjectionHash != server.ProjectionHash {
		return &StaleProjectionError{
			Kind:             "stale_generation",
			ClientGeneration: client.ConfigVersion,
			ServerGeneration: server.ConfigVersion,
		}
	}

	clientSet := make(map[string]bool, len(clientCaps))
	for _, c := range clientCaps {
		clientSet[c] = true
	}
	for _, req := range requiredCaps {
		if !clientSet[req] {
			return &StaleProjectionError{
				Kind:              "capability_missing",
				ClientGeneration:  client.ConfigVersion,
				ServerGeneration:  server.ConfigVersion,
				MissingCapability: req,
			}
		}
	}
	return nil
}
