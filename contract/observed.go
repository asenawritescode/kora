// Package contract — observed-state and reconciliation wire models (RECON-001).
//
// Runtimes report authenticated observed state while Cloud sends signed desired
// generations. These typed models are the authoritative interchange: JSON and
// NATS are transport only, and no map[string]any crosses a package boundary.
package contract

import "time"

// ObservationStatus is the runtime-reported health of a component.
//
//contract:enum
type ObservationStatus string

const (
	ObservationHealthy  ObservationStatus = "healthy"
	ObservationDegraded ObservationStatus = "degraded"
	ObservationFailed   ObservationStatus = "failed"
	ObservationWaiting  ObservationStatus = "waiting"
)

// ComponentObservation is the observed state of one resource. Detail is a
// bounded, redacted human-readable string — never a payload or secret.
//
//contract:wire
type ComponentObservation struct {
	Ref        ResourceRef       `json:"ref"`
	Status     ObservationStatus `json:"status"`
	Generation int               `json:"generation"`
	Detail     string            `json:"detail,omitempty"`
	SeenAt     time.Time         `json:"seen_at"`
}

// ObservationReport is an authenticated, runtime-authored batch of component
// observations. Signature covers the canonical report body (excluding the
// Signature field); verification happens at the ingestion boundary.
//
//contract:wire
type ObservationReport struct {
	RuntimeID  string                 `json:"runtime_id"`
	TenantID   string                 `json:"tenant_id"`
	Generation int                    `json:"generation"`
	Components []ComponentObservation `json:"components"`
	ReportedAt time.Time              `json:"reported_at"`
	Signature  []byte                 `json:"signature,omitempty"`
}
