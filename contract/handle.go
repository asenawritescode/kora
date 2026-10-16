// Package contract — capability handle contract (PLUGIN-001).
//
// Components never construct providers directly; they request capabilities and
// receive handles. The issuer grants only what the component's manifest
// declared (deny-by-default), scoped to component, operation, or call. A handle
// carries an owner, a scope, and cleanup evidence so every effect is
// attributable and releasable (invariant 6/7).
package contract

import (
	"fmt"
	"time"
)

// CapabilityName identifies a grantable capability. Values are stable and used
// as telemetry labels, so additions are fine but renames are not.
//
//contract:enum
type CapabilityName string

// GrantScope bounds the lifetime of an issued handle.
//
//contract:enum
type GrantScope string

const (
	GrantScopeComponent GrantScope = "component" // lives as long as the component is bound
	GrantScopeOperation GrantScope = "operation" // lives for one operation/kernel invocation
	GrantScopeCall      GrantScope = "call"      // lives for one provider call
)

// OperationContext is the minimal, explicit execution context for capability
// issuance. It is distinct from the kernel's richer OperationContext; the
// kernel adapts its context into this shape at the capability boundary.
type OperationContext struct {
	Site     string
	Actor    string
	TraceID  string
	Deadline time.Time
}

// CleanupEvidence records the ownership and release of a handle.
type CleanupEvidence struct {
	Owner      string
	ScopeID    string
	AcquiredAt time.Time
	ReleasedAt time.Time
}

// CapabilityHandle is a grant-checked reference to one capability. Revoke is
// idempotent and releases underlying resources via the handle's disposer.
type CapabilityHandle interface {
	Name() CapabilityName
	Scope() GrantScope
	Revoke()
	Evidence() CleanupEvidence
}

// HandleIssuer issues capability handles for a component. It returns a typed
// ErrCapabilityDenied for anything the component did not declare.
type HandleIssuer interface {
	Issue(ctx OperationContext, component string, want []CapabilityName) ([]CapabilityHandle, error)
}

// ErrCapabilityDenied is a typed denial carrying the component and the
// capability that was refused. Callers match via errors.As, never by string.
type ErrCapabilityDenied struct {
	Component  string
	Capability CapabilityName
}

func (e *ErrCapabilityDenied) Error() string {
	return fmt.Sprintf("capability %q denied for component %q", e.Capability, e.Component)
}

// CapabilityGrant declares that a component may receive a capability at a
// given scope.
type CapabilityGrant struct {
	Component  string
	Capability CapabilityName
	Scope      GrantScope
}

// StaticIssuer is a deny-by-default HandleIssuer backed by an explicit grant
// table. An undeclared capability is never issued, even read-only.
type StaticIssuer struct {
	grants map[string]map[CapabilityName]GrantScope
}

// NewStaticIssuer builds a StaticIssuer from grants. Duplicate grants for the
// same (component, capability) are resolved by last-write.
func NewStaticIssuer(grants []CapabilityGrant) *StaticIssuer {
	s := &StaticIssuer{grants: make(map[string]map[CapabilityName]GrantScope)}
	for _, g := range grants {
		if s.grants[g.Component] == nil {
			s.grants[g.Component] = make(map[CapabilityName]GrantScope)
		}
		s.grants[g.Component][g.Capability] = g.Scope
	}
	return s
}

// Issue grants exactly the requested capabilities that the component declared;
// any undeclared capability aborts the whole request with a typed denial. This
// is fail-closed: a mixed request does not partially succeed.
func (s *StaticIssuer) Issue(ctx OperationContext, component string, want []CapabilityName) ([]CapabilityHandle, error) {
	declared := s.grants[component]
	handles := make([]CapabilityHandle, 0, len(want))
	for _, name := range want {
		scope, ok := declared[name]
		if !ok {
			return nil, &ErrCapabilityDenied{Component: component, Capability: name}
		}
		handles = append(handles, newHandle(name, scope, component, ctx.TraceID))
	}
	return handles, nil
}

// handle is the concrete CapabilityHandle. It tracks acquisition/release and is
// revocable exactly once (idempotent on later calls).
type handle struct {
	name       CapabilityName
	scope      GrantScope
	owner      string
	scopeID    string
	acquiredAt time.Time
	releasedAt time.Time
	revoked    bool
}

func newHandle(name CapabilityName, scope GrantScope, owner, scopeID string) *handle {
	return &handle{name: name, scope: scope, owner: owner, scopeID: scopeID, acquiredAt: time.Now().UTC()}
}

func (h *handle) Name() CapabilityName { return h.name }
func (h *handle) Scope() GrantScope    { return h.scope }
func (h *handle) Revoke() {
	if h.revoked {
		return
	}
	h.revoked = true
	h.releasedAt = time.Now().UTC()
}

func (h *handle) Evidence() CleanupEvidence {
	return CleanupEvidence{Owner: h.owner, ScopeID: h.scopeID, AcquiredAt: h.acquiredAt, ReleasedAt: h.releasedAt}
}
