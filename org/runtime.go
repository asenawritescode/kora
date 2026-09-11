// Package org contains the provider-neutral organizational execution seam.
// Domain packages register capabilities; humans, agents, workflows, and
// integrations all enter through Execute.
package org

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/asenawritescode/kora/contract"
)

type Capability struct {
	Contract contract.CapabilityContract
	Handler  Handler
}

type Grant struct {
	Capability       string
	ActorType        contract.PrincipalType
	ActorID          string
	ExpiresAt        time.Time
	Prohibited       bool
	RequiresApproval bool
}

type Intent struct {
	ID            string
	Site          string
	Actor         contract.ActorContext
	Capability    contract.ResourceRef
	Arguments     json.RawMessage
	CorrelationID string
	ApprovalID    string
}

type Execution struct {
	OperationID string
	Event       contract.EventEnvelope
	Result      json.RawMessage
}

type Handler func(context.Context, Intent) (json.RawMessage, error)

type Runtime struct {
	mu         sync.RWMutex
	capability map[string]Capability
	grants     []Grant
	publisher  contract.EventPublisher
	provenance *ProvenanceStore
	approvals  map[string]bool
}

func NewRuntime(publisher contract.EventPublisher) *Runtime {
	return &Runtime{capability: make(map[string]Capability), publisher: publisher, provenance: NewProvenanceStore(), approvals: make(map[string]bool)}
}

func (r *Runtime) Provenance() *ProvenanceStore { return r.provenance }

func (r *Runtime) RegisterCapability(c Capability) error {
	if c.Contract.Ref.Name == "" || c.Contract.Ref.Namespace == "" || c.Contract.Ref.Version <= 0 || c.Handler == nil {
		return contract.NewError(contract.CodeValidationFailed, "capability requires a versioned ref and handler")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := c.Contract.Ref.String()
	if _, exists := r.capability[key]; exists {
		return fmt.Errorf("%w: capability %s", contract.ErrResourceVersionConflict, key)
	}
	r.capability[key] = c
	return nil
}

func (r *Runtime) Grant(g Grant) error {
	if g.Capability == "" || g.ActorID == "" || g.ActorType == "" {
		return contract.NewError(contract.CodeValidationFailed, "grant requires capability and actor")
	}
	r.mu.Lock()
	r.grants = append(r.grants, g)
	r.mu.Unlock()
	return nil
}

func (r *Runtime) authorized(intent Intent, now time.Time) (bool, bool, string) {
	for _, g := range r.grants {
		if g.Capability != intent.Capability.String() || g.ActorID != intent.Actor.PrincipalID || g.ActorType != intent.Actor.PrincipalType {
			continue
		}
		if g.Prohibited || (!g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt)) {
			return false, false, "capability is prohibited or expired"
		}
		return true, g.RequiresApproval, "explicit capability grant"
	}
	return false, false, "no explicit capability grant"
}

// Approve records a durable approval for one operation. Approval is bound to
// the operation ID and is consumed only by an intent with that same ID.
func (r *Runtime) Approve(operationID string) error {
	if operationID == "" {
		return contract.NewError(contract.CodeValidationFailed, "operation id is required")
	}
	r.mu.Lock()
	r.approvals[operationID] = true
	r.mu.Unlock()
	return nil
}

func (r *Runtime) Execute(ctx context.Context, intent Intent) (Execution, error) {
	if !intent.Actor.Authenticated() {
		return Execution{}, contract.NewError(contract.CodeUnauthenticated, "authenticated actor is required")
	}
	if intent.Site == "" || intent.Capability.Namespace == "" || intent.Capability.Version <= 0 {
		return Execution{}, contract.NewError(contract.CodeValidationFailed, "site and versioned capability are required")
	}
	r.mu.RLock()
	c, ok := r.capability[intent.Capability.String()]
	allowed, requiresApproval, reason := r.authorized(intent, time.Now().UTC())
	approved := r.approvals[intent.ID]
	r.mu.RUnlock()
	if !ok {
		return Execution{}, contract.NewError(contract.CodeNotFound, "capability is not registered")
	}
	if !allowed {
		return Execution{}, contract.NewError(contract.CodePermissionDenied, reason)
	}
	if requiresApproval && !approved {
		return Execution{}, contract.NewError(contract.CodePermissionDenied, "human approval is required")
	}
	result, err := c.Handler(ctx, intent)
	if err != nil {
		return Execution{}, err
	}
	provenanceID := contract.NewID()
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "capability.executed", Version: 1, Source: "kora.org", Site: intent.Site, AggregateType: string(contract.ResourceKindCapability), AggregateID: intent.Capability.String(), CorrelationID: intent.CorrelationID, CausationID: intent.ID, Actor: intent.Actor, Capability: intent.Capability, ProvenanceIDs: []string{provenanceID}, OccurredAt: time.Now().UTC(), Data: result}
	if err := event.Validate(); err != nil {
		return Execution{}, err
	}
	if err := r.provenance.Append(contract.ProvenanceRecord{ID: provenanceID, Resource: intent.Capability, Actor: intent.Actor, Capability: intent.Capability, Output: string(result), EventID: event.ID, RecordedAt: event.OccurredAt}); err != nil {
		return Execution{}, fmt.Errorf("record execution provenance: %w", err)
	}
	if r.publisher != nil {
		if err := r.publisher.Publish(ctx, event); err != nil {
			return Execution{}, fmt.Errorf("publish execution event: %w", err)
		}
	}
	return Execution{OperationID: intent.ID, Event: event, Result: result}, nil
}
