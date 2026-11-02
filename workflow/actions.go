package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

// ActionKey identifies an immutable, versioned workflow action adapter.
type ActionKey struct {
	Name    string
	Version uint32
}

type ActionInput struct {
	Schema contract.SchemaRef `json:"schema"`
	Data   json.RawMessage    `json:"data"`
}

type ActionResult struct {
	Schema contract.SchemaRef `json:"schema"`
	Data   json.RawMessage    `json:"data"`
}

type ActionContext struct {
	Execution      policy.Execution
	BusinessScope  contract.BusinessScope
	RequiredGrant  policy.Grant
	InstanceID     contract.InstanceID
	StepID         contract.StepID
	ExecutionID    string
	Subject        contract.SubjectRef
	IdempotencyKey contract.IdempotencyKey
}

type Action interface {
	Execute(context.Context, ActionContext, ActionInput) (ActionResult, error)
}

// ExternalFailureKind records only what an adapter can prove about an
// unsuccessful provider call. Unclassified errors remain outcome-unknown.
type ExternalFailureKind uint8

const (
	ExternalOutcomeUnknown ExternalFailureKind = iota
	ExternalNotAppliedRetryable
	ExternalNotAppliedPermanent
)

// ExternalActionFailure may be returned only when the adapter has evidence for
// the stated outcome. Wrapping a network error as NotApplied is unsafe unless
// the provider confirms rejection before applying the effect.
type ExternalActionFailure struct {
	Kind  ExternalFailureKind
	Cause error
}

func (e *ExternalActionFailure) Error() string {
	if e == nil || e.Cause == nil {
		return "external workflow action failed"
	}
	return e.Cause.Error()
}

func (e *ExternalActionFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type ActionClass string

const (
	ActionLocal    ActionClass = "local_transaction"
	ActionExternal ActionClass = "external"
)

// LocalActionExecutor must atomically validate the claim, invoke the domain
// operation, and persist workflow progress with its idempotency receipt.
type LocalActionExecutor interface {
	ExecuteLocal(context.Context, Claim, ActionContext, ActionKey, ActionInput, contract.StepID) (ActionResult, error)
}

type LocalActionFailureKind uint8

const (
	// LocalActionNotAppliedRetryable means the action transaction was rolled
	// back before its commit request; retrying cannot duplicate a local effect.
	LocalActionNotAppliedRetryable LocalActionFailureKind = iota + 1
	// LocalActionNotAppliedPermanent is a deterministic domain or validation
	// rejection known to have rolled back.
	LocalActionNotAppliedPermanent
	// LocalActionOutcomeUnknown means commit may have reached the database and
	// an operator-visible uncertain outcome must be persisted.
	LocalActionOutcomeUnknown
)

// LocalActionFailure carries transaction outcome evidence from an adapter.
// Plain errors remain conservatively uncertain for compatibility.
type LocalActionFailure struct {
	Kind  LocalActionFailureKind
	Cause error
}

func (e *LocalActionFailure) Error() string {
	if e == nil || e.Cause == nil {
		return "local workflow action failed"
	}
	return e.Cause.Error()
}

func (e *LocalActionFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type ActionRegistration struct {
	Key           ActionKey
	Class         ActionClass
	InputSchema   contract.SchemaRef
	OutputSchema  contract.SchemaRef
	RequiredGrant policy.Grant
	External      Action
	Local         LocalActionExecutor
}

// Catalog is mutable only during assembly. Freeze is required before lookup
// and makes the action set stable for published definition validation.
type Catalog struct {
	mu      sync.RWMutex
	entries map[ActionKey]ActionRegistration
	frozen  bool
}

func NewCatalog() *Catalog { return &Catalog{entries: make(map[ActionKey]ActionRegistration)} }

func (c *Catalog) Register(reg ActionRegistration) error {
	if c == nil {
		return fmt.Errorf("action catalog is nil")
	}
	if reg.Key.Name == "" || len(reg.Key.Name) > 160 || strings.TrimSpace(reg.Key.Name) != reg.Key.Name || reg.Key.Version == 0 || reg.InputSchema.ID == "" || reg.InputSchema.Version == 0 || reg.OutputSchema.ID == "" || reg.OutputSchema.Version == 0 {
		return fmt.Errorf("action key and input/output schema versions are required")
	}
	if !reg.RequiredGrant.Valid() {
		return fmt.Errorf("action %s@%d requires one exact valid service grant", reg.Key.Name, reg.Key.Version)
	}
	if (reg.Class == ActionExternal && (reg.External == nil || reg.Local != nil)) || (reg.Class == ActionLocal && (reg.Local == nil || reg.External != nil)) || (reg.Class != ActionExternal && reg.Class != ActionLocal) {
		return fmt.Errorf("action adapter does not match declared class")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frozen {
		return fmt.Errorf("action catalog is frozen")
	}
	if _, ok := c.entries[reg.Key]; ok {
		return fmt.Errorf("duplicate action %s@%d", reg.Key.Name, reg.Key.Version)
	}
	c.entries[reg.Key] = reg
	return nil
}

func (c *Catalog) Resolve(key ActionKey) (ActionRegistration, error) {
	if c == nil {
		return ActionRegistration{}, fmt.Errorf("action catalog is nil")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.frozen {
		return ActionRegistration{}, fmt.Errorf("action catalog must be frozen before use")
	}
	reg, ok := c.entries[key]
	if !ok {
		return ActionRegistration{}, fmt.Errorf("unknown action %s@%d", key.Name, key.Version)
	}
	return reg, nil
}

// Registrations returns a stable, caller-owned snapshot for capability
// discovery. Adapter values are intentionally omitted from the public shape;
// callers may expose only Key, Class and schema metadata.
func (c *Catalog) Registrations() []ActionRegistration {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	registrations := make([]ActionRegistration, 0, len(c.entries))
	for _, registration := range c.entries {
		registrations = append(registrations, registration)
	}
	sort.Slice(registrations, func(i, j int) bool {
		if registrations[i].Key.Name == registrations[j].Key.Name {
			return registrations[i].Key.Version < registrations[j].Key.Version
		}
		return registrations[i].Key.Name < registrations[j].Key.Name
	})
	return registrations
}

func (c *Catalog) Freeze() error {
	if c == nil {
		return fmt.Errorf("action catalog is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen = true
	return nil
}

func (c *Catalog) Frozen() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.frozen
}

func (c *Catalog) ValidateSchemas(schemas PayloadSchemaRegistry) error {
	if c == nil || schemas == nil {
		return fmt.Errorf("catalog and schema registry are required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.frozen {
		return fmt.Errorf("action catalog must be frozen")
	}
	for key, reg := range c.entries {
		if !schemas.Registered(reg.InputSchema) {
			return fmt.Errorf("action %s@%d input schema %s@%d is not registered", key.Name, key.Version, reg.InputSchema.ID, reg.InputSchema.Version)
		}
		if !schemas.Registered(reg.OutputSchema) {
			return fmt.Errorf("action %s@%d output schema %s@%d is not registered", key.Name, key.Version, reg.OutputSchema.ID, reg.OutputSchema.Version)
		}
	}
	return nil
}

type PayloadSchemaRegistry interface {
	Registered(contract.SchemaRef) bool
	Validate(contract.SchemaRef, json.RawMessage) error
}

type WaitFieldRegistry interface {
	RegisteredCorrelationField(eventType, field string) bool
}
