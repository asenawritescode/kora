package contract

import "sort"

// CapabilityStatus is the status vocabulary from KORA-ENGINE-RFC.md:
//
//	planned       specified but not implemented
//	experimental  implemented behind an explicit feature flag, no compatibility promise
//	supported     contract, recovery, security, and observability tests pass in a stated mode
//	retired       no new callers may depend on it
//
//contract:enum
type CapabilityStatus string

const (
	CapabilityPlanned      CapabilityStatus = "planned"
	CapabilityExperimental CapabilityStatus = "experimental"
	CapabilitySupported    CapabilityStatus = "supported"
	CapabilityRetired      CapabilityStatus = "retired"
)

// BlockingRisk marks a capability whose risk (from RFC §18.1) prevents it from
// being advertised as supported.
//
//contract:enum
type BlockingRisk string

const (
	RiskTenantIsolation BlockingRisk = "tenant-isolation"
	RiskAuthorization   BlockingRisk = "authorization"
	RiskIdentity        BlockingRisk = "identity"
	RiskCredentialScope BlockingRisk = "credential-scope"
	RiskDurableEffects  BlockingRisk = "durable-business-effects"
)

// Capability is a named, versioned contract or feature whose implementation and
// production-readiness must be tracked per the RFC handoff rule.
//
//contract:wire
type Capability struct {
	Name        string
	Description string
	Status      CapabilityStatus
	Risks       []BlockingRisk
}

// Runtime is the capability registry. Implementations register capabilities and,
// before marking anything supported, must resolve every blocking risk and attach
// test evidence.
type Runtime struct {
	caps map[string]Capability
}

// NewRuntime returns an empty capability registry.
func NewRuntime() *Runtime {
	return &Runtime{caps: make(map[string]Capability)}
}

// Register records a capability. It panics on a duplicate name so that a name
// cannot silently be overwritten with a conflicting status.
func (r *Runtime) Register(c Capability) {
	if _, exists := r.caps[c.Name]; exists {
		panic("contract: capability already registered: " + c.Name)
	}
	r.caps[c.Name] = c
}

// Get returns a capability by name and whether it exists.
func (r *Runtime) Get(name string) (Capability, bool) {
	c, ok := r.caps[name]
	return c, ok
}

// Status returns the status of a named capability. Unknown capabilities are
// "planned" so that a missing registration never reads as "supported".
func (r *Runtime) Status(name string) CapabilityStatus {
	if c, ok := r.caps[name]; ok {
		return c.Status
	}
	return CapabilityPlanned
}

// SetStatus updates the status of a registered capability. Unknown capabilities
// are registered without a description. It is a no-op with a false return if the
// capability is not found.
func (r *Runtime) SetStatus(name string, status CapabilityStatus) bool {
	c, ok := r.caps[name]
	if !ok {
		return false
	}
	c.Status = status
	r.caps[name] = c
	return true
}

// Blocked reports whether any registered capability still carries a blocking risk
// while marked supported.
func (r *Runtime) Blocked() []Capability {
	var blocked []Capability
	for _, c := range r.caps {
		if c.Status == CapabilitySupported && len(c.Risks) > 0 {
			blocked = append(blocked, c)
		}
	}
	return blocked
}

// Names returns the sorted list of registered capability names.
func (r *Runtime) Names() []string {
	names := make([]string, 0, len(r.caps))
	for name := range r.caps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BaselineCapabilities returns the canonical capability list for the current
// (Phase 0) implementation state. It is the single source of truth for the
// status vocabulary until later phases register their contracts.
func BaselineCapabilities() []Capability {
	return []Capability{
		{
			Name:        "contract.event_envelope",
			Description: "Versioned provider-neutral event envelope (RFC §7).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "contract.command_envelope",
			Description: "Versioned provider-neutral command envelope (RFC §7).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "contract.actor_context",
			Description: "Identity, delegation, and operation identity (RFC §7.3).",
			Status:      CapabilityExperimental,
			Risks:       []BlockingRisk{RiskIdentity, RiskAuthorization},
		},
		{
			Name:        "provider.nats",
			Description: "Self-hosted NATS JetStream execution fabric (RFC §9, Phase 2).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "outbox.transactional",
			Description: "Transactional outbox for durable delivery (RFC §8.1, Phase 1).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "auth.oidc",
			Description: "OIDC authorization-code + PKCE profile (RFC §17, Phase 1A).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "ai.chat",
			Description: "Context-scoped AI chat with durable transcripts, run tracking, and approval workflow (RFC §10.4).",
			Status:      CapabilityExperimental,
			Risks:       []BlockingRisk{RiskAuthorization, RiskDurableEffects},
		},
		{
			Name:        "ai.mcp",
			Description: "Model Context Protocol execution (RFC §10.4).",
			Status:      CapabilityExperimental,
			Risks:       []BlockingRisk{RiskAuthorization, RiskCredentialScope},
		},
		{
			Name:        "workflow.actor",
			Description: "Actor-based workflows with lease/fencing (RFC §10, Phase 3).",
			Status:      CapabilitySupported,
		},
		{
			Name:        "offline.sync",
			Description: "Branch/device offline synchronization (RFC §12, Phase 5).",
			Status:      CapabilitySupported,
		},
	}
}
