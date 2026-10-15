// Package contract - desired-state and reconciliation wire models (RECON-002).
//
// Cloud and runtime exchange desired state as typed records. These are the
// authoritative shapes for configuration intent; JSON and NATS are transport
// only, and reconciliation code should avoid ad hoc maps.
package contract

import "time"

// DesiredStatus describes the current lifecycle posture of a desired state set.
//
//contract:enum
type DesiredStatus string

const (
	DesiredPlanned    DesiredStatus = "planned"
	DesiredActive     DesiredStatus = "active"
	DesiredBlocked    DesiredStatus = "blocked"
	DesiredSuperseded DesiredStatus = "superseded"
)

// DesiredResource represents one resource the platform intends to have active
// at a target generation.
//
//contract:wire
type DesiredResource struct {
	Ref          ResourceRef   `json:"ref"`
	Required     bool          `json:"required,omitempty"`
	Dependencies []ResourceRef `json:"dependencies,omitempty"`
}

// DesiredReport is the signed desired-state batch published by Cloud or an
// activation controller.
//
//contract:wire
type DesiredReport struct {
	DeploymentID string            `json:"deployment_id"`
	TenantID     string            `json:"tenant_id"`
	Generation   int               `json:"generation"`
	Status       DesiredStatus     `json:"status"`
	Resources    []DesiredResource `json:"resources"`
	CreatedAt    time.Time         `json:"created_at"`
	Signature    []byte            `json:"signature,omitempty"`
}
