package contract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ModuleID identifies a durable business ownership boundary. It is independent
// of package identity, tenant identity, and ResourceRef namespace.
//
//contract:wire:v2
type ModuleID string

//contract:enum:v2
type ModuleAccess string

//contract:enum:v2
type MutationMode string

//contract:enum:v2
type OperationEffect string

const (
	ModulePrivate    ModuleAccess    = "private"
	ModulePublished  ModuleAccess    = "published"
	MutationCRUD     MutationMode    = "crud"
	MutationCommands MutationMode    = "commands_only"
	EffectQuery      OperationEffect = "query"
	EffectLocal      OperationEffect = "local_command"
	EffectExternal   OperationEffect = "external_command"
)

//contract:wire:v2
type OwnedResource struct {
	Ref          ResourceRef  `json:"ref"`
	Access       ModuleAccess `json:"access"`
	MutationMode MutationMode `json:"mutation_mode"`
}

//contract:wire:v2
type OperationSpec struct {
	Name   string          `json:"name"`
	Input  SchemaRef       `json:"input"`
	Output SchemaRef       `json:"output"`
	Effect OperationEffect `json:"effect"`
}

//contract:wire:v2
type CapabilityOffer struct {
	Name       CapabilityName  `json:"name"`
	Contract   SchemaRef       `json:"contract"`
	Operations []OperationSpec `json:"operations"`
}

//contract:wire:v2
type CapabilityRequirement struct {
	Name     CapabilityName `json:"name"`
	Contract SchemaRef      `json:"contract"`
	Required bool           `json:"required"`
}

// ModuleInstallation is the persisted lifecycle identity for one tenant module.
//
//contract:wire:v2
type ModuleInstallation struct {
	TenantID       TenantID `json:"tenant_id"`
	ModuleID       ModuleID `json:"module_id"`
	Version        string   `json:"version"`
	ArtifactDigest string   `json:"artifact_digest"`
	State          string   `json:"state"`
	Revision       uint64   `json:"revision"`
}

// ResourceOwnership is durable ownership of a canonical resource identity.
//
//contract:wire:v2
type ResourceOwnership struct {
	TenantID      TenantID    `json:"tenant_id"`
	Resource      ResourceRef `json:"resource"`
	OwnerModuleID ModuleID    `json:"owner_module_id"`
}

// ModuleSnapshot is a complete candidate/published ownership state.
//
//contract:wire:v2
type ModuleSnapshot struct {
	TenantID      TenantID             `json:"tenant_id"`
	Revision      uint64               `json:"revision"`
	Modules       []ModuleDescriptor   `json:"modules,omitempty"`
	Installations []ModuleInstallation `json:"installations"`
	Ownership     []ResourceOwnership  `json:"ownership"`
}

// ModuleStore is the persistence boundary used by activation. CommitActivation
// must atomically compare and increment the snapshot revision with the caller's
// activation transaction.
type ModuleStore interface {
	Load(tenant TenantID) (ModuleSnapshot, error)
}

// ModuleDescriptor is the canonical package level business module contract.
//
//contract:wire:v2
type ModuleDescriptor struct {
	ID        ModuleID                `json:"id"`
	Version   string                  `json:"version"`
	Resources []OwnedResource         `json:"resources,omitempty"`
	Provides  []CapabilityOffer       `json:"provides,omitempty"`
	Requires  []CapabilityRequirement `json:"requires,omitempty"`
}

func (m ModuleDescriptor) OwnerOf(ref ResourceRef) (OwnedResource, bool) {
	for _, resource := range m.Resources {
		if resource.Ref.Namespace == ref.Namespace && resource.Ref.Name == ref.Name {
			return resource, true
		}
	}
	return OwnedResource{}, false
}

// ModuleExecution is trusted in-process provenance minted by module dispatch.
// It must never be decoded from a user request.
type ModuleExecution struct {
	TenantID          TenantID
	CallerModuleID    ModuleID
	OwnerModuleID     ModuleID
	BindingRevision   uint64
	AllowedOperation  string
	AuthorizedCommand string
	ScopeKey          string
	verified          bool
}

// BusinessRef is a stable provider-qualified identity, never a local row name.
//
//contract:wire:v2
type BusinessRef struct {
	ProviderID   string `json:"provider_id"`
	ResourceType string `json:"resource_type"`
	ID           string `json:"id"`
}

// ReferenceRule declares whether a field points to a local row or a published
// provider-owned business identity.
//
//contract:wire:v2
type ReferenceRule struct {
	Kind           string         `json:"kind"`
	CapabilityName CapabilityName `json:"capability_name,omitempty"`
	ResourceType   string         `json:"resource_type"`
}

func (r ReferenceRule) Validate() error {
	switch r.Kind {
	case "local_owned":
		if r.ResourceType == "" || r.CapabilityName != "" {
			return fmt.Errorf("local-owned reference requires resource type and no capability")
		}
	case "published_business_ref":
		if r.ResourceType == "" || r.CapabilityName == "" {
			return fmt.Errorf("published reference requires resource type and capability")
		}
	default:
		return fmt.Errorf("unknown reference rule kind %q", r.Kind)
	}
	return nil
}

type ScopeContext struct {
	TenantID      TenantID
	LegalEntityID string
	ProviderID    string
}
type ResolvedBusinessRef struct {
	Canonical BusinessRef
	Revision  uint64
	Display   string
}
type BusinessReferenceResolver interface {
	Resolve(scope ScopeContext, ref BusinessRef) (ResolvedBusinessRef, error)
}

func ValidateBusinessReference(resolver BusinessReferenceResolver, scope ScopeContext, ref BusinessRef, expectedResourceType string) (ResolvedBusinessRef, error) {
	if scope.TenantID == "" || ref.ProviderID == "" || ref.ResourceType == "" || ref.ID == "" {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference requires verified scope and provider-qualified identity")
	}
	if ref.ResourceType != expectedResourceType {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference resource type mismatch")
	}
	if resolver == nil {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference provider is unavailable")
	}
	resolved, err := resolver.Resolve(scope, ref)
	if err != nil {
		return ResolvedBusinessRef{}, err
	}
	if resolved.Canonical != ref {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference provider returned a different identity")
	}
	return resolved, nil
}

// HistoricalSnapshot retains approved provider fields captured at issuance.
//
//contract:wire:v2
type HistoricalSnapshot struct {
	Source     BusinessRef `json:"source"`
	CapturedAt time.Time   `json:"captured_at"`
	Schema     SchemaRef   `json:"schema"`
	//contract:json
	Data json.RawMessage `json:"data"`
}

// BusinessEventDefinition ties an authoritative business event to its owning module.
//
//contract:wire:v2
type BusinessEventDefinition struct {
	Type          string    `json:"type"`
	PayloadSchema SchemaRef `json:"payload_schema"`
	OwnerModuleID ModuleID  `json:"owner_module_id"`
}

// CapabilityAction declares a schema-checked provider operation.
//
//contract:wire:v2
type CapabilityAction struct {
	CapabilityName CapabilityName `json:"capability_name"`
	OperationName  string         `json:"operation_name"`
	InputSchema    SchemaRef      `json:"input_schema"`
	OutputSchema   SchemaRef      `json:"output_schema"`
}

// PinnedDispatch records provider and operation identity before an effect attempt.
//
//contract:wire:v2
type PinnedDispatch struct {
	ExecutionID     string           `json:"execution_id"`
	TenantID        TenantID         `json:"tenant_id"`
	ScopeKey        string           `json:"scope_key"`
	CallerModuleID  ModuleID         `json:"caller_module_id"`
	ProviderID      string           `json:"provider_id"`
	BindingRevision uint64           `json:"binding_revision"`
	Operation       CapabilityAction `json:"operation"`
	IdempotencyKey  IdempotencyKey   `json:"idempotency_key"`
	InputHash       string           `json:"input_hash"`
}

type ExternalOutcome string

const (
	ExternalApplied    ExternalOutcome = "applied"
	ExternalNotApplied ExternalOutcome = "not_applied"
	ExternalUnknown    ExternalOutcome = "unknown"
)

// ConsumerDefinition declares owner-controlled event subscriptions.
//
//contract:wire:v2
type ConsumerDefinition struct {
	ConsumerID           string               `json:"consumer_id"`
	OwnerModuleID        ModuleID             `json:"owner_module_id"`
	AcceptedEventSchemas map[string]SchemaRef `json:"accepted_event_schemas"`
}

func ValidateBusinessEvent(definition BusinessEventDefinition, owner ModuleID, schema SchemaRef, eventType string) error {
	if definition.OwnerModuleID == "" || definition.OwnerModuleID != owner {
		return fmt.Errorf("business event owner does not match verified execution")
	}
	if definition.Type != eventType || definition.PayloadSchema != schema {
		return fmt.Errorf("business event type or payload schema mismatch")
	}
	if definition.Type == "" || definition.PayloadSchema.ID == "" || definition.PayloadSchema.Version == 0 {
		return fmt.Errorf("business event contract is incomplete")
	}
	return nil
}

func AppendBusinessEvent(ctx context.Context, writer interface {
	Append(context.Context, *sql.Tx, EventEnvelope) error
}, tx *sql.Tx, execution ModuleExecution, definition BusinessEventDefinition, payloadSchema SchemaRef, payload json.RawMessage, site, aggregateType, aggregateID, correlationID, causationID string) (string, error) {
	if !execution.Verified() || site != string(execution.TenantID) {
		return "", fmt.Errorf("business event requires verified matching module execution")
	}
	if execution.OwnerModuleID != definition.OwnerModuleID {
		return "", fmt.Errorf("business event can only be published by its owning module")
	}
	if err := ValidateBusinessEvent(definition, execution.OwnerModuleID, payloadSchema, definition.Type); err != nil {
		return "", err
	}
	if writer == nil || tx == nil || !json.Valid(payload) {
		return "", fmt.Errorf("business event writer, transaction, and valid payload are required")
	}
	event := EventEnvelope{ID: NewEventID(), Type: definition.Type, Version: CurrentVersion, Source: string(execution.OwnerModuleID), Site: site, AggregateType: aggregateType, AggregateID: aggregateID, OccurredAt: time.Now().UTC(), CorrelationID: correlationID, CausationID: causationID, Data: payload}
	if err := writer.Append(ctx, tx, event); err != nil {
		return "", err
	}
	return event.ID, nil
}

func NewModuleExecution(tenant TenantID, caller, owner ModuleID, revision uint64, operation, scope string) (ModuleExecution, error) {
	if tenant == "" || caller == "" || owner == "" || operation == "" || scope == "" {
		return ModuleExecution{}, fmt.Errorf("module execution identity is incomplete")
	}
	return ModuleExecution{TenantID: tenant, CallerModuleID: caller, OwnerModuleID: owner, BindingRevision: revision, AllowedOperation: operation, ScopeKey: scope, verified: true}, nil
}

func NewCommandModuleExecution(tenant TenantID, caller, owner ModuleID, revision uint64, command, operation, scope string) (ModuleExecution, error) {
	e, err := NewModuleExecution(tenant, caller, owner, revision, operation, scope)
	if err != nil {
		return ModuleExecution{}, err
	}
	if strings.TrimSpace(command) == "" {
		return ModuleExecution{}, fmt.Errorf("authorized domain command is required")
	}
	e.AuthorizedCommand = command
	return e, nil
}

func (e ModuleExecution) Verified() bool {
	return e.verified && e.TenantID != "" && e.CallerModuleID != "" && e.OwnerModuleID != "" && e.AllowedOperation != "" && e.ScopeKey != ""
}

var moduleIDPattern = regexp.MustCompile(`^[a-z0-9]+(\.[a-z0-9-]+)+$`)
var moduleSemverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

func (m ModuleDescriptor) Validate(registeredSchema func(SchemaRef) bool) error {
	if !moduleIDPattern.MatchString(string(m.ID)) {
		return fmt.Errorf("invalid module id %q", m.ID)
	}
	if !moduleSemverPattern.MatchString(m.Version) {
		return fmt.Errorf("module %q requires a semver version", m.ID)
	}
	resources := map[string]bool{}
	for _, resource := range m.Resources {
		key := fmt.Sprintf("%s/%s", resource.Ref.Namespace, resource.Ref.Name)
		if resource.Ref.Namespace == "" || resource.Ref.Name == "" || resource.Ref.Version < 1 {
			return fmt.Errorf("module %q has invalid resource reference", m.ID)
		}
		if resources[key] {
			return fmt.Errorf("module %q duplicates resource %q", m.ID, key)
		}
		resources[key] = true
		if resource.Access != ModulePrivate && resource.Access != ModulePublished {
			return fmt.Errorf("module %q resource %q has invalid access", m.ID, key)
		}
		if resource.MutationMode != MutationCRUD && resource.MutationMode != MutationCommands {
			return fmt.Errorf("module %q resource %q has invalid mutation mode", m.ID, key)
		}
	}
	capabilities := map[string]bool{}
	for _, offer := range m.Provides {
		if offer.Name == "" || capabilities[string(offer.Name)] {
			return fmt.Errorf("module %q has empty or duplicate capability offer %q", m.ID, offer.Name)
		}
		capabilities[string(offer.Name)] = true
		if err := validateModuleSchema(offer.Contract, registeredSchema); err != nil {
			return err
		}
		ops := map[string]bool{}
		for _, op := range offer.Operations {
			if op.Name == "" || ops[op.Name] {
				return fmt.Errorf("module %q capability %q has empty or duplicate operation", m.ID, offer.Name)
			}
			ops[op.Name] = true
			if op.Effect != EffectQuery && op.Effect != EffectLocal && op.Effect != EffectExternal {
				return fmt.Errorf("module %q operation %q has unknown effect %q", m.ID, op.Name, op.Effect)
			}
			if err := validateModuleSchema(op.Input, registeredSchema); err != nil {
				return err
			}
			if err := validateModuleSchema(op.Output, registeredSchema); err != nil {
				return err
			}
		}
	}
	required := map[string]bool{}
	for _, req := range m.Requires {
		if req.Name == "" || required[string(req.Name)] {
			return fmt.Errorf("module %q has empty or duplicate capability requirement", m.ID)
		}
		required[string(req.Name)] = true
		if err := validateModuleSchema(req.Contract, registeredSchema); err != nil {
			return err
		}
	}
	return nil
}

func validateModuleSchema(ref SchemaRef, registered func(SchemaRef) bool) error {
	if strings.TrimSpace(ref.ID) == "" || ref.Version == 0 {
		return fmt.Errorf("schema id and version are required")
	}
	if registered != nil && !registered(ref) {
		return fmt.Errorf("schema %s@%d is not registered", ref.ID, ref.Version)
	}
	return nil
}

// ValidateModuleSet rejects ambiguous ownership and returns deterministic errors.
func ValidateModuleSet(modules []ModuleDescriptor, registered func(SchemaRef) bool) error {
	ids, owners := map[ModuleID]bool{}, map[string]ModuleID{}
	ordered := append([]ModuleDescriptor(nil), modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, module := range ordered {
		if ids[module.ID] {
			return fmt.Errorf("duplicate module id %q", module.ID)
		}
		ids[module.ID] = true
		if err := module.Validate(registered); err != nil {
			return err
		}
		for _, resource := range module.Resources {
			key := fmt.Sprintf("%s/%s", resource.Ref.Namespace, resource.Ref.Name)
			if owner, ok := owners[key]; ok {
				return fmt.Errorf("resource %q has duplicate owners %q and %q", key, owner, module.ID)
			}
			owners[key] = module.ID
		}
	}
	return nil
}
