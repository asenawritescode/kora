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
	"unicode"
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
	Ref          ResourceRef       `json:"ref"`
	Access       ModuleAccess      `json:"access"`
	MutationMode MutationMode      `json:"mutation_mode"`
	ScopeKind    ResourceScopeKind `json:"scope_kind"`
	ScopeField   string            `json:"scope_field,omitempty"`
}

// BusinessScope identifies a tenant or one verified legal entity.
//
//contract:wire:v2
type BusinessScope struct {
	TenantID      TenantID          `json:"tenant_id"`
	Kind          BusinessScopeKind `json:"kind"`
	LegalEntityID string            `json:"legal_entity_id,omitempty"`
}

// CrossEntityOperation is an explicit command envelope for an operation that
// must act across two legal entities. Both scoped grants are checked before a
// command handler is allowed to execute it.
//
//contract:wire:v2
type CrossEntityOperation struct {
	OperationID    string   `json:"operation_id"`
	SourceEntityID string   `json:"source_entity_id"`
	TargetEntityID string   `json:"target_entity_id"`
	Resource       string   `json:"resource"`
	Operation      string   `json:"operation"`
	TenantID       TenantID `json:"tenant_id"`
}

func (o CrossEntityOperation) Validate() error {
	if !validBusinessEntityID(o.SourceEntityID) || !validBusinessEntityID(o.TargetEntityID) || o.SourceEntityID == o.TargetEntityID || !moduleIDPattern.MatchString(o.OperationID) {
		return fmt.Errorf("cross-entity operation requires distinct entities and a valid operation ID")
	}
	if _, err := ParseTenantID(string(o.TenantID)); err != nil || !validBusinessEntityID(o.Resource) || !validBusinessEntityID(o.Operation) {
		return fmt.Errorf("cross-entity operation identity is invalid")
	}
	return nil
}

//contract:enum:v2
type BusinessScopeKind string

const (
	ScopeKindTenant      BusinessScopeKind = "tenant"
	ScopeKindLegalEntity BusinessScopeKind = "legal_entity"
)

//contract:enum:v2
type ResourceScopeKind string

const (
	ScopeTenantShared ResourceScopeKind = "tenant_shared"
	ScopeEntityOwned  ResourceScopeKind = "entity_owned"
)

func (s BusinessScope) Validate() error {
	if _, err := ParseTenantID(string(s.TenantID)); err != nil {
		return fmt.Errorf("business scope tenant is required")
	}
	switch s.Kind {
	case ScopeKindTenant:
		if s.LegalEntityID != "" {
			return fmt.Errorf("tenant scope cannot carry legal entity")
		}
	case ScopeKindLegalEntity:
		if !validBusinessEntityID(s.LegalEntityID) {
			return fmt.Errorf("legal entity scope requires a valid entity ID")
		}
	default:
		return fmt.Errorf("unknown business scope kind %q", s.Kind)
	}
	return nil
}

func validBusinessEntityID(id string) bool {
	if id == "" || strings.TrimSpace(id) != id || len(id) > 160 {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func ValidateResourceBusinessScope(scope BusinessScope, resourcePolicy ResourceScopeKind) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	switch resourcePolicy {
	case ScopeTenantShared:
		return nil
	case ScopeEntityOwned:
		if scope.Kind != ScopeKindLegalEntity {
			return fmt.Errorf("entity-owned resource requires legal entity scope")
		}
		return nil
	default:
		return fmt.Errorf("resource scope policy is required")
	}
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

// PublishedQuery is an owner-controlled, read-only capability surface. The
// operation must be declared by the owner module with EffectQuery; consumers
// never receive direct access to the provider's tables.
//
//contract:wire:v2
type PublishedQuery struct {
	OwnerModuleID ModuleID          `json:"owner_module_id"`
	Name          string            `json:"name"`
	Capability    CapabilityName    `json:"capability"`
	Operation     string            `json:"operation"`
	InputSchema   SchemaRef         `json:"input_schema"`
	OutputSchema  SchemaRef         `json:"output_schema"`
	ScopePolicy   ResourceScopeKind `json:"scope_policy"`
	AllowedFields []string          `json:"allowed_fields"`
	PageLimit     uint16            `json:"page_limit"`
}

// QueryFreshness describes whether a published query result is current.
//
//contract:enum:v2
type QueryFreshness string

const (
	QueryFresh      QueryFreshness = "fresh"
	QueryStale      QueryFreshness = "stale"
	QueryUnknown    QueryFreshness = "unknown"
	QueryIncomplete QueryFreshness = "incomplete"
)

// QueryResult is a bounded, schema-validated result from a published query.
//
//contract:wire:v2
type QueryResult struct {
	//contract:json
	Data       json.RawMessage `json:"data"`
	NextCursor string          `json:"next_cursor,omitempty"`
	// SnapshotToken pins every page in a paginated query to one immutable source snapshot.
	SnapshotToken  string         `json:"snapshot_token,omitempty"`
	SourceRevision string         `json:"source_revision,omitempty"`
	AsOf           time.Time      `json:"as_of"`
	Freshness      QueryFreshness `json:"freshness"`
}

// ProjectionSource pins one projection input to a published query contract.
//
//contract:wire:v2
type ProjectionSource struct {
	OwnerModuleID ModuleID  `json:"owner_module_id"`
	QueryName     string    `json:"query_name"`
	OutputSchema  SchemaRef `json:"output_schema"`
}

// ProjectionDefinition declares a rebuildable, owner-controlled projection.
//
//contract:wire:v2
type ProjectionDefinition struct {
	ID            string             `json:"id"`
	OwnerModuleID ModuleID           `json:"owner_module_id"`
	Sources       []ProjectionSource `json:"sources"`
	ScopePolicy   ResourceScopeKind  `json:"scope_policy"`
	FactKey       string             `json:"fact_key"`
	AllowedFields []string           `json:"allowed_fields"`
	OutputSchema  SchemaRef          `json:"output_schema"`
	// consistent_snapshot requires every source to return the same non-empty
	// global snapshot token and as-of instant. Each paginated cursor must stay
	// pinned to that immutable snapshot; providers unable to prove that complete
	// cross-source boundary are unavailable for rebuild.
	RebuildStrategy string `json:"rebuild_strategy"`
}

func (p ProjectionDefinition) Validate() error {
	if p.ID == "" || p.OwnerModuleID == "" || !moduleIDPattern.MatchString(string(p.OwnerModuleID)) || len(p.Sources) == 0 || len(p.Sources) > 32 || p.RebuildStrategy != "consistent_snapshot" || p.ScopePolicy != ScopeTenantShared && p.ScopePolicy != ScopeEntityOwned || !validModuleName(p.FactKey) || len(p.AllowedFields) == 0 || len(p.AllowedFields) > 128 || p.OutputSchema.ID == "" || p.OutputSchema.Version == 0 {
		return fmt.Errorf("projection definition requires an owner, bounded sources, output schema, and supported rebuild strategy")
	}
	allowed := make(map[string]bool, len(p.AllowedFields))
	factKeyAllowed := false
	for _, field := range p.AllowedFields {
		if !validModuleName(field) || allowed[field] {
			return fmt.Errorf("projection %q has an invalid or duplicate allowed field", p.ID)
		}
		allowed[field] = true
		factKeyAllowed = factKeyAllowed || field == p.FactKey
	}
	if !factKeyAllowed {
		return fmt.Errorf("projection %q fact key is not published", p.ID)
	}
	seen := make(map[ProjectionSource]bool, len(p.Sources))
	for _, source := range p.Sources {
		if source.OwnerModuleID == "" || !moduleIDPattern.MatchString(string(source.OwnerModuleID)) || !validModuleName(source.QueryName) || source.OutputSchema.ID == "" || source.OutputSchema.Version == 0 || source.OutputSchema != p.OutputSchema || seen[source] {
			return fmt.Errorf("projection %q has an invalid or duplicate source", p.ID)
		}
		seen[source] = true
	}
	return nil
}

// ProjectionCheckpoint resumes projection replay from one source and scope.
//
//contract:wire:v2
type ProjectionCheckpoint struct {
	ProjectionID string        `json:"projection_id"`
	TenantID     TenantID      `json:"tenant_id"`
	EntityScope  BusinessScope `json:"entity_scope"`
	SourceID     string        `json:"source_id"`
	Cursor       string        `json:"cursor"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

func (q QueryResult) Validate(query PublishedQuery, limit uint16, scope BusinessScope) error {
	if scope.Validate() != nil || ValidateResourceBusinessScope(scope, query.ScopePolicy) != nil {
		return fmt.Errorf("published query result scope is invalid")
	}
	if limit == 0 || limit > query.PageLimit || len(q.Data) == 0 || !json.Valid(q.Data) || q.AsOf.IsZero() {
		return fmt.Errorf("published query result is incomplete or exceeds its page contract")
	}
	switch q.Freshness {
	case QueryFresh, QueryStale, QueryUnknown, QueryIncomplete:
	default:
		return fmt.Errorf("published query result has invalid freshness")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(q.Data, &rows); err != nil || len(rows) > int(limit) {
		return fmt.Errorf("published query result data must be a bounded row array")
	}
	allowed := make(map[string]bool, len(query.AllowedFields))
	for _, field := range query.AllowedFields {
		allowed[field] = true
	}
	for _, row := range rows {
		for field, value := range row {
			if !allowed[field] || !json.Valid(value) {
				return fmt.Errorf("published query result contains an undeclared field")
			}
		}
	}
	if len(q.NextCursor) > 2048 || len(q.SnapshotToken) > 2048 || len(q.SourceRevision) > 256 {
		return fmt.Errorf("published query result cursor or source revision is too large")
	}
	return nil
}

func (p ProjectionCheckpoint) Validate() error {
	if p.ProjectionID == "" || p.SourceID == "" || p.Cursor == "" || p.UpdatedAt.IsZero() || p.EntityScope.Validate() != nil || p.TenantID != p.EntityScope.TenantID {
		return fmt.Errorf("projection checkpoint identity is invalid")
	}
	return nil
}

// ModuleInstallation is the persisted lifecycle identity for one tenant module.
//
//contract:wire:v2
type ModuleInstallation struct {
	TenantID                 TenantID    `json:"tenant_id"`
	ModuleID                 ModuleID    `json:"module_id"`
	Version                  string      `json:"version"`
	ArtifactDigest           string      `json:"artifact_digest"`
	State                    string      `json:"state"`
	Revision                 uint64      `json:"revision"`
	StateChangedAt           time.Time   `json:"state_changed_at,omitempty"`
	LastLifecycleOperationID OperationID `json:"last_lifecycle_operation_id,omitempty"`
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
	TenantID           TenantID             `json:"tenant_id"`
	Revision           uint64               `json:"revision"`
	Modules            []ModuleDescriptor   `json:"modules,omitempty"`
	Packages           []InstalledPackage   `json:"packages,omitempty"`
	Installations      []ModuleInstallation `json:"installations"`
	Ownership          []ResourceOwnership  `json:"ownership"`
	ProviderSelections []ProviderSelection  `json:"provider_selections,omitempty"`
}

// CapabilityLease is an internal durable, scope-bound grant for one provider
// operation. It is persisted by the tenant module store, not exposed on a wire.
type CapabilityLease struct {
	ID               string        `json:"id"`
	TenantID         TenantID      `json:"tenant_id"`
	Scope            BusinessScope `json:"scope"`
	CallerModuleID   ModuleID      `json:"caller_module_id"`
	ProviderModuleID ModuleID      `json:"provider_module_id,omitempty"`
	ProviderID       string        `json:"provider_id"`
	Operation        string        `json:"operation"`
	BindingRevision  uint64        `json:"binding_revision"`
	GrantEpoch       uint64        `json:"grant_epoch"`
	ExpiresAt        time.Time     `json:"expires_at"`
	State            string        `json:"state"`
	AdmittedAttempts uint64        `json:"admitted_attempts"`
	CredentialRef    string        `json:"credential_ref,omitempty"`
}

func (l CapabilityLease) Validate() error {
	if l.ID == "" || l.TenantID == "" || l.Scope.Validate() != nil || l.Scope.TenantID != l.TenantID || l.CallerModuleID == "" || l.ProviderModuleID == "" || l.ProviderID == "" || l.Operation == "" || l.BindingRevision == 0 || l.GrantEpoch == 0 || l.ExpiresAt.IsZero() || l.State != "active" && l.State != "revoked" && l.State != "expired" {
		return fmt.Errorf("capability lease identity, scope, epoch, expiry, or state is invalid")
	}
	if len(l.CredentialRef) > 320 || strings.ContainsAny(l.CredentialRef, "\r\n\x00") {
		return fmt.Errorf("capability credential reference is invalid")
	}
	return nil
}

// CapabilitySelector identifies one caller/provider operation within a
// business scope. It intentionally has no wildcard fields.
type CapabilitySelector struct {
	CallerModuleID ModuleID
	ProviderID     string
	Operation      string
}

func (s CapabilitySelector) Validate() error {
	if s.CallerModuleID == "" || s.ProviderID == "" || s.Operation == "" {
		return fmt.Errorf("capability selector requires caller, provider, and operation")
	}
	return nil
}

// RevokeCapability is a revision-fenced security decision for one exact
// capability selector in one tenant or legal-entity scope.
type RevokeCapability struct {
	Scope            BusinessScope
	Selector         CapabilitySelector
	ExpectedRevision uint64
	Reason           string
}

func (r RevokeCapability) Validate() error {
	if r.Scope.Validate() != nil || r.Selector.Validate() != nil || r.ExpectedRevision == 0 || strings.TrimSpace(r.Reason) == "" || len(r.Reason) > 1000 {
		return fmt.Errorf("capability revocation scope, selector, revision, or reason is invalid")
	}
	return nil
}

// InstalledPackage preserves immutable package identity and composition
// constraints already checked before package resources were activated.
// It lives in the existing module snapshot rather than a second registry.
//
//contract:wire:v2
type InstalledPackage struct {
	PackageID string            `json:"package_id"`
	ReleaseID string            `json:"release_id"`
	Version   string            `json:"version"`
	Digest    string            `json:"digest"`
	Requires  []ManifestPackRef `json:"requires,omitempty"`
	Conflicts []ManifestPackRef `json:"conflicts,omitempty"`
}

// ModuleStore is the persistence boundary used by activation. CommitActivation
// must atomically compare and increment the snapshot revision with the caller's
// activation transaction.
type ModuleStore interface {
	Load(tenant TenantID) (ModuleSnapshot, error)
	CommitProviderSelections(ctx context.Context, tenant TenantID, expectedRevision uint64, selections []ProviderSelection) (ModuleSnapshot, error)
}

// ModuleDescriptor is the canonical package level business module contract.
//
//contract:wire:v2
type ModuleDescriptor struct {
	ID              ModuleID                `json:"id"`
	Version         string                  `json:"version"`
	Resources       []OwnedResource         `json:"resources,omitempty"`
	Queries         []PublishedQuery        `json:"queries,omitempty"`
	Projections     []ProjectionDefinition  `json:"projections,omitempty"`
	Provides        []CapabilityOffer       `json:"provides,omitempty"`
	Requires        []CapabilityRequirement `json:"requires,omitempty"`
	ExtensionPoints []ExtensionPoint        `json:"extension_points,omitempty"`
	Extensions      []ExtensionRegistration `json:"extensions,omitempty"`
}

// ExtensionKind is a behavior explicitly allowed by an owner-declared point.
//
//contract:enum:v2
type ExtensionKind string

const (
	ExtensionFields     ExtensionKind = "fields"
	ExtensionValidation ExtensionKind = "validation"
	ExtensionPostCommit ExtensionKind = "post_commit"
)

// ExtensionPoint is an owner declaration of a narrowly scoped extension surface.
//
//contract:wire:v2
type ExtensionPoint struct {
	OwnerModuleID   ModuleID    `json:"owner_module_id"`
	ID              string      `json:"id"`
	Version         uint32      `json:"version"`
	Target          ResourceRef `json:"target"`
	AllowedKinds    []string    `json:"allowed_kinds"`
	ProtectedFields []string    `json:"protected_fields,omitempty"`
	InputSchema     SchemaRef   `json:"input_schema"`
	OutputSchema    SchemaRef   `json:"output_schema"`
	ExecutionPhase  string      `json:"execution_phase"`
	FailurePolicy   string      `json:"failure_policy"`
}

// ExtensionRegistration grants no resource mutation authority.
//
//contract:wire:v2
type ExtensionRegistration struct {
	ID               string           `json:"id"`
	ExtenderModuleID ModuleID         `json:"extender_module_id"`
	PointID          string           `json:"point_id"`
	PointVersion     uint32           `json:"point_version"`
	Kind             ExtensionKind    `json:"kind"`
	ArtifactDigest   string           `json:"artifact_digest"`
	HandlerRef       ResourceRef      `json:"handler_ref"`
	Event            string           `json:"event,omitempty"`
	Order            int32            `json:"order"`
	DependsOn        []string         `json:"depends_on,omitempty"`
	AddedFields      []ExtensionField `json:"added_fields,omitempty"`
}

// ResolvedModuleExtension is an activation-ordered extension registration.
//
//contract:wire:v2
type ResolvedModuleExtension struct {
	Point        ExtensionPoint        `json:"point"`
	Registration ExtensionRegistration `json:"registration"`
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
	Modules          []ModuleDescriptor        `json:"modules"`
	Packages         []InstalledPackage        `json:"packages,omitempty"`
	Extensions       []ResolvedModuleExtension `json:"extensions,omitempty"`
	Bindings         []BindingInspection       `json:"bindings"`
	ActivationOrder  []ModuleID                `json:"activation_order"`
	Conflicts        []string                  `json:"conflicts,omitempty"`
	CyclePaths       [][]ModuleID              `json:"cycle_paths,omitempty"`
	DisabledOptional []CapabilityRequirement   `json:"disabled_optional,omitempty"`
	PlanHash         string                    `json:"plan_hash"`
}

// PlanInstalledComposition combines package dependency validation with the
// existing capability graph planner and hashes the entire activation plan.
func PlanInstalledComposition(packages []InstalledPackage, modules []ModuleDescriptor, providers []ProviderRegistration, selections []ProviderSelection, scope BusinessScope, revision uint64) (CompositionPlan, error) {
	if err := ValidateInstalledPackages(packages); err != nil {
		return CompositionPlan{}, err
	}
	plan, err := PlanComposition(modules, providers, selections, scope, revision)
	if err != nil {
		return plan, err
	}
	plan.Packages = append([]InstalledPackage(nil), packages...)
	sort.Slice(plan.Packages, func(i, j int) bool { return plan.Packages[i].ReleaseID < plan.Packages[j].ReleaseID })
	canonical, err := json.Marshal(struct {
		Packages        []InstalledPackage
		Modules         []ModuleDescriptor
		Extensions      []ResolvedModuleExtension
		Bindings        []BindingInspection
		ActivationOrder []ModuleID
		Revision        uint64
	}{plan.Packages, plan.Modules, plan.Extensions, plan.Bindings, plan.ActivationOrder, revision})
	if err != nil {
		return CompositionPlan{}, err
	}
	hash := sha256.Sum256(canonical)
	plan.PlanHash = hex.EncodeToString(hash[:])
	return plan, nil
}

// ValidateInstalledPackages enforces immutable package requirements and
// conflicts across the complete candidate installation set.
func ValidateInstalledPackages(packages []InstalledPackage) error {
	if len(packages) > 512 {
		return fmt.Errorf("installed package set exceeds the supported limit")
	}
	byPackage := make(map[string][]InstalledPackage, len(packages))
	byRelease := make(map[string]InstalledPackage, len(packages))
	for _, pkg := range packages {
		if err := ValidateManifestPackRefs(pkg.Requires, pkg.Conflicts); err != nil {
			return fmt.Errorf("installed package %q has invalid constraints: %w", pkg.PackageID, err)
		}
		if pkg.PackageID == "" || pkg.ReleaseID == "" || pkg.Version == "" || pkg.Digest == "" {
			return fmt.Errorf("installed package identity is incomplete")
		}
		if _, exists := byRelease[pkg.ReleaseID]; exists {
			return fmt.Errorf("release %q is installed more than once", pkg.ReleaseID)
		}
		byRelease[pkg.ReleaseID] = pkg
		byPackage[pkg.PackageID] = append(byPackage[pkg.PackageID], pkg)
	}
	for _, pkg := range packages {
		for _, req := range pkg.Requires {
			matched := false
			for _, candidate := range byPackage[req.PackageID] {
				if req.ReleaseID != "" && candidate.ReleaseID != req.ReleaseID || req.Version != "" && candidate.Version != req.Version {
					continue
				}
				matched = true
				break
			}
			if !matched {
				return fmt.Errorf("installed package %q is missing required package %q", pkg.PackageID, req.PackageID)
			}
		}
		for _, conflict := range pkg.Conflicts {
			for _, candidate := range packages {
				if pkg.ReleaseID == candidate.ReleaseID {
					continue
				}
				packageMatch := conflict.PackageID != "" && conflict.PackageID == candidate.PackageID && (conflict.Version == "" || conflict.Version == candidate.Version)
				releaseMatch := conflict.ReleaseID != "" && conflict.ReleaseID == candidate.ReleaseID
				if packageMatch || releaseMatch {
					return fmt.Errorf("installed package %q conflicts with %q", pkg.PackageID, candidate.PackageID)
				}
			}
		}
	}
	return nil
}

//contract:wire:v2
type BindingKey struct {
	Scope            BusinessScope  `json:"scope"`
	ConsumerModuleID ModuleID       `json:"consumer_module_id"`
	CapabilityName   CapabilityName `json:"capability_name"`
}
type ProviderRegistration struct {
	TenantID      TenantID
	Scope         BusinessScope
	ProviderID    string
	OwnerModuleID ModuleID
	Kind          string
	Offers        []CapabilityOffer
}

//contract:wire:v2
type ProviderSelection struct {
	Key        BindingKey `json:"key"`
	ProviderID string     `json:"provider_id"`
}

func (selection ProviderSelection) Validate() error {
	if selection.Key.Scope.Validate() != nil || !moduleIDPattern.MatchString(string(selection.Key.ConsumerModuleID)) || !validModuleName(string(selection.Key.CapabilityName)) || !validBusinessEntityID(selection.ProviderID) {
		return fmt.Errorf("provider selection identity is invalid")
	}
	return nil
}

func PlanComposition(modules []ModuleDescriptor, providers []ProviderRegistration, selections []ProviderSelection, scope BusinessScope, revision uint64) (CompositionPlan, error) {
	if err := scope.Validate(); err != nil {
		return CompositionPlan{}, err
	}
	ordered := append([]ModuleDescriptor(nil), modules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	ordered = normalizeModuleExtensions(ordered)
	if err := ValidateModuleSet(ordered, nil); err != nil {
		return CompositionPlan{}, err
	}
	extensions, err := OrderModuleExtensions(ordered, nil)
	if err != nil {
		return CompositionPlan{}, err
	}
	moduleByID := map[ModuleID]ModuleDescriptor{}
	for _, module := range ordered {
		moduleByID[module.ID] = module
	}
	selected := map[BindingKey]string{}
	for _, selection := range selections {
		if err := selection.Validate(); err != nil {
			return CompositionPlan{}, err
		}
		if selection.Key.Scope.TenantID != scope.TenantID {
			return CompositionPlan{}, fmt.Errorf("provider selection crosses tenant")
		}
		if selection.Key.Scope != scope {
			continue
		}
		if _, exists := selected[selection.Key]; exists {
			return CompositionPlan{}, fmt.Errorf("provider selection for module %q capability %q is duplicated", selection.Key.ConsumerModuleID, selection.Key.CapabilityName)
		}
		selected[selection.Key] = selection.ProviderID
	}
	plan := CompositionPlan{Modules: ordered, Extensions: extensions}
	edges := map[ModuleID]map[ModuleID]bool{}
	disabled := map[string]CapabilityRequirement{}
	for _, consumer := range ordered {
		for _, requirement := range consumer.Requires {
			key := BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: requirement.Name}
			providerID := selected[key]
			var candidates []ProviderRegistration
			for _, provider := range providers {
				if provider.TenantID != scope.TenantID {
					continue
				}
				if provider.Scope.TenantID != "" && provider.Scope != scope {
					continue
				}
				if provider.Scope.TenantID == "" && (provider.Scope.Kind != "" || provider.Scope.LegalEntityID != "") {
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
		cycle = boundedModuleCycle(cycle, 16)
		plan.CyclePaths = [][]ModuleID{cycle}
		return plan, fmt.Errorf("module initialization cycle: %s", moduleCycleText(cycle))
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
		Modules    []ModuleDescriptor
		Extensions []ResolvedModuleExtension
		Bindings   []BindingInspection
		Order      []ModuleID
		Revision   uint64
	}{ordered, extensions, plan.Bindings, order, revision})
	if err != nil {
		return CompositionPlan{}, err
	}
	hash := sha256.Sum256(canonical)
	plan.PlanHash = hex.EncodeToString(hash[:])
	return plan, nil
}

func boundedModuleCycle(cycle []ModuleID, limit int) []ModuleID {
	if limit < 2 || len(cycle) <= limit {
		return append([]ModuleID(nil), cycle...)
	}
	bounded := append([]ModuleID(nil), cycle[:limit-1]...)
	bounded = append(bounded, ModuleID(fmt.Sprintf("…%d more", len(cycle)-(limit-1))))
	return bounded
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
	_, err := OrderModuleExtensions(modules, registered)
	return err
}

type plannedModuleExtension struct {
	point        ExtensionPoint
	registration ExtensionRegistration
	key          string
}

// OrderModuleExtensions validates declarations and returns a stable dependency
// order shared by activation and runtime consumers.
func OrderModuleExtensions(modules []ModuleDescriptor, registered func(SchemaRef) bool) ([]ResolvedModuleExtension, error) {
	if len(modules) > 256 {
		return nil, fmt.Errorf("module set exceeds the supported extension planning limit")
	}
	pointCount, registrationCount := 0, 0
	for _, module := range modules {
		if len(module.ExtensionPoints) > 128 || len(module.Extensions) > 256 {
			return nil, fmt.Errorf("module %q exceeds the supported extension declaration limits", module.ID)
		}
		pointCount += len(module.ExtensionPoints)
		registrationCount += len(module.Extensions)
	}
	if pointCount > 2048 || registrationCount > 4096 {
		return nil, fmt.Errorf("module set exceeds the supported extension declaration limits")
	}
	modules = normalizeModuleExtensions(modules)
	points := map[string]ExtensionPoint{}
	ownerModules := map[ModuleID]ModuleDescriptor{}
	for _, module := range modules {
		ownerModules[module.ID] = module
		for _, point := range module.ExtensionPoints {
			if point.OwnerModuleID != module.ID || !validModuleName(point.ID) || point.Version == 0 || point.Target.Namespace == "" || point.Target.Name == "" || point.Target.Version < 1 || len(point.AllowedKinds) != 1 {
				return nil, fmt.Errorf("invalid extension point in module %q", module.ID)
			}
			key := string(module.ID) + "/" + point.ID
			if _, ok := points[key]; ok {
				return nil, fmt.Errorf("duplicate extension point %q", key)
			}
			target, ownsTarget := module.OwnerOf(point.Target)
			if !ownsTarget || target.Ref != point.Target {
				return nil, fmt.Errorf("extension point %q target is not owned by its declaring module", key)
			}
			if err := validateModuleSchema(point.InputSchema, registered); err != nil {
				return nil, fmt.Errorf("extension point %q input: %w", key, err)
			}
			if err := validateModuleSchema(point.OutputSchema, registered); err != nil {
				return nil, fmt.Errorf("extension point %q output: %w", key, err)
			}
			allowed := map[ExtensionKind]bool{}
			for _, rawKind := range point.AllowedKinds {
				kind := ExtensionKind(rawKind)
				if !validExtensionKind(kind) || allowed[kind] {
					return nil, fmt.Errorf("extension point %q has invalid or duplicate allowed kind %q", key, rawKind)
				}
				allowed[kind] = true
			}
			protected := map[string]bool{}
			for _, field := range point.ProtectedFields {
				if !validModuleName(field) || protected[field] {
					return nil, fmt.Errorf("extension point %q has invalid or duplicate protected field %q", key, field)
				}
				protected[field] = true
			}
			points[key] = point
		}
	}
	byID := map[string]plannedModuleExtension{}
	byPoint := map[string][]plannedModuleExtension{}
	orders := map[string]map[int32]bool{}
	fields := map[string]bool{}
	for _, module := range modules {
		for _, extension := range module.Extensions {
			if len(extension.AddedFields) > 128 || len(extension.DependsOn) > 256 {
				return nil, fmt.Errorf("extension %s/%s exceeds field or dependency limits", module.ID, extension.ID)
			}
			key := extension.PointID
			point, ok := points[key]
			if !ok {
				return nil, fmt.Errorf("extension point %q is undeclared", key)
			}
			if !validModuleName(extension.ID) || extension.ExtenderModuleID != module.ID || extension.PointVersion != point.Version || !validDigest(extension.ArtifactDigest) {
				return nil, fmt.Errorf("extension %q has invalid identity, digest, owner or point version", key)
			}
			allowed := false
			for _, rawKind := range point.AllowedKinds {
				allowed = allowed || ExtensionKind(rawKind) == extension.Kind
			}
			if !allowed {
				return nil, fmt.Errorf("extension %s/%s uses undeclared kind %q", module.ID, extension.ID, extension.Kind)
			}
			phase, failure := extensionPolicy(extension.Kind)
			if phase == "" || point.ExecutionPhase != phase || point.FailurePolicy != failure {
				return nil, fmt.Errorf("extension point %q has an invalid phase or failure policy for %q", key, extension.Kind)
			}
			if extension.Kind == ExtensionValidation && extension.Event != "" && extension.Event != "validate" || extension.Kind == ExtensionFields && extension.Event != "" || extension.Kind == ExtensionPostCommit && !validPostCommitExtensionEvent(extension.Event) {
				return nil, fmt.Errorf("extension %s/%s has an invalid event for kind %q", module.ID, extension.ID, extension.Kind)
			}
			handler, handlerOwned := module.OwnerOf(extension.HandlerRef)
			if !handlerOwned || handler.Ref != extension.HandlerRef {
				return nil, fmt.Errorf("extension %s/%s handler is not owned by its extender", module.ID, extension.ID)
			}
			if extension.Kind != ExtensionFields && len(extension.AddedFields) != 0 {
				return nil, fmt.Errorf("extension %s/%s adds fields for non-field extension kind", module.ID, extension.ID)
			}
			if extension.Kind == ExtensionFields && len(extension.AddedFields) == 0 {
				return nil, fmt.Errorf("field extension %s/%s declares no fields", module.ID, extension.ID)
			}
			if orders[key] == nil {
				orders[key] = map[int32]bool{}
			}
			if orders[key][extension.Order] {
				return nil, fmt.Errorf("extension point %q has ambiguous order %d", key, extension.Order)
			}
			orders[key][extension.Order] = true
			globalID := string(module.ID) + "/" + extension.ID
			if _, exists := byID[globalID]; exists {
				return nil, fmt.Errorf("duplicate extension identity %q", globalID)
			}
			item := plannedModuleExtension{point: point, registration: extension, key: globalID}
			byID[globalID] = item
			byPoint[key] = append(byPoint[key], item)
			for _, field := range extension.AddedFields {
				if !validModuleName(field.Key) || !strings.HasPrefix(field.Key, string(module.ID)+".") || field.Schema.ID == "" || field.Schema.Version == 0 {
					return nil, fmt.Errorf("extension field %q must be extender namespaced and versioned", field.Key)
				}
				for _, protected := range point.ProtectedFields {
					if field.Key == protected {
						return nil, fmt.Errorf("extension field %q overrides an owner-protected field", field.Key)
					}
				}
				fieldKey := key + "/" + field.Key
				if fields[fieldKey] {
					return nil, fmt.Errorf("extension field %q is declared more than once", field.Key)
				}
				fields[fieldKey] = true
				if registered != nil && !registered(field.Schema) {
					return nil, fmt.Errorf("extension field %q schema is unregistered", field.Key)
				}
			}
			if _, ok := ownerModules[point.OwnerModuleID]; !ok {
				return nil, fmt.Errorf("extension point owner is not installed")
			}
		}
	}
	pointKeys := make([]string, 0, len(byPoint))
	for key := range byPoint {
		pointKeys = append(pointKeys, key)
	}
	sort.Strings(pointKeys)
	result := make([]ResolvedModuleExtension, 0, len(byID))
	for _, pointKey := range pointKeys {
		remaining := make(map[string]plannedModuleExtension, len(byPoint[pointKey]))
		for _, item := range byPoint[pointKey] {
			remaining[item.key] = item
		}
		for len(remaining) > 0 {
			var ready []plannedModuleExtension
			for _, item := range remaining {
				blocked := false
				for _, dependency := range item.registration.DependsOn {
					dependencyItem, exists := byID[dependency]
					if !exists || dependencyItem.registration.PointID != pointKey {
						return nil, fmt.Errorf("extension %q has an unknown or cross-point dependency %q", item.key, dependency)
					}
					if _, pending := remaining[dependency]; pending {
						blocked = true
						break
					}
				}
				if !blocked {
					ready = append(ready, item)
				}
			}
			if len(ready) == 0 {
				return nil, fmt.Errorf("extension dependency cycle at point %q", pointKey)
			}
			sort.Slice(ready, func(i, j int) bool { return extensionLess(ready[i], ready[j]) })
			for _, item := range ready {
				result = append(result, ResolvedModuleExtension{Point: item.point, Registration: item.registration})
				delete(remaining, item.key)
			}
		}
	}
	return result, nil
}

func normalizeModuleExtensions(modules []ModuleDescriptor) []ModuleDescriptor {
	ordered := append([]ModuleDescriptor(nil), modules...)
	for i := range ordered {
		module := &ordered[i]
		module.ExtensionPoints = append([]ExtensionPoint(nil), module.ExtensionPoints...)
		for j := range module.ExtensionPoints {
			point := &module.ExtensionPoints[j]
			point.AllowedKinds = append([]string(nil), point.AllowedKinds...)
			point.ProtectedFields = append([]string(nil), point.ProtectedFields...)
			sort.Strings(point.AllowedKinds)
			sort.Strings(point.ProtectedFields)
		}
		sort.Slice(module.ExtensionPoints, func(i, j int) bool { return module.ExtensionPoints[i].ID < module.ExtensionPoints[j].ID })
		module.Extensions = append([]ExtensionRegistration(nil), module.Extensions...)
		for j := range module.Extensions {
			extension := &module.Extensions[j]
			extension.DependsOn = append([]string(nil), extension.DependsOn...)
			sort.Strings(extension.DependsOn)
			extension.AddedFields = append([]ExtensionField(nil), extension.AddedFields...)
			sort.Slice(extension.AddedFields, func(i, j int) bool { return extension.AddedFields[i].Key < extension.AddedFields[j].Key })
		}
		sort.Slice(module.Extensions, func(i, j int) bool { return module.Extensions[i].ID < module.Extensions[j].ID })
	}
	return ordered
}

func validExtensionKind(kind ExtensionKind) bool {
	return kind == ExtensionFields || kind == ExtensionValidation || kind == ExtensionPostCommit
}

func extensionPolicy(kind ExtensionKind) (string, string) {
	switch kind {
	case ExtensionFields:
		return "activation", "reject"
	case ExtensionValidation:
		return "before_commit", "reject"
	case ExtensionPostCommit:
		return "after_commit", "retry"
	default:
		return "", ""
	}
}

func validPostCommitExtensionEvent(event string) bool {
	switch event {
	case "after_insert", "after_save", "after_delete", "after_submit", "after_cancel":
		return true
	default:
		return false
	}
}

func extensionLess(left, right plannedModuleExtension) bool {
	if left.registration.Order != right.registration.Order {
		return left.registration.Order < right.registration.Order
	}
	if left.registration.ExtenderModuleID != right.registration.ExtenderModuleID {
		return left.registration.ExtenderModuleID < right.registration.ExtenderModuleID
	}
	return left.registration.ID < right.registration.ID
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
	Scope             BusinessScope
	workflowPin       *WorkflowCommandPin
	seal              [32]byte
}

type WorkflowCommandPin struct {
	InstanceID     InstanceID
	StepID         StepID
	IdempotencyKey IdempotencyKey
}

func (e ModuleExecution) WithWorkflowCommandPin(instance InstanceID, step StepID, key IdempotencyKey) (ModuleExecution, error) {
	if !e.Verified() || instance == "" || step == "" || key == "" {
		return ModuleExecution{}, fmt.Errorf("workflow command pin requires verified module identity and durable action identity")
	}
	e.workflowPin = &WorkflowCommandPin{InstanceID: instance, StepID: step, IdempotencyKey: key}
	e.seal = e.identitySeal()
	return e, nil
}

func (e ModuleExecution) WorkflowCommandPin() (WorkflowCommandPin, bool) {
	if !e.Verified() || e.workflowPin == nil {
		return WorkflowCommandPin{}, false
	}
	return *e.workflowPin, true
}

// BusinessRef is a stable provider-qualified identity, never a local row name.
//
//contract:wire:v2
type BusinessRef struct {
	ProviderID   string        `json:"provider_id"`
	ResourceType string        `json:"resource_type"`
	ID           string        `json:"id"`
	Scope        BusinessScope `json:"scope"`
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
	Scope      BusinessScope
	ProviderID string
}
type ResolvedBusinessRef struct {
	Canonical BusinessRef
	Revision  uint64
	Display   string
}
type BusinessReferenceResolver interface {
	Resolve(context.Context, ScopeContext, BusinessRef) (ResolvedBusinessRef, error)
}

// BusinessReferenceProviderHandle is a granted capability handle that also
// resolves provider-owned business identities in the handle's exact scope.
type BusinessReferenceProviderHandle interface {
	CapabilityHandle
	BusinessReferenceResolver
}

// BusinessReferenceValidator is injected from a verified runtime binding plan
// into the shared document validation path. It is deliberately not serializable
// and cannot be supplied through an API payload.
type BusinessReferenceValidator func(ModuleID, ReferenceRule, json.RawMessage) error

// HistoricalSnapshotValidator checks a retained snapshot against its declared
// field schema and verified transaction scope without consulting its provider.
type HistoricalSnapshotValidator func(SchemaRef, HistoricalSnapshot) error

func ValidateBusinessReference(ctx context.Context, resolver BusinessReferenceResolver, scope ScopeContext, ref BusinessRef, expectedResourceType string) (ResolvedBusinessRef, error) {
	if ctx == nil || scope.Scope.Validate() != nil || ref.Scope.Validate() != nil || ref.Scope != scope.Scope || ref.ProviderID == "" || scope.ProviderID == "" || ref.ProviderID != scope.ProviderID || ref.ResourceType == "" || ref.ID == "" {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference requires verified scope and provider-qualified identity")
	}
	if ref.ResourceType != expectedResourceType {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference resource type mismatch")
	}
	if resolver == nil {
		return ResolvedBusinessRef{}, fmt.Errorf("business reference provider is unavailable")
	}
	resolved, err := resolver.Resolve(ctx, scope, ref)
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
	Source        BusinessRef   `json:"source"`
	BusinessScope BusinessScope `json:"business_scope"`
	CapturedAt    time.Time     `json:"captured_at"`
	Schema        SchemaRef     `json:"schema"`
	//contract:json
	Data json.RawMessage `json:"data"`
}

// ValidateHistoricalSnapshot checks the captured payload against its stored
// schema without resolving the source provider.
func ValidateHistoricalSnapshot(snapshot HistoricalSnapshot, validate func(SchemaRef, json.RawMessage) error) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if validate == nil {
		return fmt.Errorf("historical snapshot schema validator is unavailable")
	}
	if err := validate(snapshot.Schema, append(json.RawMessage(nil), snapshot.Data...)); err != nil {
		return fmt.Errorf("historical snapshot data failed captured schema: %w", err)
	}
	return nil
}

func (s HistoricalSnapshot) Validate() error {
	if err := s.BusinessScope.Validate(); err != nil {
		return fmt.Errorf("historical snapshot business scope: %w", err)
	}
	if err := s.Source.Scope.Validate(); err != nil || s.Source.Scope != s.BusinessScope {
		return fmt.Errorf("historical snapshot source scope does not match snapshot scope")
	}
	if s.Source.ProviderID == "" || s.Source.ResourceType == "" || s.Source.ID == "" {
		return fmt.Errorf("historical snapshot source identity is required")
	}
	if s.Schema.ID == "" || s.Schema.Version == 0 || len(s.Data) == 0 || !json.Valid(s.Data) {
		return fmt.Errorf("historical snapshot schema and JSON data are required")
	}
	return nil
}

// BusinessEventDefinition ties an authoritative business event to its owning module.
//
//contract:wire:v2
type BusinessEventDefinition struct {
	Type          string    `json:"type" yaml:"type"`
	PayloadSchema SchemaRef `json:"payload_schema" yaml:"payload_schema"`
	OwnerModuleID ModuleID  `json:"owner_module_id" yaml:"owner_module_id"`
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
	ExecutionID      string           `json:"execution_id"`
	Scope            BusinessScope    `json:"scope"`
	CallerModuleID   ModuleID         `json:"caller_module_id"`
	ProviderModuleID ModuleID         `json:"provider_module_id,omitempty"`
	ProviderID       string           `json:"provider_id"`
	BindingRevision  uint64           `json:"binding_revision"`
	Operation        CapabilityAction `json:"operation"`
	IdempotencyKey   IdempotencyKey   `json:"idempotency_key"`
	InputHash        string           `json:"input_hash"`
}

func (d PinnedDispatch) Validate() error {
	if d.ExecutionID == "" || d.Scope.Validate() != nil || d.CallerModuleID == "" || d.ProviderID == "" || d.BindingRevision == 0 || d.Operation.OperationName == "" || d.IdempotencyKey == "" || d.InputHash == "" {
		return fmt.Errorf("pinned dispatch identity is incomplete")
	}
	if d.Operation.CapabilityName == "" || d.Operation.InputSchema.ID == "" || d.Operation.InputSchema.Version == 0 || d.Operation.OutputSchema.ID == "" || d.Operation.OutputSchema.Version == 0 {
		return fmt.Errorf("pinned dispatch operation contract is incomplete")
	}
	decoded, err := hex.DecodeString(d.InputHash)
	if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != d.InputHash {
		return fmt.Errorf("pinned dispatch input hash must be canonical SHA-256 hex")
	}
	return nil
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
	ModuleID           ModuleID             `json:"module_id"`
	TargetState        ModuleLifecycleState `json:"target_state"`
	TargetVersion      string               `json:"target_version,omitempty"`
	ProviderSelections []ProviderSelection  `json:"provider_selections,omitempty"`
	ExpectedRevision   uint64               `json:"expected_revision"`
}

// ModuleChangePlan carries bounded reasons why an operation cannot proceed.
//
//contract:wire:v2
type ModuleChangePlan struct {
	ModuleID           ModuleID             `json:"module_id"`
	TargetState        ModuleLifecycleState `json:"target_state"`
	TargetVersion      string               `json:"target_version,omitempty"`
	ProviderSelections []ProviderSelection  `json:"provider_selections,omitempty"`
	PlanHash           string               `json:"plan_hash"`
	BaseRevision       uint64               `json:"base_revision"`
	DependencyBlockers []ModuleBlocker      `json:"dependency_blockers,omitempty"`
	OpenWorkBlockers   []ModuleBlocker      `json:"open_work_blockers,omitempty"`
	BindingChanges     []string             `json:"binding_changes,omitempty"`
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
	Meta               CommandMeta          `json:"meta"`
	ModuleID           ModuleID             `json:"module_id"`
	TargetState        ModuleLifecycleState `json:"target_state"`
	TargetVersion      string               `json:"target_version,omitempty"`
	ProviderSelections []ProviderSelection  `json:"provider_selections,omitempty"`
	PlanHash           string               `json:"plan_hash"`
	ExpectedRevision   uint64               `json:"expected_revision"`
}

func ValidateApplyModuleChange(cmd ApplyModuleChange) error {
	if _, err := ParseIdempotencyKey(string(cmd.Meta.IdempotencyKey)); err != nil || cmd.ModuleID == "" || len(cmd.PlanHash) != 64 || cmd.ExpectedRevision == 0 || cmd.ExpectedRevision > 1<<63-1 {
		return fmt.Errorf("module lifecycle command identity, plan hash, or revision is invalid")
	}
	if cmd.TargetState != ModuleActive && cmd.TargetState != ModuleDraining && cmd.TargetState != ModuleDisabled && cmd.TargetState != ModuleRemoved {
		return fmt.Errorf("module lifecycle target state is invalid")
	}
	if _, err := hex.DecodeString(cmd.PlanHash); err != nil {
		return fmt.Errorf("module lifecycle plan hash is invalid")
	}
	if err := ValidatePlanModuleChange(PlanModuleChange{ModuleID: cmd.ModuleID, TargetState: cmd.TargetState, TargetVersion: cmd.TargetVersion, ProviderSelections: cmd.ProviderSelections, ExpectedRevision: cmd.ExpectedRevision}); err != nil {
		return err
	}
	return nil
}

type OpenWorkInspector interface {
	InspectOpenWork(context.Context, TenantID, ModuleID, uint16) ([]ModuleBlocker, error)
}

// OpenWorkDomainInspector declares which lifecycle work category it covers.
// Registration is explicit so one adapter cannot accidentally stand in for
// unrelated owner work such as obligations or event delivery.
type OpenWorkDomainInspector interface {
	OpenWorkDomain() string
	OpenWorkInspector
}

type OpenWorkCoverageInspector interface {
	OpenWorkDomains() []string
	OpenWorkInspector
}

func ValidateModuleLifecycleTransition(from, to ModuleLifecycleState) error {
	switch from {
	case ModuleInstalled:
		if to == ModuleActive {
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

func ValidatePlanModuleChange(request PlanModuleChange) error {
	if request.ModuleID == "" || request.ExpectedRevision == 0 || request.ExpectedRevision > 1<<63-1 {
		return fmt.Errorf("module plan identity and expected revision are required")
	}
	if request.TargetState != ModuleActive && request.TargetState != ModuleDraining && request.TargetState != ModuleDisabled && request.TargetState != ModuleRemoved {
		return fmt.Errorf("module plan target state is invalid")
	}
	if len(request.TargetVersion) > 128 || strings.ContainsAny(request.TargetVersion, "\r\n\x00") {
		return fmt.Errorf("module plan target version is invalid")
	}
	seen := map[BindingKey]bool{}
	for _, selection := range request.ProviderSelections {
		if selection.Validate() != nil || selection.Key.ConsumerModuleID != request.ModuleID || seen[selection.Key] {
			return fmt.Errorf("provider cutover must contain unique selections for the target module")
		}
		seen[selection.Key] = true
	}
	return nil
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

// ValidateConsumerDefinition binds every accepted payload schema to a known
// event contract and an explicit consumer owner.
func ValidateConsumerDefinition(consumer ConsumerDefinition, owner ModuleID, events map[string]BusinessEventDefinition) error {
	if consumer.ConsumerID == "" || consumer.OwnerModuleID == "" || consumer.OwnerModuleID != owner || len(consumer.AcceptedEventSchemas) == 0 || len(consumer.AcceptedEventSchemas) > 100 {
		return fmt.Errorf("consumer identity, owner, and bounded event subscriptions are required")
	}
	for eventType, schema := range consumer.AcceptedEventSchemas {
		definition, ok := events[eventType]
		if !ok || definition.Type != eventType || definition.PayloadSchema != schema || definition.OwnerModuleID == "" {
			return fmt.Errorf("consumer subscription %q does not match a registered owned event schema", eventType)
		}
	}
	return nil
}

// BusinessEventPayloadSchema reads the domain schema marker carried inside
// Data; EventEnvelope.Version remains the transport contract version.
func BusinessEventPayloadSchema(data json.RawMessage) (SchemaRef, error) {
	var payload struct {
		Schema SchemaRef       `json:"schema"`
		Body   json.RawMessage `json:"payload"`
	}
	if !json.Valid(data) || json.Unmarshal(data, &payload) != nil || payload.Schema.ID == "" || payload.Schema.Version == 0 || len(payload.Body) == 0 || !json.Valid(payload.Body) {
		return SchemaRef{}, fmt.Errorf("business event data must contain a versioned schema and valid payload")
	}
	return payload.Schema, nil
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
}, tx *sql.Tx, schemas interface {
	Registered(SchemaRef) bool
	Validate(SchemaRef, json.RawMessage) error
}, execution ModuleExecution, definition BusinessEventDefinition, payloadSchema SchemaRef, payload json.RawMessage, site, aggregateType, aggregateID, correlationID, causationID string) (string, error) {
	if !execution.Verified() || site != string(execution.TenantID) {
		return "", fmt.Errorf("business event requires verified matching module execution")
	}
	if execution.OwnerModuleID != definition.OwnerModuleID {
		return "", fmt.Errorf("business event can only be published by its owning module")
	}
	if err := ValidateBusinessEvent(definition, execution.OwnerModuleID, payloadSchema, definition.Type); err != nil {
		return "", err
	}
	if writer == nil || tx == nil || schemas == nil || !schemas.Registered(payloadSchema) || !json.Valid(payload) {
		return "", fmt.Errorf("business event writer, transaction, and valid payload are required")
	}
	if err := schemas.Validate(payloadSchema, payload); err != nil {
		return "", fmt.Errorf("business event payload failed its registered schema: %w", err)
	}
	data, err := json.Marshal(struct {
		Schema  SchemaRef       `json:"schema"`
		Payload json.RawMessage `json:"payload"`
	}{Schema: payloadSchema, Payload: payload})
	if err != nil {
		return "", err
	}
	event := EventEnvelope{ID: NewEventID(), Type: definition.Type, Version: CurrentVersion, Source: string(execution.OwnerModuleID), Site: site, BusinessScope: execution.Scope, AggregateType: aggregateType, AggregateID: aggregateID, OccurredAt: time.Now().UTC(), CorrelationID: correlationID, CausationID: causationID, Data: data}
	if err := writer.Append(ctx, tx, event); err != nil {
		return "", err
	}
	return event.ID, nil
}

func NewModuleExecution(scope BusinessScope, caller, owner ModuleID, revision uint64, operation string) (ModuleExecution, error) {
	if scope.Validate() != nil || caller == "" || owner == "" || revision == 0 || operation == "" {
		return ModuleExecution{}, fmt.Errorf("module execution identity is incomplete")
	}
	e := ModuleExecution{TenantID: scope.TenantID, CallerModuleID: caller, OwnerModuleID: owner, BindingRevision: revision, AllowedOperation: operation, Scope: scope}
	e.seal = e.identitySeal()
	return e, nil
}

func NewCommandModuleExecution(scope BusinessScope, caller, owner ModuleID, revision uint64, command, operation string) (ModuleExecution, error) {
	e, err := NewModuleExecution(scope, caller, owner, revision, operation)
	if err != nil {
		return ModuleExecution{}, err
	}
	if strings.TrimSpace(command) == "" {
		return ModuleExecution{}, fmt.Errorf("authorized domain command is required")
	}
	e.AuthorizedCommand = command
	e.seal = e.identitySeal()
	return e, nil
}

func (e ModuleExecution) Verified() bool {
	return e.TenantID == e.Scope.TenantID && e.Scope.Validate() == nil && e.CallerModuleID != "" && e.OwnerModuleID != "" && e.BindingRevision != 0 && e.AllowedOperation != "" && e.seal != ([32]byte{}) && e.seal == e.identitySeal()
}

func (e ModuleExecution) identitySeal() [32]byte {
	identity, _ := json.Marshal(struct {
		TenantID          TenantID
		CallerModuleID    ModuleID
		OwnerModuleID     ModuleID
		BindingRevision   uint64
		AllowedOperation  string
		AuthorizedCommand string
		Scope             BusinessScope
		WorkflowPin       *WorkflowCommandPin
	}{e.TenantID, e.CallerModuleID, e.OwnerModuleID, e.BindingRevision, e.AllowedOperation, e.AuthorizedCommand, e.Scope, e.workflowPin})
	return sha256.Sum256(identity)
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
		if resource.ScopeKind != ScopeTenantShared && resource.ScopeKind != ScopeEntityOwned {
			return fmt.Errorf("module %q resource %q requires an explicit business scope policy", m.ID, key)
		}
		if resource.ScopeKind == ScopeEntityOwned && !validScopeField(resource.ScopeField) {
			return fmt.Errorf("module %q resource %q requires a valid entity scope field", m.ID, key)
		}
	}
	queryNames := map[string]bool{}
	queryOperations := make(map[string]OperationSpec)
	for _, offer := range m.Provides {
		for _, operation := range offer.Operations {
			queryOperations[string(offer.Name)+"/"+operation.Name] = operation
		}
	}
	for _, query := range m.Queries {
		if query.OwnerModuleID != m.ID || !moduleIDPattern.MatchString(string(query.OwnerModuleID)) || !validModuleName(query.Name) || len("module.")+len(query.OwnerModuleID)+len(".query.")+len(query.Name) > 160 || queryNames[query.Name] || query.Capability == "" || !validModuleName(query.Operation) {
			return fmt.Errorf("module %q has invalid or duplicate published query", m.ID)
		}
		queryNames[query.Name] = true
		if query.ScopePolicy != ScopeTenantShared && query.ScopePolicy != ScopeEntityOwned {
			return fmt.Errorf("module %q query %q requires an explicit scope policy", m.ID, query.Name)
		}
		if query.PageLimit == 0 || query.PageLimit > 1000 || len(query.AllowedFields) == 0 {
			return fmt.Errorf("module %q query %q requires allowed fields and a page limit from 1 to 1000", m.ID, query.Name)
		}
		fields := map[string]bool{}
		for _, field := range query.AllowedFields {
			if !validModuleName(field) || fields[field] {
				return fmt.Errorf("module %q query %q has an invalid or duplicate allowed field", m.ID, query.Name)
			}
			fields[field] = true
		}
		operation, ok := queryOperations[string(query.Capability)+"/"+query.Operation]
		if !ok || operation.Effect != EffectQuery || operation.Input != query.InputSchema || operation.Output != query.OutputSchema {
			return fmt.Errorf("module %q query %q must match a declared query capability operation", m.ID, query.Name)
		}
		if err := validateModuleSchema(query.InputSchema, registeredSchema); err != nil {
			return err
		}
		if err := validateModuleSchema(query.OutputSchema, registeredSchema); err != nil {
			return err
		}
	}
	projectionIDs := map[string]bool{}
	for _, projection := range m.Projections {
		if projection.Validate() != nil || projection.OwnerModuleID != m.ID || len("module.")+len(projection.OwnerModuleID)+len(".projection.")+len(projection.ID) > 160 || projectionIDs[projection.ID] {
			return fmt.Errorf("module %q has an invalid projection definition", m.ID)
		}
		projectionIDs[projection.ID] = true
		if err := validateModuleSchema(projection.OutputSchema, registeredSchema); err != nil {
			return err
		}
		for _, source := range projection.Sources {
			if !moduleIDPattern.MatchString(string(source.OwnerModuleID)) || !validModuleName(source.QueryName) {
				return fmt.Errorf("module %q projection %q references an invalid published query", m.ID, projection.ID)
			}
			if source.OwnerModuleID == m.ID {
				if !queryNames[source.QueryName] {
					return fmt.Errorf("module %q projection %q references unpublished query %s/%s", m.ID, projection.ID, source.OwnerModuleID, source.QueryName)
				}
			}
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

func validScopeField(field string) bool {
	if field == "" || len(field) > 64 {
		return false
	}
	for i, r := range field {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

var moduleNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

func validModuleName(name string) bool { return moduleNamePattern.MatchString(name) }

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
	if len(modules) > 256 {
		return fmt.Errorf("module set exceeds the supported limit")
	}
	ids, owners := map[ModuleID]bool{}, map[string]ModuleID{}
	queries := map[ModuleID]map[string]PublishedQuery{}
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
		queries[module.ID] = make(map[string]PublishedQuery, len(module.Queries))
		for _, query := range module.Queries {
			queries[module.ID][query.Name] = query
		}
		for _, resource := range module.Resources {
			key := fmt.Sprintf("%s/%s", resource.Ref.Namespace, resource.Ref.Name)
			if owner, ok := owners[key]; ok {
				return fmt.Errorf("resource %q has duplicate owners %q and %q", key, owner, module.ID)
			}
			owners[key] = module.ID
		}
	}
	for _, module := range ordered {
		for _, projection := range module.Projections {
			for _, source := range projection.Sources {
				query, ok := queries[source.OwnerModuleID][source.QueryName]
				if ok {
					factKeyPublished := false
					for _, field := range query.AllowedFields {
						factKeyPublished = factKeyPublished || field == projection.FactKey
					}
					allowedFieldsPublished := true
					for _, field := range projection.AllowedFields {
						found := false
						for _, sourceField := range query.AllowedFields {
							found = found || field == sourceField
						}
						allowedFieldsPublished = allowedFieldsPublished && found
					}
					if query.OutputSchema != source.OutputSchema || query.ScopePolicy != projection.ScopePolicy || !factKeyPublished || !allowedFieldsPublished {
						return fmt.Errorf("module %q projection %q source %s/%s has an incompatible output or fact-key contract", module.ID, projection.ID, source.OwnerModuleID, source.QueryName)
					}
				}
			}
		}
	}
	return ValidateModuleExtensions(ordered, registered)
}
