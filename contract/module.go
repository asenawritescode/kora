package contract

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	ID              ModuleID                `json:"id"`
	Version         string                  `json:"version"`
	Resources       []OwnedResource         `json:"resources,omitempty"`
	Provides        []CapabilityOffer       `json:"provides,omitempty"`
	Requires        []CapabilityRequirement `json:"requires,omitempty"`
	ExtensionPoints []ExtensionPoint        `json:"extension_points,omitempty"`
	Extensions      []ExtensionRegistration `json:"extensions,omitempty"`
}

// ExtensionPoint is an owner declaration of a narrowly scoped extension surface.
//
//contract:wire:v2
type ExtensionPoint struct {
	OwnerModuleID  ModuleID    `json:"owner_module_id"`
	ID             string      `json:"id"`
	Version        uint32      `json:"version"`
	Target         ResourceRef `json:"target"`
	AllowedKinds   []string    `json:"allowed_kinds"`
	InputSchema    SchemaRef   `json:"input_schema"`
	OutputSchema   SchemaRef   `json:"output_schema"`
	ExecutionPhase string      `json:"execution_phase"`
	FailurePolicy  string      `json:"failure_policy"`
}

// ExtensionRegistration grants no resource mutation authority.
//
//contract:wire:v2
type ExtensionRegistration struct {
	ExtenderModuleID ModuleID         `json:"extender_module_id"`
	PointID          string           `json:"point_id"`
	PointVersion     uint32           `json:"point_version"`
	ArtifactDigest   string           `json:"artifact_digest"`
	HandlerRef       ResourceRef      `json:"handler_ref"`
	Order            int32            `json:"order"`
	AddedFields      []ExtensionField `json:"added_fields,omitempty"`
}

// ExtensionField must use an extender-owned namespaced key.
//
//contract:wire:v2
type ExtensionField struct {
	Key    string    `json:"key"`
	Schema SchemaRef `json:"schema"`
}

// PackageDependency preserves immutable package requirements and conflicts.
//
//contract:wire:v2
type PackageDependency struct {
	PackageID string `json:"package_id"`
	ReleaseID string `json:"release_id,omitempty"`
	Version   string `json:"version,omitempty"`
	Required  bool   `json:"required"`
}

// CompositionPlan is deterministic preflight output before configuration mutation.
//
//contract:wire:v2
type CompositionPlan struct {
	Modules          []ModuleDescriptor      `json:"modules"`
	Bindings         []BindingInspection     `json:"bindings"`
	ActivationOrder  []ModuleID              `json:"activation_order"`
	Conflicts        []string                `json:"conflicts,omitempty"`
	CyclePaths       [][]ModuleID            `json:"cycle_paths,omitempty"`
	DisabledOptional []CapabilityRequirement `json:"disabled_optional,omitempty"`
	PlanHash         string                  `json:"plan_hash"`
}

type BindingKey struct {
	TenantID         TenantID
	ScopeKey         string
	ConsumerModuleID ModuleID
	CapabilityName   CapabilityName
}
type ProviderRegistration struct {
	TenantID      TenantID
	ProviderID    string
	OwnerModuleID ModuleID
	Kind          string
	Offers        []CapabilityOffer
}
type ProviderSelection struct {
	Key        BindingKey
	ProviderID string
}

func PlanComposition(modules []ModuleDescriptor, providers []ProviderRegistration, selections []ProviderSelection, tenant TenantID, scope string, revision uint64) (CompositionPlan, error) {
	ordered := append([]ModuleDescriptor(nil), modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	if err := ValidateModuleSet(ordered, nil); err != nil {
		return CompositionPlan{}, err
	}
	if err := ValidateModuleExtensions(ordered, nil); err != nil {
		return CompositionPlan{}, err
	}
	moduleByID := map[ModuleID]ModuleDescriptor{}
	for _, module := range ordered {
		moduleByID[module.ID] = module
	}
	selected := map[BindingKey]string{}
	for _, selection := range selections {
		if selection.Key.TenantID != tenant || selection.Key.ScopeKey != scope {
			return CompositionPlan{}, fmt.Errorf("provider selection crosses scope")
		}
		selected[selection.Key] = selection.ProviderID
	}
	plan := CompositionPlan{Modules: ordered}
	edges := map[ModuleID]map[ModuleID]bool{}
	disabled := map[string]CapabilityRequirement{}
	for _, consumer := range ordered {
		for _, requirement := range consumer.Requires {
			key := BindingKey{TenantID: tenant, ScopeKey: scope, ConsumerModuleID: consumer.ID, CapabilityName: requirement.Name}
			providerID := selected[key]
			var candidates []ProviderRegistration
			for _, provider := range providers {
				if provider.TenantID != tenant {
					continue
				}
				if providerID != "" && provider.ProviderID != providerID {
					continue
				}
				for _, offer := range provider.Offers {
					if offer.Name == requirement.Name && offer.Contract == requirement.Contract {
						candidates = append(candidates, provider)
						break
					}
				}
			}
			sort.Slice(candidates, func(i, j int) bool { return candidates[i].ProviderID < candidates[j].ProviderID })
			if len(candidates) == 0 {
				if requirement.Required {
					return CompositionPlan{}, fmt.Errorf("module %q requires missing capability %q", consumer.ID, requirement.Name)
				}
				disabled[string(consumer.ID)+"/"+string(requirement.Name)] = requirement
				continue
			}
			if providerID == "" && len(candidates) > 1 {
				return CompositionPlan{}, fmt.Errorf("capability %q requires explicit provider selection", requirement.Name)
			}
			provider := candidates[0]
			if provider.OwnerModuleID == consumer.ID {
				return CompositionPlan{}, fmt.Errorf("module %q has self dependency through %q", consumer.ID, requirement.Name)
			}
			if edges[consumer.ID] == nil {
				edges[consumer.ID] = map[ModuleID]bool{}
			}
			if moduleByID[provider.OwnerModuleID].ID != "" {
				edges[consumer.ID][provider.OwnerModuleID] = true
			}
			plan.Bindings = append(plan.Bindings, BindingInspection{ConsumerModuleID: consumer.ID, CapabilityName: requirement.Name, ProviderID: provider.ProviderID, BindingRevision: revision})
		}
	}
	order, cycle := topologicalModuleOrder(ordered, edges)
	if len(cycle) > 0 {
		return CompositionPlan{}, fmt.Errorf("module initialization cycle: %s", moduleCycleText(cycle))
	}
	plan.ActivationOrder = order
	for _, req := range disabled {
		plan.DisabledOptional = append(plan.DisabledOptional, req)
	}
	sort.Slice(plan.DisabledOptional, func(i, j int) bool { return plan.DisabledOptional[i].Name < plan.DisabledOptional[j].Name })
	sort.Slice(plan.Bindings, func(i, j int) bool {
		a, b := plan.Bindings[i], plan.Bindings[j]
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	canonical, err := json.Marshal(struct {
		Modules  []ModuleDescriptor
		Bindings []BindingInspection
		Order    []ModuleID
		Revision uint64
	}{ordered, plan.Bindings, order, revision})
	if err != nil {
		return CompositionPlan{}, err
	}
	hash := sha256.Sum256(canonical)
	plan.PlanHash = hex.EncodeToString(hash[:])
	return plan, nil
}

func topologicalModuleOrder(modules []ModuleDescriptor, dependencies map[ModuleID]map[ModuleID]bool) ([]ModuleID, []ModuleID) {
	indegree := map[ModuleID]int{}
	dependents := map[ModuleID][]ModuleID{}
	for _, m := range modules {
		indegree[m.ID] = 0
	}
	for consumer, deps := range dependencies {
		for dep := range deps {
			if _, ok := indegree[dep]; !ok {
				continue
			}
			indegree[consumer]++
			dependents[dep] = append(dependents[dep], consumer)
		}
	}
	var queue []ModuleID
	for id, n := range indegree {
		if n == 0 {
			queue = append(queue, id)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
	var order []ModuleID
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, consumer := range dependents[id] {
			indegree[consumer]--
			if indegree[consumer] == 0 {
				queue = append(queue, consumer)
				sort.Slice(queue, func(i, j int) bool { return queue[i] < queue[j] })
			}
		}
	}
	if len(order) == len(indegree) {
		return order, nil
	}
	var cycle []ModuleID
	for id, n := range indegree {
		if n > 0 {
			cycle = append(cycle, id)
		}
	}
	sort.Slice(cycle, func(i, j int) bool { return cycle[i] < cycle[j] })
	return nil, cycle
}

func moduleCycleText(cycle []ModuleID) string {
	parts := make([]string, len(cycle))
	for i, id := range cycle {
		parts[i] = string(id)
	}
	return strings.Join(parts, " -> ")
}

func ValidateModuleExtensions(modules []ModuleDescriptor, registered func(SchemaRef) bool) error {
	points := map[string]ExtensionPoint{}
	ownerModules := map[ModuleID]ModuleDescriptor{}
	for _, module := range modules {
		ownerModules[module.ID] = module
		for _, point := range module.ExtensionPoints {
			if point.OwnerModuleID != module.ID || point.ID == "" || point.Version == 0 || point.Target.Name == "" || len(point.AllowedKinds) == 0 {
				return fmt.Errorf("invalid extension point in module %q", module.ID)
			}
			key := string(module.ID) + "/" + point.ID
			if _, ok := points[key]; ok {
				return fmt.Errorf("duplicate extension point %q", key)
			}
			points[key] = point
		}
	}
	orders := map[string]map[int32]bool{}
	for _, module := range modules {
		for _, extension := range module.Extensions {
			key := extension.PointID
			point, ok := points[key]
			if !ok {
				return fmt.Errorf("extension point %q is undeclared", key)
			}
			if extension.ExtenderModuleID != module.ID || extension.PointVersion != point.Version || extension.ArtifactDigest == "" {
				return fmt.Errorf("extension %q has invalid owner or point version", key)
			}
			if orders[key] == nil {
				orders[key] = map[int32]bool{}
			}
			if orders[key][extension.Order] {
				return fmt.Errorf("extension point %q has ambiguous order %d", key, extension.Order)
			}
			orders[key][extension.Order] = true
			for _, field := range extension.AddedFields {
				if !strings.HasPrefix(field.Key, string(module.ID)+".") || field.Schema.ID == "" || field.Schema.Version == 0 {
					return fmt.Errorf("extension field %q must be extender namespaced and versioned", field.Key)
				}
				if registered != nil && !registered(field.Schema) {
					return fmt.Errorf("extension field %q schema is unregistered", field.Key)
				}
			}
			if _, ok := ownerModules[point.OwnerModuleID]; !ok {
				return fmt.Errorf("extension point owner is not installed")
			}
		}
	}
	return nil
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

type ModuleLifecycleState string

const (
	ModuleInstalled ModuleLifecycleState = "installed"
	ModuleActive    ModuleLifecycleState = "active"
	ModuleDraining  ModuleLifecycleState = "draining"
	ModuleDisabled  ModuleLifecycleState = "disabled"
	ModuleRemoved   ModuleLifecycleState = "removed"
)

// PlanModuleChange is the optimistic input to lifecycle planning.
//
//contract:wire:v2
type PlanModuleChange struct {
	ModuleID         ModuleID             `json:"module_id"`
	TargetState      ModuleLifecycleState `json:"target_state"`
	TargetVersion    string               `json:"target_version,omitempty"`
	ExpectedRevision uint64               `json:"expected_revision"`
}

// ModuleChangePlan carries bounded reasons why an operation cannot proceed.
//
//contract:wire:v2
type ModuleChangePlan struct {
	PlanHash           string          `json:"plan_hash"`
	BaseRevision       uint64          `json:"base_revision"`
	DependencyBlockers []ModuleBlocker `json:"dependency_blockers,omitempty"`
	OpenWorkBlockers   []ModuleBlocker `json:"open_work_blockers,omitempty"`
	BindingChanges     []string        `json:"binding_changes,omitempty"`
}

// ModuleBlocker is deliberately small and excludes business payloads.
//
//contract:wire:v2
type ModuleBlocker struct {
	Code     string   `json:"code"`
	Count    uint32   `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

// ModuleInspection is a redacted, tenant scoped runtime status projection.
//
//contract:wire:v2
type ModuleInspection struct {
	ConfigRevision         uint64                 `json:"config_revision"`
	Installations          []ModuleInstallation   `json:"installations"`
	ResourceOwners         []ResourceOwnership    `json:"resource_owners"`
	Bindings               []BindingInspection    `json:"bindings"`
	CapabilityAvailability []CapabilityInspection `json:"capability_availability"`
	BlockingReasons        []ModuleBlocker        `json:"blocking_reasons"`
	PendingWork            PendingWorkSummary     `json:"pending_work"`
}

// BindingInspection contains no credentials or provider configuration.
//
//contract:wire:v2
type BindingInspection struct {
	ConsumerModuleID ModuleID       `json:"consumer_module_id"`
	CapabilityName   CapabilityName `json:"capability_name"`
	ProviderID       string         `json:"provider_id"`
	BindingRevision  uint64         `json:"binding_revision"`
}

// CapabilityInspection is a bounded availability diagnostic.
//
//contract:wire:v2
type CapabilityInspection struct {
	Name       CapabilityName `json:"name"`
	State      string         `json:"state"`
	ReasonCode string         `json:"reason_code,omitempty"`
}

// PendingWorkSummary provides counts only.
//
//contract:wire:v2
type PendingWorkSummary struct {
	WorkflowCount       uint32 `json:"workflow_count"`
	DeliveryCount       uint32 `json:"delivery_count"`
	UnknownOutcomeCount uint32 `json:"unknown_outcome_count"`
	ScheduledCount      uint32 `json:"scheduled_count"`
	OpenObligationCount uint32 `json:"open_obligation_count"`
}

// ExecutionInspection describes a pinned execution without exposing its payload.
//
//contract:wire:v2
type ExecutionInspection struct {
	ExecutionID         string           `json:"execution_id"`
	CallerModuleID      ModuleID         `json:"caller_module_id"`
	OwnerModuleID       ModuleID         `json:"owner_module_id"`
	ProviderID          string           `json:"provider_id"`
	BindingRevision     uint64           `json:"binding_revision"`
	Contract            CapabilityAction `json:"contract"`
	CorrelationID       string           `json:"correlation_id"`
	DeliveryState       string           `json:"delivery_state"`
	ReconciliationState string           `json:"reconciliation_state"`
}

// ApplyModuleChange pins the reviewed plan to the expected snapshot revision.
//
//contract:wire:v2
type ApplyModuleChange struct {
	Meta             CommandMeta `json:"meta"`
	ModuleID         ModuleID    `json:"module_id"`
	PlanHash         string      `json:"plan_hash"`
	ExpectedRevision uint64      `json:"expected_revision"`
}

type OpenWorkInspector interface {
	InspectOpenWork(context.Context, TenantID, ModuleID, uint16) ([]ModuleBlocker, error)
}

func ValidateModuleLifecycleTransition(from, to ModuleLifecycleState) error {
	switch from {
	case ModuleInstalled:
		if to == ModuleActive || to == ModuleDisabled {
			return nil
		}
	case ModuleActive:
		if to == ModuleDraining {
			return nil
		}
	case ModuleDraining:
		if to == ModuleDisabled {
			return nil
		}
	case ModuleDisabled:
		if to == ModuleActive || to == ModuleRemoved {
			return nil
		}
	}
	return fmt.Errorf("invalid module lifecycle transition %q -> %q", from, to)
}

func ValidateModuleChangePlan(plan ModuleChangePlan, expectedRevision uint64, openWork []ModuleBlocker) error {
	if plan.PlanHash == "" || plan.BaseRevision != expectedRevision {
		return fmt.Errorf("module change plan is stale or incomplete")
	}
	if len(plan.DependencyBlockers) > 0 || len(plan.OpenWorkBlockers) > 0 || len(openWork) > 0 {
		return fmt.Errorf("module change is blocked by dependencies or open work")
	}
	return nil
}

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
