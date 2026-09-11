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

type ServiceScope string

const (
	ScopeTenant    ServiceScope = "tenant"
	ScopeSession   ServiceScope = "session"
	ScopeWorkflow  ServiceScope = "workflow"
	ScopeExecution ServiceScope = "execution"
)

type PluginService struct {
	Name        string       `json:"name"`
	Provider    ResourceRef  `json:"provider"`
	Scope       ServiceScope `json:"scope"`
	Permissions []string     `json:"permissions,omitempty"`
	Cleanup     []string     `json:"cleanup,omitempty"`
}

type ProvenanceRecord struct {
	ID         string        `json:"id"`
	Resource   ResourceRef   `json:"resource"`
	Inputs     []ResourceRef `json:"inputs,omitempty"`
	Rule       ResourceRef   `json:"rule,omitempty"`
	Actor      ActorContext  `json:"actor"`
	Capability ResourceRef   `json:"capability,omitempty"`
	Output     string        `json:"output,omitempty"`
	EventID    string        `json:"event_id"`
	RecordedAt time.Time     `json:"recorded_at"`
}

type AgentManifest struct {
	Ref              ResourceRef   `json:"ref"`
	Allowed          []ResourceRef `json:"allowed,omitempty"`
	Prohibited       []ResourceRef `json:"prohibited,omitempty"`
	ApprovalRequired []ResourceRef `json:"approval_required,omitempty"`
	TimeoutSeconds   int           `json:"timeout_seconds,omitempty"`
	MaxRetries       int           `json:"max_retries,omitempty"`
}

type ToolCall struct {
	ID         string      `json:"id"`
	Agent      ResourceRef `json:"agent"`
	Capability ResourceRef `json:"capability"`
	State      string      `json:"state"`
	ApprovalID string      `json:"approval_id,omitempty"`
	StartedAt  time.Time   `json:"started_at"`
	FinishedAt time.Time   `json:"finished_at,omitempty"`
}

// AgentAllows applies the deny-by-default agent policy. Approval-required
// capabilities are reported separately so callers can create a durable human
// handoff instead of silently executing them.
func (m AgentManifest) AgentAllows(ref ResourceRef) (allowed bool, approvalRequired bool) {
	for _, denied := range m.Prohibited {
		if denied == ref {
			return false, false
		}
	}
	for _, approval := range m.ApprovalRequired {
		if approval == ref {
			return false, true
		}
	}
	for _, granted := range m.Allowed {
		if granted == ref {
			return true, false
		}
	}
	return false, false
}

type PackageManifest struct {
	Ref         ResourceRef   `json:"ref"`
	Provides    []ResourceRef `json:"provides,omitempty"`
	Requires    []ResourceRef `json:"requires,omitempty"`
	Permissions []string      `json:"permissions,omitempty"`
	Migrations  []string      `json:"migrations,omitempty"`
	Cleanup     []string      `json:"cleanup,omitempty"`
}
