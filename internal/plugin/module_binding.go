package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
)

type ProviderKind string

const (
	ProviderInternal ProviderKind = "internal"
	ProviderExternal ProviderKind = "external"
)

const maxPublishedProjectionRows = 100000

type BindingKey struct {
	Scope            contract.BusinessScope
	ConsumerModuleID contract.ModuleID
	CapabilityName   contract.CapabilityName
}
type ProviderRegistration struct {
	TenantID                    contract.TenantID
	Scope                       contract.BusinessScope
	ProviderID                  string
	OwnerModuleID               contract.ModuleID
	Kind                        ProviderKind
	Offers                      []contract.CapabilityOffer
	Factories                   map[contract.CapabilityName]ProviderFactory
	Queries                     map[string]PublishedQueryExecutor
	BusinessReconciliationRules []BusinessReconciliationRule
	ValidateSchema              func(contract.SchemaRef, []byte) error
}

// BusinessReconciliationRule is registered with its owning module provider.
// Source and target data still flow through the declared published-query
// contracts in the resolved binding plan.
type BusinessReconciliationRule struct {
	OwnerModuleID            contract.ModuleID
	ID                       string
	Version                  uint32
	SourceQueryOwnerModuleID contract.ModuleID
	SourceQueryName          string
	SourceQueryInput         json.RawMessage
	SourcePageLimit          uint16
	TargetQueryOwnerModuleID contract.ModuleID
	TargetQueryName          string
	TargetQueryInput         json.RawMessage
	TargetPageLimit          uint16
	GracePeriod              time.Duration
	MaxRows                  int
	RepairCallerModuleID     contract.ModuleID
	RepairCapability         contract.CapabilityName
	RepairOperation          string
	RepairLeaseID            string
}
type ProviderSelection struct {
	Key        BindingKey
	ProviderID string
}
type ResolvedBinding struct {
	Key            BindingKey
	ProviderID     string
	OwnerModuleID  contract.ModuleID
	Kind           ProviderKind
	Contract       contract.SchemaRef
	Operations     []contract.OperationSpec
	Revision       uint64
	Factory        ProviderFactory
	validateSchema func(contract.SchemaRef, []byte) error
}

func (b ResolvedBinding) Open(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
	if b.Factory == nil {
		return nil, fmt.Errorf("provider %q is unavailable", b.ProviderID)
	}
	if ctx.Site != string(b.Key.Scope.TenantID) {
		return nil, fmt.Errorf("capability binding tenant does not match execution")
	}
	if ctx.Scope != b.Key.Scope || ctx.Scope.Validate() != nil {
		return nil, fmt.Errorf("capability binding business scope does not match execution")
	}
	return b.Factory.Open(ctx)
}

func (p BindingPlan) Resolve(key BindingKey) (ResolvedBinding, bool) {
	for _, binding := range p.Bindings {
		if binding.Key == key {
			return binding, true
		}
	}
	return ResolvedBinding{}, false
}

// ProviderSelections returns the immutable provider choices represented by a
// resolved plan in deterministic key order.
func (p BindingPlan) ProviderSelections() ([]contract.ProviderSelection, error) {
	byKey := make(map[contract.BindingKey]string, len(p.Bindings)+len(p.Queries))
	add := func(key contract.BindingKey, providerID string) error {
		if providerID == "" {
			return fmt.Errorf("resolved provider selection has no provider identity")
		}
		if existing, ok := byKey[key]; ok && existing != providerID {
			return fmt.Errorf("capability %q resolved to conflicting providers", key.CapabilityName)
		}
		byKey[key] = providerID
		return nil
	}
	for _, binding := range p.Bindings {
		key := contract.BindingKey{Scope: binding.Key.Scope, ConsumerModuleID: binding.Key.ConsumerModuleID, CapabilityName: binding.Key.CapabilityName}
		if err := add(key, binding.ProviderID); err != nil {
			return nil, err
		}
	}
	for key, query := range p.Queries {
		selectionKey := contract.BindingKey{Scope: key.Scope, ConsumerModuleID: key.OwnerModuleID, CapabilityName: query.Definition.Capability}
		if err := add(selectionKey, query.ProviderID); err != nil {
			return nil, err
		}
	}
	selections := make([]contract.ProviderSelection, 0, len(byKey))
	for key, providerID := range byKey {
		selections = append(selections, contract.ProviderSelection{Key: key, ProviderID: providerID})
	}
	sort.Slice(selections, func(i, j int) bool {
		a, b := selections[i].Key, selections[j].Key
		if a.Scope.TenantID != b.Scope.TenantID {
			return a.Scope.TenantID < b.Scope.TenantID
		}
		if a.Scope.Kind != b.Scope.Kind {
			return a.Scope.Kind < b.Scope.Kind
		}
		if a.Scope.LegalEntityID != b.Scope.LegalEntityID {
			return a.Scope.LegalEntityID < b.Scope.LegalEntityID
		}
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	return selections, nil
}

type BindingDiagnostic struct {
	Key  BindingKey
	Code string
}
type BindingPlan struct {
	Revision                    uint64
	Composition                 contract.CompositionPlan
	Packages                    []contract.InstalledPackage
	Extensions                  []contract.ResolvedModuleExtension
	Bindings                    []ResolvedBinding
	Queries                     map[PublishedQueryKey]ResolvedPublishedQuery
	QueryDefinitions            map[PublishedQueryKey]contract.PublishedQuery
	BusinessReconciliationRules []BusinessReconciliationRule
	Projections                 map[PublishedProjectionKey]contract.ProjectionDefinition
	ProjectionScopes            map[PublishedProjectionKey]contract.ResourceScopeKind
	Blockers                    []BindingDiagnostic
	Warnings                    []BindingDiagnostic
	LeaseAdmission              CapabilityLeaseAdmission
	LeaseReader                 CapabilityLeaseReader
}

// CapabilityLeaseAdmission is the tenant module store's durable pre-effect
// gate. Implementations serialize admission against security revocation.
type CapabilityLeaseAdmission interface {
	AdmitCapabilityAttempt(context.Context, contract.CapabilityLease, time.Time) error
}

// CapabilityLeaseOwnerAdmission persists the provider ownership resolved by
// the immutable binding when issuing a lease.
type CapabilityLeaseOwnerAdmission interface {
	EnsureCapabilityLease(context.Context, contract.CapabilityLease) error
}

// CapabilityLeaseReader loads an existing durable grant without issuing or
// widening authority. Scheduled and other recovered work use this to bind to
// the grant that operators can revoke through the module store.
type CapabilityLeaseReader interface {
	LoadCapabilityLease(context.Context, contract.TenantID, string) (contract.CapabilityLease, bool, error)
}

// InstalledModules is the durable module set of one scope at one revision.
type InstalledModules struct {
	Packages   []contract.InstalledPackage
	Modules    []contract.ModuleDescriptor
	Providers  []ProviderRegistration
	Selections []ProviderSelection
	Revision   uint64
}

// ResolveInstalledModules uses the same provider resolver while including the
// durable package dependency set in deterministic preflight and plan hashing.
func ResolveInstalledModules(scope contract.BusinessScope, installed InstalledModules) (BindingPlan, error) {
	packages, modules, providers, selections, revision := installed.Packages, installed.Modules, installed.Providers, installed.Selections, installed.Revision
	plan, err := ResolveModules(scope, modules, providers, selections, revision)
	if err != nil {
		return plan, err
	}
	providerRegistrations := make([]contract.ProviderRegistration, 0, len(providers))
	for _, provider := range providers {
		providerRegistrations = append(providerRegistrations, contract.ProviderRegistration{TenantID: provider.TenantID, Scope: provider.Scope, ProviderID: provider.ProviderID, OwnerModuleID: provider.OwnerModuleID, Kind: string(provider.Kind), Offers: append([]contract.CapabilityOffer(nil), provider.Offers...)})
	}
	selected := make([]contract.ProviderSelection, 0, len(selections))
	for _, selection := range selections {
		selected = append(selected, contract.ProviderSelection{Key: contract.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName}, ProviderID: selection.ProviderID})
	}
	composition, err := contract.PlanInstalledComposition(packages, modules, providerRegistrations, selected, scope, revision)
	if err != nil {
		return plan, err
	}
	plan.Composition, plan.Packages = composition, composition.Packages
	return plan, nil
}

type PublishedProjectionKey struct {
	OwnerModuleID contract.ModuleID
	ID            string
}

type PublishedProjectionStore interface {
	Save(context.Context, analytics.PublishedProjectionSnapshot) error
	MarkStale(context.Context, contract.ProjectionDefinition, contract.BusinessScope, string) error
}

// RebuildPublishedProjection reads every source under pinned provider
// revisions, verifies each paged result, then atomically publishes one
// complete snapshot through the existing analytics rebuild store.
func (p BindingPlan) RebuildPublishedProjection(ctx context.Context, store PublishedProjectionStore, scope contract.BusinessScope, key PublishedProjectionKey, inputs map[string]json.RawMessage) error {
	definition, ok := p.Projections[key]
	if ctx == nil || !ok || store == nil || scope.Validate() != nil {
		return fmt.Errorf("published projection is unavailable")
	}
	if err := definition.Validate(); err != nil {
		return err
	}
	if contract.ValidateResourceBusinessScope(scope, definition.ScopePolicy) != nil {
		return fmt.Errorf("published projection scope is invalid")
	}
	encodedInputs, err := json.Marshal(inputs)
	if err != nil || len(encodedInputs) > 65536 {
		return fmt.Errorf("published projection inputs exceed their request bound")
	}
	validInputs := make(map[string]bool, len(definition.Sources))
	for _, source := range definition.Sources {
		validInputs[string(source.OwnerModuleID)+"/"+source.QueryName] = true
	}
	for sourceID := range inputs {
		if !validInputs[sourceID] {
			return fmt.Errorf("published projection input references an undeclared source")
		}
	}
	allRows := make([]json.RawMessage, 0)
	factKeys := make(map[string]bool)
	projectionFields := make(map[string]bool, len(definition.AllowedFields))
	for _, field := range definition.AllowedFields {
		projectionFields[field] = true
	}
	var sourceRevisions []string
	replayCursors := make(map[string]string, len(definition.Sources))
	var projectionAsOf time.Time
	var projectionSnapshotToken string
	for _, source := range definition.Sources {
		queryKey := PublishedQueryKey{Scope: scope, OwnerModuleID: source.OwnerModuleID, Name: source.QueryName}
		resolved, exists := p.Queries[queryKey]
		factKeyPublished := false
		allProjectionFieldsPublished := true
		if exists {
			for _, field := range resolved.Definition.AllowedFields {
				factKeyPublished = factKeyPublished || field == definition.FactKey
			}
			for field := range projectionFields {
				published := false
				for _, sourceField := range resolved.Definition.AllowedFields {
					published = published || sourceField == field
				}
				allProjectionFieldsPublished = allProjectionFieldsPublished && published
			}
		}
		if !exists || resolved.replay == nil || resolved.Definition.OutputSchema != source.OutputSchema || resolved.Definition.ScopePolicy != definition.ScopePolicy || !factKeyPublished || !allProjectionFieldsPublished {
			return p.markProjectionStale(ctx, store, definition, scope)
		}
		sourceID := string(source.OwnerModuleID) + "/" + source.QueryName
		input := inputs[sourceID]
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		cursor := ""
		seenCursors := make(map[string]bool)
		var sourceRevision string
		var sourceAsOf time.Time
		var sourceSnapshotToken string
		var sourceSnapshotCursor string
		var done bool
		for page := 0; page < 1000 && !done; page++ {
			limit := resolved.Definition.PageLimit
			result, queryErr := p.ExecutePublishedQuery(ctx, queryKey, nil, QueryPageRequest{Input: input, Cursor: cursor, Limit: limit})
			if queryErr != nil || result.Freshness != contract.QueryFresh || result.SourceRevision == "" {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if sourceRevision == "" {
				sourceRevision, sourceAsOf = result.SourceRevision, result.AsOf
				sourceSnapshotCursor = result.SourceCursor
				if result.SnapshotToken == "" || sourceSnapshotCursor == "" {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
			} else if sourceRevision != result.SourceRevision || !sourceAsOf.Equal(result.AsOf) || result.SnapshotToken == "" || result.SourceCursor != sourceSnapshotCursor {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if page > 0 && result.SnapshotToken != sourceSnapshotToken {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if page == 0 {
				sourceSnapshotToken = result.SnapshotToken
				if projectionSnapshotToken == "" {
					projectionSnapshotToken = sourceSnapshotToken
				} else if sourceSnapshotToken != projectionSnapshotToken {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
			}
			var rows []json.RawMessage
			if err := json.Unmarshal(result.Data, &rows); err != nil {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			for _, row := range rows {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(row, &fields); err != nil || fields[definition.FactKey] == nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				var factValue any
				decoder := json.NewDecoder(strings.NewReader(string(fields[definition.FactKey])))
				decoder.UseNumber()
				if err := decoder.Decode(&factValue); err != nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				var factKey string
				switch value := factValue.(type) {
				case string:
					factKey = value
				case json.Number:
					factKey = value.String()
				default:
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				if strings.TrimSpace(factKey) == "" {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				if factKeys[factKey] {
					continue
				}
				factKeys[factKey] = true
				projectionRow := make(map[string]json.RawMessage, len(projectionFields))
				for field := range projectionFields {
					if value, present := fields[field]; present {
						projectionRow[field] = value
					}
				}
				canonicalRow, err := json.Marshal(projectionRow)
				if err != nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				allRows = append(allRows, canonicalRow)
				if len(allRows) > maxPublishedProjectionRows {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
			}
			encoded, err := json.Marshal(allRows)
			if err != nil || len(encoded) > 16<<20 {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if result.NextCursor == "" {
				done = true
				continue
			}
			if seenCursors[result.NextCursor] || result.NextCursor == cursor {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			seenCursors[result.NextCursor] = true
			cursor = result.NextCursor
		}
		if !done {
			return p.markProjectionStale(ctx, store, definition, scope)
		}
		// Replay every committed source change after the pinned snapshot through a
		// stable high-water cursor before publishing the projection.
		replayCursor := ""
		var throughCursor string
		var sourceReplayAsOf time.Time
		seenReplayCursors := map[string]bool{}
		for page := 0; page < 1000; page++ {
			execution, err := contract.NewModuleExecution(scope, source.OwnerModuleID, source.OwnerModuleID, resolved.Revision, resolved.Definition.Operation)
			if err != nil {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			replay, err := resolved.replay.ReplayPublishedQuery(ctx, execution, resolved.Definition, QueryReplayRequest{Input: input, After: sourceSnapshotCursor, Through: throughCursor, Replay: replayCursor, Limit: resolved.Definition.PageLimit})
			if err != nil || replay.Validate(resolved.Definition, resolved.Definition.PageLimit) != nil {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if sourceReplayAsOf.IsZero() {
				sourceReplayAsOf = replay.AsOf
			} else if !sourceReplayAsOf.Equal(replay.AsOf) {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			if throughCursor == "" {
				throughCursor = replay.ThroughCursor
			} else if throughCursor != replay.ThroughCursor {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			for _, change := range replay.Changes {
				if !change.Deleted && resolved.validate(resolved.Definition.OutputSchema, change.Data) != nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				index := -1
				for i, row := range allRows {
					key, keyErr := projectionFactKey(row, definition.FactKey)
					if keyErr != nil {
						return p.markProjectionStale(ctx, store, definition, scope)
					}
					if key == change.FactKey {
						index = i
						break
					}
				}
				if change.Deleted {
					if index >= 0 {
						allRows = append(allRows[:index], allRows[index+1:]...)
					}
					continue
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(change.Data, &fields); err != nil || fields[definition.FactKey] == nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				projected := make(map[string]json.RawMessage, len(projectionFields))
				for field := range projectionFields {
					if value, ok := fields[field]; ok {
						projected[field] = value
					}
				}
				encoded, err := json.Marshal(projected)
				if err != nil {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
				if index >= 0 {
					allRows[index] = encoded
				} else {
					allRows = append(allRows, encoded)
				}
				if len(allRows) > maxPublishedProjectionRows {
					return p.markProjectionStale(ctx, store, definition, scope)
				}
			}
			if replay.NextCursor == "" {
				break
			}
			if replay.NextCursor == replayCursor || seenReplayCursors[replay.NextCursor] {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
			seenReplayCursors[replay.NextCursor] = true
			replayCursor = replay.NextCursor
			if page == 999 {
				return p.markProjectionStale(ctx, store, definition, scope)
			}
		}
		replayCursors[sourceID] = throughCursor
		sourceRevisions = append(sourceRevisions, sourceID+"="+sourceRevision+"@"+throughCursor)
		if projectionAsOf.IsZero() {
			projectionAsOf = sourceReplayAsOf
		} else if !sourceReplayAsOf.Equal(projectionAsOf) {
			return p.markProjectionStale(ctx, store, definition, scope)
		}
	}
	data, err := json.Marshal(allRows)
	if err != nil {
		return p.markProjectionStale(ctx, store, definition, scope)
	}
	revisionBytes := sha256.Sum256([]byte(strings.Join(sourceRevisions, "\n")))
	checkpoint, err := json.Marshal(replayCursors)
	if err != nil || len(checkpoint) > 2048 {
		return p.markProjectionStale(ctx, store, definition, scope)
	}
	if err := store.Save(ctx, analytics.PublishedProjectionSnapshot{
		Definition: definition, Scope: scope, SourceID: "published_sources",
		SourceRevision: hex.EncodeToString(revisionBytes[:]), Cursor: string(checkpoint), AsOf: projectionAsOf,
		Freshness: contract.QueryFresh, Data: data,
	}); err != nil {
		return p.markProjectionStale(ctx, store, definition, scope)
	}
	return nil
}

func projectionFactKey(row json.RawMessage, field string) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil || fields[field] == nil {
		return "", fmt.Errorf("projection row is missing fact key %q", field)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(fields[field])))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch typed := value.(type) {
	case string:
		return typed, nil
	case json.Number:
		return typed.String(), nil
	default:
		return "", fmt.Errorf("projection fact key %q must be a string or number", field)
	}
}

func (p BindingPlan) markProjectionStale(ctx context.Context, store PublishedProjectionStore, definition contract.ProjectionDefinition, scope contract.BusinessScope) error {
	if ctx == nil {
		ctx = context.Background()
	}
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.MarkStale(markCtx, definition, scope, "published_sources"); err != nil {
		return fmt.Errorf("published projection rebuild failed and stale state could not be recorded: %w", err)
	}
	return fmt.Errorf("published projection rebuild failed; retained snapshot is marked stale")
}

// ResolveModules produces a deterministic candidate plan. It does not open
// providers; health is checked only for selected providers during generation assembly.
func ResolveModules(scope contract.BusinessScope, modules []contract.ModuleDescriptor, providers []ProviderRegistration, selections []ProviderSelection, revision uint64) (BindingPlan, error) {
	if scope.Validate() != nil {
		return BindingPlan{}, fmt.Errorf("binding requires explicit tenant and scope")
	}
	if err := contract.ValidateModuleSet(modules, nil); err != nil {
		return BindingPlan{}, fmt.Errorf("validate installed modules before binding: %w", err)
	}
	extensions, err := contract.OrderModuleExtensions(modules, nil)
	if err != nil {
		return BindingPlan{}, fmt.Errorf("validate installed module extensions before binding: %w", err)
	}
	if err := validateLineEntryLayouts(modules); err != nil {
		return BindingPlan{}, err
	}
	plan := BindingPlan{Revision: revision, Extensions: extensions, Queries: make(map[PublishedQueryKey]ResolvedPublishedQuery), QueryDefinitions: make(map[PublishedQueryKey]contract.PublishedQuery), Projections: make(map[PublishedProjectionKey]contract.ProjectionDefinition), ProjectionScopes: make(map[PublishedProjectionKey]contract.ResourceScopeKind)}
	selected := map[BindingKey]string{}
	declaredSelections := make(map[BindingKey]bool)
	for _, module := range modules {
		for _, requirement := range module.Requires {
			declaredSelections[BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: requirement.Name}] = true
		}
		for _, query := range module.Queries {
			declaredSelections[BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}] = true
		}
	}
	for _, selection := range selections {
		contractSelection := contract.ProviderSelection{Key: contract.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName}, ProviderID: selection.ProviderID}
		if err := contractSelection.Validate(); err != nil {
			return plan, err
		}
		if selection.Key.Scope.TenantID != scope.TenantID {
			return plan, fmt.Errorf("provider selection crosses tenant")
		}
		if selection.Key.Scope != scope {
			continue
		}
		if !declaredSelections[selection.Key] {
			return plan, fmt.Errorf("provider selection does not match an installed module capability")
		}
		if _, exists := selected[selection.Key]; exists {
			return plan, fmt.Errorf("provider selection for module %q capability %q is duplicated", selection.Key.ConsumerModuleID, selection.Key.CapabilityName)
		}
		selected[selection.Key] = selection.ProviderID
	}
	providerKeys := make(map[string]bool, len(providers))
	installedModules := make(map[contract.ModuleID]contract.ModuleDescriptor, len(modules))
	for _, module := range modules {
		installedModules[module.ID] = module
	}
	for _, provider := range providers {
		if provider.TenantID != scope.TenantID {
			continue
		}
		if _, err := contract.ParseTenantID(string(provider.TenantID)); err != nil {
			return plan, fmt.Errorf("provider registration has an invalid tenant identity")
		}
		if provider.Scope.TenantID == "" {
			if provider.Scope.Kind != "" || provider.Scope.LegalEntityID != "" {
				return plan, fmt.Errorf("provider %q has an incomplete business scope", provider.ProviderID)
			}
		} else if provider.Scope.TenantID != provider.TenantID || provider.Scope.Validate() != nil {
			return plan, fmt.Errorf("provider %q has an invalid business scope", provider.ProviderID)
		}
		if provider.ProviderID == "" || strings.TrimSpace(provider.ProviderID) != provider.ProviderID || len(provider.ProviderID) > 160 {
			return plan, fmt.Errorf("provider registration has an invalid provider identity")
		}
		providerIdentity := contract.ModuleDescriptor{ID: provider.OwnerModuleID, Version: "1.0.0"}
		if err := providerIdentity.Validate(nil); err != nil {
			return plan, fmt.Errorf("provider %q has an invalid owner identity: %w", provider.ProviderID, err)
		}
		if provider.Kind != ProviderInternal && provider.Kind != ProviderExternal {
			return plan, fmt.Errorf("provider %q has an unsupported provider kind", provider.ProviderID)
		}
		if provider.Kind == ProviderInternal {
			owner, installed := installedModules[provider.OwnerModuleID]
			if !installed {
				return plan, fmt.Errorf("internal provider %q is not owned by an installed module", provider.ProviderID)
			}
			for _, offer := range provider.Offers {
				if !moduleDeclaresOffer(owner, offer) {
					return plan, fmt.Errorf("internal provider %q advertises a capability not declared by owner module %q", provider.ProviderID, owner.ID)
				}
			}
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s", provider.TenantID, provider.Scope.Kind, provider.Scope.LegalEntityID, provider.ProviderID)
		if providerKeys[key] {
			return plan, fmt.Errorf("provider %q is registered more than once for the same scope and owner", provider.ProviderID)
		}
		providerKeys[key] = true
	}
	for _, module := range modules {
		for _, query := range module.Queries {
			declared := false
			for _, offer := range module.Provides {
				if offer.Name != query.Capability {
					continue
				}
				for _, operation := range offer.Operations {
					if operation.Name == query.Operation && operation.Effect == contract.EffectQuery && operation.Input == query.InputSchema && operation.Output == query.OutputSchema {
						declared = true
					}
				}
			}
			if query.OwnerModuleID != module.ID || !declared {
				return plan, fmt.Errorf("published query %s/%s does not match its owner's query capability contract", module.ID, query.Name)
			}
			key := PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}
			plan.QueryDefinitions[key] = query
			selectionKey := BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}
			selectedProvider := selected[selectionKey]
			var matches []ProviderRegistration
			for _, provider := range providers {
				if provider.TenantID != scope.TenantID || provider.OwnerModuleID != module.ID || provider.Queries[query.Name] == nil || provider.ProviderID == "" || (selectedProvider != "" && provider.ProviderID != selectedProvider) {
					continue
				}
				// A query executor is only eligible when it is also the selected
				// provider for the declared query capability and advertises the
				// exact operation contract. Query maps alone are not authority.
				offersQuery := false
				for _, offer := range provider.Offers {
					if offer.Name != query.Capability {
						continue
					}
					for _, operation := range offer.Operations {
						if operation.Name == query.Operation && operation.Effect == contract.EffectQuery && operation.Input == query.InputSchema && operation.Output == query.OutputSchema {
							offersQuery = true
						}
					}
				}
				if !offersQuery {
					continue
				}
				if provider.Kind != ProviderInternal && provider.Kind != ProviderExternal {
					continue
				}
				if provider.Scope.TenantID != "" && provider.Scope != scope {
					continue
				}
				if provider.Scope.TenantID == "" && (provider.Scope.Kind != "" || provider.Scope.LegalEntityID != "") {
					continue
				}
				matches = append(matches, provider)
			}
			sort.Slice(matches, func(i, j int) bool { return matches[i].ProviderID < matches[j].ProviderID })
			if selectedProvider == "" && len(matches) > 1 {
				plan.Warnings = append(plan.Warnings, BindingDiagnostic{Key: selectionKey, Code: "published_query_provider_selection_required"})
				continue
			}
			var chosen *ProviderRegistration
			for i := range matches {
				if chosen != nil {
					return plan, fmt.Errorf("published query %s/%s has multiple providers; select one explicitly", module.ID, query.Name)
				}
				chosen = &matches[i]
			}
			if chosen == nil {
				plan.Warnings = append(plan.Warnings, BindingDiagnostic{Key: selectionKey, Code: "published_query_provider_unavailable"})
				continue
			}
			if chosen.ValidateSchema == nil {
				plan.Warnings = append(plan.Warnings, BindingDiagnostic{Key: selectionKey, Code: "published_query_schema_validator_unavailable"})
				continue
			}
			executor := chosen.Queries[query.Name]
			plan.Queries[key] = ResolvedPublishedQuery{Definition: query, ProviderID: chosen.ProviderID, Revision: revision, executor: executor, replay: func() PublishedQueryReplayExecutor {
				replay, _ := executor.(PublishedQueryReplayExecutor)
				return replay
			}(), validate: chosen.ValidateSchema}
		}
		for _, projection := range module.Projections {
			projectionKey := PublishedProjectionKey{OwnerModuleID: module.ID, ID: projection.ID}
			plan.Projections[projectionKey] = projection
			plan.ProjectionScopes[projectionKey] = projection.ScopePolicy
		}
	}
	reconciliationRuleIDs := make(map[string]bool)
	for _, provider := range providers {
		if provider.TenantID != scope.TenantID {
			continue
		}
		for _, rule := range provider.BusinessReconciliationRules {
			if rule.OwnerModuleID == "" {
				rule.OwnerModuleID = provider.OwnerModuleID
			}
			key := string(rule.OwnerModuleID) + "/" + rule.ID
			if rule.OwnerModuleID != provider.OwnerModuleID || installedModules[rule.OwnerModuleID].ID == "" || !validReconciliationRegistrationName(rule.ID) || rule.Version == 0 || rule.GracePeriod < 0 || rule.MaxRows < 1 || rule.MaxRows > 10000 || rule.RepairCallerModuleID == "" || rule.RepairCapability == "" || !validReconciliationRegistrationName(rule.RepairOperation) || rule.RepairLeaseID == "" || len(rule.RepairLeaseID) > 160 || reconciliationRuleIDs[key] {
				return plan, fmt.Errorf("provider %q has an invalid or duplicate owner reconciliation rule", provider.ProviderID)
			}
			if !validBusinessQueryRegistration(rule.SourceQueryOwnerModuleID, rule.SourceQueryName, rule.SourceQueryInput, rule.SourcePageLimit) || !validBusinessQueryRegistration(rule.TargetQueryOwnerModuleID, rule.TargetQueryName, rule.TargetQueryInput, rule.TargetPageLimit) {
				return plan, fmt.Errorf("owner reconciliation rule %s has invalid published-query references", key)
			}
			callerModule := installedModules[rule.RepairCallerModuleID]
			requiresRepairCapability := false
			for _, requirement := range callerModule.Requires {
				if requirement.Name == rule.RepairCapability && requirement.Required {
					requiresRepairCapability = true
				}
			}
			if !requiresRepairCapability {
				return plan, fmt.Errorf("owner reconciliation rule %s repair command must use a declared mandatory module capability", key)
			}
			source, sourceOK := plan.QueryDefinitions[PublishedQueryKey{Scope: scope, OwnerModuleID: rule.SourceQueryOwnerModuleID, Name: rule.SourceQueryName}]
			target, targetOK := plan.QueryDefinitions[PublishedQueryKey{Scope: scope, OwnerModuleID: rule.TargetQueryOwnerModuleID, Name: rule.TargetQueryName}]
			if !sourceOK || !targetOK || rule.SourcePageLimit > source.PageLimit || rule.TargetPageLimit > target.PageLimit || !queryAllowsBusinessFields(source, true) || !queryAllowsBusinessFields(target, false) {
				return plan, fmt.Errorf("owner reconciliation rule %s references unavailable queries or incomplete business fields", key)
			}
			reconciliationRuleIDs[key] = true
			plan.BusinessReconciliationRules = append(plan.BusinessReconciliationRules, rule)
		}
	}
	sort.Slice(plan.BusinessReconciliationRules, func(i, j int) bool {
		if plan.BusinessReconciliationRules[i].OwnerModuleID != plan.BusinessReconciliationRules[j].OwnerModuleID {
			return plan.BusinessReconciliationRules[i].OwnerModuleID < plan.BusinessReconciliationRules[j].OwnerModuleID
		}
		return plan.BusinessReconciliationRules[i].ID < plan.BusinessReconciliationRules[j].ID
	})
	orderedProviders := append([]ProviderRegistration(nil), providers...)
	sort.Slice(orderedProviders, func(i, j int) bool { return orderedProviders[i].ProviderID < orderedProviders[j].ProviderID })
	for _, module := range modules {
		for _, requirement := range module.Requires {
			key := BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: requirement.Name}
			var matches []struct {
				provider ProviderRegistration
				offer    contract.CapabilityOffer
			}
			for _, provider := range orderedProviders {
				if provider.TenantID != scope.TenantID {
					continue
				}
				if provider.Scope.TenantID != "" && provider.Scope != scope {
					continue
				}
				if provider.Scope.TenantID == "" && (provider.Scope.Kind != "" || provider.Scope.LegalEntityID != "") {
					continue
				}
				if provider.ProviderID == "" || provider.OwnerModuleID == "" || (provider.Kind != ProviderInternal && provider.Kind != ProviderExternal) {
					continue
				}
				for _, offer := range provider.Offers {
					if offer.Name == requirement.Name && offer.Contract == requirement.Contract {
						matches = append(matches, struct {
							provider ProviderRegistration
							offer    contract.CapabilityOffer
						}{provider, offer})
					}
				}
			}
			if len(matches) == 0 {
				d := BindingDiagnostic{Key: key, Code: "provider_missing_or_contract_mismatch"}
				if requirement.Required {
					plan.Blockers = append(plan.Blockers, d)
				} else {
					plan.Warnings = append(plan.Warnings, d)
				}
				continue
			}
			providerID := selected[key]
			if providerID == "" && len(matches) > 1 {
				plan.Blockers = append(plan.Blockers, BindingDiagnostic{Key: key, Code: "provider_selection_required"})
				continue
			}
			if providerID == "" {
				providerID = matches[0].provider.ProviderID
			}
			var chosen *ProviderRegistration
			var offer contract.CapabilityOffer
			for i := range matches {
				if matches[i].provider.ProviderID == providerID {
					chosen = &matches[i].provider
					offer = matches[i].offer
					break
				}
			}
			if chosen == nil {
				return plan, fmt.Errorf("selected provider %q does not offer required capability", providerID)
			}
			if chosen.OwnerModuleID == module.ID {
				return plan, fmt.Errorf("module cannot bind its own provided capability")
			}
			factory := chosen.Factories[requirement.Name]
			if factory == nil {
				if requirement.Required {
					plan.Blockers = append(plan.Blockers, BindingDiagnostic{Key: key, Code: "provider_unavailable"})
				} else {
					plan.Warnings = append(plan.Warnings, BindingDiagnostic{Key: key, Code: "provider_unavailable"})
				}
				continue
			}
			plan.Bindings = append(plan.Bindings, ResolvedBinding{Key: key, ProviderID: chosen.ProviderID, OwnerModuleID: chosen.OwnerModuleID, Kind: chosen.Kind, Contract: offer.Contract, Operations: append([]contract.OperationSpec(nil), offer.Operations...), Revision: revision, Factory: factory, validateSchema: chosen.ValidateSchema})
		}
	}
	for _, rule := range plan.BusinessReconciliationRules {
		binding, ok := plan.Resolve(BindingKey{Scope: scope, ConsumerModuleID: rule.RepairCallerModuleID, CapabilityName: rule.RepairCapability})
		if !ok {
			continue // The existing capability resolver already reports a blocker.
		}
		operationFound := false
		for _, operation := range binding.Operations {
			if operation.Name == rule.RepairOperation && (operation.Effect == contract.EffectLocal || operation.Effect == contract.EffectExternal) {
				operationFound = true
			}
		}
		if !operationFound {
			return plan, fmt.Errorf("owner reconciliation rule %s repair operation is not a declared command", string(rule.OwnerModuleID)+"/"+rule.ID)
		}
	}
	if len(plan.Blockers) == 0 {
		compositionProviders := make([]contract.ProviderRegistration, 0, len(providers))
		for _, provider := range providers {
			compositionProviders = append(compositionProviders, contract.ProviderRegistration{
				TenantID: provider.TenantID, Scope: provider.Scope, ProviderID: provider.ProviderID,
				OwnerModuleID: provider.OwnerModuleID, Kind: string(provider.Kind), Offers: append([]contract.CapabilityOffer(nil), provider.Offers...),
			})
		}
		compositionSelections := make([]contract.ProviderSelection, 0, len(selections))
		for _, selection := range selections {
			compositionSelections = append(compositionSelections, contract.ProviderSelection{
				Key:        contract.BindingKey{Scope: selection.Key.Scope, ConsumerModuleID: selection.Key.ConsumerModuleID, CapabilityName: selection.Key.CapabilityName},
				ProviderID: selection.ProviderID,
			})
		}
		composition, err := contract.PlanComposition(modules, compositionProviders, compositionSelections, scope, revision)
		if err != nil {
			return plan, fmt.Errorf("plan module initialization dependencies: %w", err)
		}
		plan.Composition = composition
	}
	sort.Slice(plan.Bindings, func(i, j int) bool {
		a, b := plan.Bindings[i].Key, plan.Bindings[j].Key
		if a.ConsumerModuleID != b.ConsumerModuleID {
			return a.ConsumerModuleID < b.ConsumerModuleID
		}
		return a.CapabilityName < b.CapabilityName
	})
	sort.Slice(plan.Blockers, func(i, j int) bool {
		return plan.Blockers[i].Key.ConsumerModuleID < plan.Blockers[j].Key.ConsumerModuleID
	})
	sort.Slice(plan.Warnings, func(i, j int) bool {
		return plan.Warnings[i].Key.ConsumerModuleID < plan.Warnings[j].Key.ConsumerModuleID
	})
	return plan, nil
}
