package contract

import "time"

// CapabilityContract describes what may be done without prescribing its
// executor.
type CapabilityContract struct {
	Ref         ResourceRef     `json:"ref"`
	Description string          `json:"description,omitempty"`
	Inputs      []TypedField    `json:"inputs,omitempty"`
	Outputs     []TypedField    `json:"outputs,omitempty"`
	Requires    []ResourceRef   `json:"requires,omitempty"`
	Effects     []string        `json:"effects,omitempty"`
	Risk        ToolSafetyLevel `json:"risk,omitempty"`
}

// SkillContract is a versioned procedure attached to a capability.
type SkillContract struct {
	Ref        ResourceRef `json:"ref"`
	Capability ResourceRef `json:"capability"`
	Steps      []string    `json:"steps,omitempty"`
	Escalation string      `json:"escalation,omitempty"`
}

type PolicyDecision string

const (
	PolicyAllow   PolicyDecision = "allow"
	PolicyDeny    PolicyDecision = "deny"
	PolicyApprove PolicyDecision = "requires_approval"
)

// PolicyEvaluation is safe to persist as provenance for an operation.
type PolicyEvaluation struct {
	Decision    PolicyDecision `json:"decision"`
	Capability  ResourceRef    `json:"capability"`
	Actor       ActorContext   `json:"actor"`
	Policy      ResourceRef    `json:"policy,omitempty"`
	Reason      string         `json:"reason,omitempty"`
	EvaluatedAt time.Time      `json:"evaluated_at"`
}
