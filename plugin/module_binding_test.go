package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
)

func TestResolveModulesRequiresExplicitSelectionAndEnforcesScope(t *testing.T) {
	contractRef := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contractRef, Required: true}}}
	providerModule := contract.ModuleDescriptor{ID: "billing.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contractRef}}}
	provider := func(id string) ProviderRegistration {
		return ProviderRegistration{TenantID: "tenant-a", ProviderID: id, OwnerModuleID: "billing.ar", Kind: ProviderInternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contractRef}}, Factories: map[contract.CapabilityName]ProviderFactory{"billing.issue": NewStaticFactory("billing.issue", nil)}}
	}
	modules := []contract.ModuleDescriptor{consumer, providerModule}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan, err := ResolveModules(scope, modules, []ProviderRegistration{provider("p2"), provider("p1")}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "provider_selection_required" {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	key := BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}
	plan, err = ResolveModules(scope, modules, []ProviderRegistration{provider("p2"), provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p2"}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].ProviderID != "p2" || plan.Bindings[0].Revision != 7 {
		t.Fatalf("selection not pinned: %#v", plan)
	}
	key.Scope.TenantID = "tenant-b"
	if _, err := ResolveModules(scope, modules, []ProviderRegistration{provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p1"}}, 7); err == nil {
		t.Fatal("cross-tenant selection accepted")
	}
}

func TestResolveModulesRejectsDuplicateAndUnmatchedProviderSelections(t *testing.T) {
	contractRef := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contractRef, Required: true}}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	key := BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "billing", OwnerModuleID: "billing.ar", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contractRef}}, Factories: map[contract.CapabilityName]ProviderFactory{"billing.issue": NewStaticFactory("billing.issue", nil)}}
	if _, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider}, []ProviderSelection{{Key: key, ProviderID: "billing"}, {Key: key, ProviderID: "billing"}}, 1); err == nil {
		t.Fatal("duplicate provider selections were accepted")
	}
	if _, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider, provider}, []ProviderSelection{{Key: key, ProviderID: "billing"}}, 1); err == nil {
		t.Fatal("duplicate provider registration was accepted")
	}
	unknown := key
	unknown.CapabilityName = "billing.refund"
	if _, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider}, []ProviderSelection{{Key: unknown, ProviderID: "billing"}}, 1); err == nil {
		t.Fatal("provider selection for an undeclared capability was accepted")
	}
}

func TestResolveInstalledModulesHashesDeterministicPackageComposition(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	modules := []contract.ModuleDescriptor{{ID: "sales.orders", Version: "1.0.0"}}
	base := contract.InstalledPackage{PackageID: "base", ReleaseID: "base-1", Version: "1.0.0", Digest: "base-digest"}
	consumer := contract.InstalledPackage{PackageID: "sales", ReleaseID: "sales-1", Version: "1.0.0", Digest: "sales-digest", Requires: []contract.ManifestPackRef{{PackageID: "base", ReleaseID: "base-1", Version: "1.0.0"}}}
	first, err := ResolveInstalledModules(scope, []contract.InstalledPackage{consumer, base}, modules, nil, nil, 5)
	if err != nil {
		t.Fatal("resolve installed package composition:", err)
	}
	second, err := ResolveInstalledModules(scope, []contract.InstalledPackage{base, consumer}, modules, nil, nil, 5)
	if err != nil || first.Composition.PlanHash != second.Composition.PlanHash {
		t.Fatalf("package input order changed composition hash: %q / %q, %v", first.Composition.PlanHash, second.Composition.PlanHash, err)
	}
	if len(first.Packages) != 2 || first.Packages[0].ReleaseID != "base-1" {
		t.Fatalf("resolved package constraints are not retained deterministically: %#v", first.Packages)
	}
	consumer.Digest = "changed-digest"
	changed, err := ResolveInstalledModules(scope, []contract.InstalledPackage{base, consumer}, modules, nil, nil, 5)
	if err != nil || changed.Composition.PlanHash == first.Composition.PlanHash {
		t.Fatalf("composition hash did not bind immutable package digest: %q / %q, %v", first.Composition.PlanHash, changed.Composition.PlanHash, err)
	}
}

func TestResolveModulesRejectsDeterministicInitializationCycle(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	capabilityA := contract.CapabilityOffer{Name: "billing.post", Contract: contract.SchemaRef{ID: "journal", Version: 1}}
	capabilityB := contract.CapabilityOffer{Name: "inventory.reserve", Contract: contract.SchemaRef{ID: "reservation", Version: 1}}
	accounting := contract.ModuleDescriptor{ID: "accounting.ledger", Version: "1.0.0", Provides: []contract.CapabilityOffer{capabilityA}, Requires: []contract.CapabilityRequirement{{Name: capabilityB.Name, Contract: capabilityB.Contract, Required: true}}}
	inventory := contract.ModuleDescriptor{ID: "inventory.stock", Version: "1.0.0", Provides: []contract.CapabilityOffer{capabilityB}, Requires: []contract.CapabilityRequirement{{Name: capabilityA.Name, Contract: capabilityA.Contract, Required: true}}}
	providers := []ProviderRegistration{
		{TenantID: scope.TenantID, ProviderID: "accounting", OwnerModuleID: accounting.ID, Kind: ProviderInternal, Offers: []contract.CapabilityOffer{capabilityA}, Factories: map[contract.CapabilityName]ProviderFactory{capabilityA.Name: NewStaticFactory(capabilityA.Name, nil)}},
		{TenantID: scope.TenantID, ProviderID: "inventory", OwnerModuleID: inventory.ID, Kind: ProviderInternal, Offers: []contract.CapabilityOffer{capabilityB}, Factories: map[contract.CapabilityName]ProviderFactory{capabilityB.Name: NewStaticFactory(capabilityB.Name, nil)}},
	}
	_, firstErr := ResolveModules(scope, []contract.ModuleDescriptor{accounting, inventory}, providers, nil, 3)
	_, secondErr := ResolveModules(scope, []contract.ModuleDescriptor{inventory, accounting}, []ProviderRegistration{providers[1], providers[0]}, nil, 3)
	if firstErr == nil || secondErr == nil || firstErr.Error() != secondErr.Error() {
		t.Fatalf("initialization cycle diagnostics are not stable: %v / %v", firstErr, secondErr)
	}
}

func TestResolvedPlanExportsUniquePublishedQueryProviderSelection(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	inputSchema := contract.SchemaRef{ID: "ar.balance.input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "ar.balance.output", Version: 1}
	operation := contract.OperationSpec{Name: "list", Effect: contract.EffectQuery, Input: inputSchema, Output: outputSchema}
	query := contract.PublishedQuery{OwnerModuleID: "finance.ar", Name: "balances", Capability: "ar.balance", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 10}
	module := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: "ar.balance", Contract: outputSchema, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "ar-internal", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: testPublishedQueryExecutor{}}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, nil, 5)
	if err != nil {
		t.Fatal("resolve unique provider:", err)
	}
	selections, err := plan.ProviderSelections()
	if err != nil || len(selections) != 1 || selections[0].ProviderID != "ar-internal" || selections[0].Key.CapabilityName != "ar.balance" {
		t.Fatalf("resolved provider selections = %#v, %v", selections, err)
	}
}

func TestResolveModulesIncludesOwnerRegisteredBusinessReconciliationRules(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "recon.input", Version: 1}, contract.SchemaRef{ID: "recon.output", Version: 1}
	queryModule := func(id, queryName, capability string, fields []string) (contract.ModuleDescriptor, ProviderRegistration) {
		operation := contract.OperationSpec{Name: "list", Effect: contract.EffectQuery, Input: input, Output: output}
		offer := contract.CapabilityOffer{Name: contract.CapabilityName(capability), Contract: output, Operations: []contract.OperationSpec{operation}}
		query := contract.PublishedQuery{OwnerModuleID: contract.ModuleID(id), Name: queryName, Capability: offer.Name, Operation: operation.Name, InputSchema: input, OutputSchema: output, ScopePolicy: contract.ScopeTenantShared, AllowedFields: fields, PageLimit: 20}
		module := contract.ModuleDescriptor{ID: contract.ModuleID(id), Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}, Queries: []contract.PublishedQuery{query}}
		provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: id + "-provider", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: []contract.CapabilityOffer{offer}, Queries: map[string]PublishedQueryExecutor{queryName: testPublishedQueryExecutor{}}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
		return module, provider
	}
	source, sourceProvider := queryModule("sales.orders", "billable", "sales.reconcile", []string{"key", "source_ref", "entity_id", "amount", "currency", "occurred_at"})
	target, targetProvider := queryModule("accounts.receivable", "invoices", "ar.reconcile", []string{"key", "target_ref", "entity_id", "amount", "currency", "occurred_at"})
	repairOffer := contract.CapabilityOffer{Name: "ar.repair", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectLocal}}}
	target.Provides = append(target.Provides, repairOffer)
	targetProvider.Offers = append(targetProvider.Offers, repairOffer)
	targetProvider.Factories = map[contract.CapabilityName]ProviderFactory{"ar.repair": NewStaticFactory("ar.repair", nil)}
	owner := contract.ModuleDescriptor{ID: "finance.reconciliation", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "ar.repair", Contract: output, Required: true}}}
	ownerProvider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "recon-owner", OwnerModuleID: owner.ID, Kind: ProviderInternal, BusinessReconciliationRules: []BusinessReconciliationRule{{
		OwnerModuleID: owner.ID, ID: "sales-ar", Version: 1,
		SourceQueryOwnerModuleID: source.ID, SourceQueryName: "billable", SourceQueryInput: json.RawMessage(`{}`), SourcePageLimit: 10,
		TargetQueryOwnerModuleID: target.ID, TargetQueryName: "invoices", TargetQueryInput: json.RawMessage(`{}`), TargetPageLimit: 10,
		MaxRows: 100, RepairCallerModuleID: owner.ID, RepairCapability: "ar.repair", RepairOperation: "issue", RepairLeaseID: "lease-repair",
	}}}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{source, target, owner}, []ProviderRegistration{targetProvider, ownerProvider, sourceProvider}, nil, 4)
	if err != nil {
		t.Fatal("resolve owner reconciliation rule:", err)
	}
	if len(plan.BusinessReconciliationRules) != 1 || plan.BusinessReconciliationRules[0].OwnerModuleID != owner.ID || plan.BusinessReconciliationRules[0].TargetQueryName != "invoices" {
		t.Fatalf("owner reconciliation rules in plan = %+v", plan.BusinessReconciliationRules)
	}
	ownerProvider.BusinessReconciliationRules[0].SourceQueryName = "private_rows"
	if _, err := ResolveModules(scope, []contract.ModuleDescriptor{source, target, owner}, []ProviderRegistration{sourceProvider, targetProvider, ownerProvider}, nil, 4); err == nil {
		t.Fatal("reconciliation rule referencing an unpublished query was accepted")
	}
}

func TestResolveModulesCanShareProviderRegistrationsAcrossGenerations(t *testing.T) {
	schema := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: schema, Required: true}}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	provider := func(id string) ProviderRegistration {
		return ProviderRegistration{TenantID: scope.TenantID, ProviderID: id, OwnerModuleID: "billing.external", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: schema}}, Factories: map[contract.CapabilityName]ProviderFactory{"billing.issue": NewStaticFactory("billing.issue", nil)}}
	}
	providers := []ProviderRegistration{provider("provider-b"), provider("provider-a")}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}, ProviderID: "provider-b"}
	var workers sync.WaitGroup
	for i := 0; i < 24; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, providers, []ProviderSelection{selection}, 12)
			if err != nil || len(plan.Bindings) != 1 || plan.Bindings[0].ProviderID != "provider-b" {
				t.Errorf("resolve shared provider registrations = %#v, %v", plan, err)
			}
		}()
	}
	workers.Wait()
	if providers[0].ProviderID != "provider-b" || providers[1].ProviderID != "provider-a" {
		t.Fatalf("resolver mutated caller provider order: %#v", providers)
	}
}

func TestProviderOutageBlocksOnlyBoundCapabilityAtExecution(t *testing.T) {
	schema := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	offer := contract.CapabilityOffer{Name: "billing.issue", Contract: schema}
	providerModule := contract.ModuleDescriptor{ID: "billing.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: schema, Required: true}}}
	unrelated := contract.ModuleDescriptor{ID: "inventory.stock", Version: "1.0.0"}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: offer.Name}, ProviderID: "billing-internal"}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: selection.ProviderID, OwnerModuleID: providerModule.ID, Kind: ProviderInternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{offer.Name: NewStaticFactory(offer.Name, errors.New("provider offline"))}}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, providerModule, unrelated}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 2)
	if err != nil || len(plan.Blockers) != 0 {
		t.Fatalf("provider outage should not prevent unrelated module activation: %#v, %v", plan, err)
	}
	bound, ok := plan.Resolve(selection.Key)
	if !ok {
		t.Fatal("consumer binding disappeared during provider outage")
	}
	if _, err := bound.Open(contract.OperationContext{Site: string(scope.TenantID), Scope: scope}); err == nil {
		t.Fatal("provider outage did not block the bound capability operation")
	}
}

func TestFreshModuleCompositionModeMatrix(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-mode-matrix", Kind: contract.ScopeKindTenant}
	schema := contract.SchemaRef{ID: "fixture.billing.contract", Version: 1}
	capability := contract.CapabilityName("billing.issue")
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: capability, Contract: schema, Required: true}}}
	optionalConsumer := consumer
	optionalConsumer.ID = "sales.optional"
	optionalConsumer.Requires = []contract.CapabilityRequirement{{Name: capability, Contract: schema, Required: false}}
	internalOwner := contract.ModuleDescriptor{ID: "finance.internal", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: capability, Contract: schema}}}
	externalOwner := contract.ModuleDescriptor{ID: "finance.external", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: capability, Contract: schema}}}
	provider := func(id string, owner contract.ModuleDescriptor, kind ProviderKind, factory ProviderFactory) ProviderRegistration {
		factories := map[contract.CapabilityName]ProviderFactory{}
		if factory != nil {
			factories[capability] = factory
		}
		return ProviderRegistration{TenantID: scope.TenantID, ProviderID: id, OwnerModuleID: owner.ID, Kind: kind, Offers: owner.Provides, Factories: factories}
	}
	t.Run("absent optional capability", func(t *testing.T) {
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{optionalConsumer}, nil, nil, 1)
		if err != nil || len(plan.Blockers) != 0 || len(plan.Warnings) != 1 || plan.Warnings[0].Code != "provider_missing_or_contract_mismatch" {
			t.Fatalf("optional capability absence = plan %+v, err %v", plan, err)
		}
	})
	t.Run("absent mandatory capability", func(t *testing.T) {
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, nil, nil, 1)
		if err != nil || len(plan.Blockers) != 1 || plan.Blockers[0].Code != "provider_missing_or_contract_mismatch" {
			t.Fatalf("mandatory capability absence = plan %+v, err %v", plan, err)
		}
	})
	for _, mode := range []struct {
		name  string
		owner contract.ModuleDescriptor
		kind  ProviderKind
	}{
		{name: "internal provider", owner: internalOwner, kind: ProviderInternal},
		{name: "external provider", owner: externalOwner, kind: ProviderExternal},
	} {
		t.Run(mode.name, func(t *testing.T) {
			providerID := "fixture-" + string(mode.kind)
			registered := provider(providerID, mode.owner, mode.kind, NewStaticFactory(capability, nil))
			selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: capability}, ProviderID: providerID}
			plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, mode.owner}, []ProviderRegistration{registered}, []ProviderSelection{selection}, 2)
			if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 1 || plan.Bindings[0].Kind != mode.kind || plan.Bindings[0].ProviderID != providerID {
				t.Fatalf("%s composition = plan %+v, err %v", mode.name, plan, err)
			}
		})
	}
	t.Run("provider outage", func(t *testing.T) {
		providerID := "fixture-outage"
		registered := provider(providerID, internalOwner, ProviderInternal, NewStaticFactory(capability, errors.New("provider offline")))
		selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: capability}, ProviderID: providerID}
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, internalOwner}, []ProviderRegistration{registered}, []ProviderSelection{selection}, 3)
		if err != nil || len(plan.Blockers) != 0 {
			t.Fatalf("outage blocked unrelated composition: plan %+v, err %v", plan, err)
		}
		binding, ok := plan.Resolve(selection.Key)
		if !ok {
			t.Fatal("outage removed the binding diagnostic")
		}
		if _, err := binding.Open(contract.OperationContext{Site: string(scope.TenantID), Scope: scope}); err == nil {
			t.Fatal("provider outage allowed capability use")
		}
	})
	t.Run("provider cutover", func(t *testing.T) {
		providerA := provider("provider-a", internalOwner, ProviderInternal, NewStaticFactory(capability, nil))
		providerB := provider("provider-b", externalOwner, ProviderExternal, NewStaticFactory(capability, nil))
		selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: capability}, ProviderID: "provider-b"}
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, internalOwner, externalOwner}, []ProviderRegistration{providerA, providerB}, []ProviderSelection{selection}, 4)
		if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 1 || plan.Bindings[0].ProviderID != "provider-b" {
			t.Fatalf("provider cutover composition = plan %+v, err %v", plan, err)
		}
	})
	t.Run("module removal", func(t *testing.T) {
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{internalOwner}, []ProviderRegistration{provider("provider-a", internalOwner, ProviderInternal, NewStaticFactory(capability, nil))}, nil, 5)
		if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 0 {
			t.Fatalf("removed consumer module remained in the plan: %+v, err %v", plan, err)
		}
	})
}

type testPublishedQueryExecutor struct{ result contract.QueryResult }

func (e testPublishedQueryExecutor) ExecutePublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ json.RawMessage, _ string, _ uint16) (contract.QueryResult, error) {
	return e.result, nil
}

type actorScopedTestQueryExecutor struct {
	result contract.QueryResult
	ignore bool
}

func (e actorScopedTestQueryExecutor) ExecutePublishedQuery(ctx context.Context, execution contract.ModuleExecution, query contract.PublishedQuery, input json.RawMessage, cursor string, limit uint16) (contract.QueryResult, error) {
	return e.result, nil
}

func (e actorScopedTestQueryExecutor) ExecutePublishedQueryForActor(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, actor PublishedQueryActorScope, _ json.RawMessage, _ string, limit uint16) (contract.QueryResult, error) {
	if e.ignore {
		return e.result, nil
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(e.result.Data, &rows); err != nil {
		return contract.QueryResult{}, err
	}
	visible := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if actor.OwnerOnly {
			var subject string
			if json.Unmarshal(row[actor.OwnerField], &subject) == nil && subject == actor.SubjectID {
				visible = append(visible, row)
				continue
			}
		}
		for _, field := range actor.AssignedFields {
			var subject string
			if json.Unmarshal(row[field], &subject) == nil && subject == actor.SubjectID {
				visible = append(visible, row)
				break
			}
		}
	}
	if len(visible) > int(limit) {
		visible = visible[:limit]
	}
	data, err := json.Marshal(visible)
	e.result.Data = data
	return e.result, err
}

func TestPublishedQueryAppliesActorAssignmentBeforePageLimit(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	in, out := contract.SchemaRef{ID: "task.input", Version: 1}, contract.SchemaRef{ID: "task.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "assigned", Capability: "sales.tasks", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"assigned_to", "id", "owner_id"}, PageLimit: 1}
	operation := contract.OperationSpec{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}
	module := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
	result := contract.QueryResult{Data: json.RawMessage(`[{"id":"other","assigned_to":"bob","owner_id":"bob"},{"id":"mine","assigned_to":"alice","owner_id":"alice"}]`), SnapshotToken: "snapshot-1", SourceRevision: "r1", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sales-provider", OwnerModuleID: module.ID, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: actorScopedTestQueryExecutor{result: result}}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
		if !json.Valid(data) {
			return errors.New("invalid json")
		}
		return nil
	}}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 1)
	if err != nil {
		t.Fatal("resolve scoped query:", err)
	}
	actor := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", AssignedFields: []string{"assigned_to"}}
	got, err := plan.ExecutePublishedQueryForActor(context.Background(), scope, module.ID, query.Name, actor, json.RawMessage(`{}`), "", 1)
	if err != nil || string(got.Data) != `[{"assigned_to":"alice","id":"mine","owner_id":"alice"}]` {
		t.Fatalf("actor-scoped page = %s, error=%v", got.Data, err)
	}
	query.OwnerField = "owner_id"
	module.Queries[0] = query
	provider.Queries[query.Name] = actorScopedTestQueryExecutor{result: result}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 2)
	if err != nil {
		t.Fatal("resolve owner-scoped query:", err)
	}
	ownerScope := PublishedQueryActorScope{BusinessScope: scope, SubjectID: "alice", OwnerOnly: true, OwnerField: query.OwnerField}
	got, err = plan.ExecutePublishedQueryForActor(context.Background(), scope, module.ID, query.Name, ownerScope, json.RawMessage(`{}`), "", 1)
	if err != nil || string(got.Data) != `[{"assigned_to":"alice","id":"mine","owner_id":"alice"}]` {
		t.Fatalf("owner-scoped page = %s, error=%v", got.Data, err)
	}
	provider.Queries[query.Name] = actorScopedTestQueryExecutor{result: result, ignore: true}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 3)
	if err != nil {
		t.Fatal("resolve misbehaving scoped query:", err)
	}
	if _, err := plan.ExecutePublishedQueryForActor(context.Background(), scope, module.ID, query.Name, actor, json.RawMessage(`{}`), "", 1); err == nil {
		t.Fatal("provider returned a row outside the verified actor assignment")
	}
}

type testProjectionStore struct {
	snapshot analytics.PublishedProjectionSnapshot
	saved    bool
	stale    bool
}

func (s *testProjectionStore) Save(_ context.Context, snapshot analytics.PublishedProjectionSnapshot) error {
	s.snapshot, s.saved = snapshot, true
	return nil
}

func (s *testProjectionStore) MarkStale(_ context.Context, _ contract.ProjectionDefinition, _ contract.BusinessScope, _ string) error {
	s.stale = true
	return nil
}

func TestPublishedQueryUsesSameBoundedContractForInternalAndExternalProviders(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	inputSchema, outputSchema := contract.SchemaRef{ID: "report.input", Version: 1}, contract.SchemaRef{ID: "report.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "balances", Capability: "sales.report", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"amount", "id"}, PageLimit: 10}
	queryOperation := contract.OperationSpec{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
	module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{queryOperation}}}, Queries: []contract.PublishedQuery{query}}
	result := contract.QueryResult{Data: json.RawMessage(`[{"amount":12,"id":"fact-1"}]`), SourceRevision: "rev-4", AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}
	for _, kind := range []ProviderKind{ProviderInternal, ProviderExternal} {
		provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: string(kind) + "-provider", OwnerModuleID: module.ID, Kind: kind, Offers: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{queryOperation}}}, Queries: map[string]PublishedQueryExecutor{"balances": testPublishedQueryExecutor{result}}, ValidateSchema: func(_ contract.SchemaRef, b []byte) error {
			if !json.Valid(b) {
				return errors.New("invalid JSON")
			}
			return nil
		}}
		selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: module.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
		plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatalf("%s resolve: %v", kind, err)
		}
		got, err := plan.ExecutePublishedQuery(context.Background(), scope, module.ID, query.Name, json.RawMessage(`{}`), "", 5)
		if err != nil || string(got.Data) != string(result.Data) {
			t.Fatalf("%s query = %#v, %v", kind, got, err)
		}
		if _, err := plan.ExecutePublishedQuery(context.Background(), scope, module.ID, query.Name, json.RawMessage(`{}`), "", 11); err == nil {
			t.Fatalf("%s query exceeded page limit", kind)
		}
		bad := testPublishedQueryExecutor{contract.QueryResult{Data: json.RawMessage(`[{"secret":true}]`), AsOf: time.Now().UTC(), Freshness: contract.QueryFresh}}
		provider.Queries[query.Name] = bad
		plan, err = ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = plan.ExecutePublishedQuery(context.Background(), scope, module.ID, query.Name, json.RawMessage(`{}`), "", 5); err == nil {
			t.Fatalf("%s provider leaked an undeclared field", kind)
		}
		provider.Offers = nil
		unoffered, err := ResolveModules(scope, []contract.ModuleDescriptor{module}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unoffered.ExecutePublishedQuery(context.Background(), scope, module.ID, query.Name, json.RawMessage(`{}`), "", 5); err == nil {
			t.Fatalf("%s query executor without the capability offer was accepted", kind)
		}
	}
}

func TestPublishedProjectionRebuildPinsSourceRevisionAndPreservesOnOutage(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	inputSchema, outputSchema := contract.SchemaRef{ID: "report.input", Version: 1}, contract.SchemaRef{ID: "report.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "balances", Capability: "sales.report", Operation: "list", InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"amount", "id", "internal_note"}, PageLimit: 2}
	queryOperation := contract.OperationSpec{Name: query.Operation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
	projection := contract.ProjectionDefinition{ID: "sales_balance", OwnerModuleID: "reports.ar", Sources: []contract.ProjectionSource{{OwnerModuleID: query.OwnerModuleID, QueryName: query.Name, OutputSchema: outputSchema}}, ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"id", "amount"}, OutputSchema: outputSchema, RebuildStrategy: "consistent_snapshot"}
	source := contract.ModuleDescriptor{ID: query.OwnerModuleID, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{queryOperation}}}, Queries: []contract.PublishedQuery{query}}
	owner := contract.ModuleDescriptor{ID: projection.OwnerModuleID, Version: "1.0.0", Projections: []contract.ProjectionDefinition{projection}}
	asOf := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	concurrentSourceRevision := "source-r7"
	pageExecutor := projectionPageExecutor{pages: map[string]contract.QueryResult{
		"":     {Data: json.RawMessage(`[{"amount":12,"id":"fact-1","internal_note":"hidden"}]`), NextCursor: "next", SnapshotToken: "snapshot-r7", SourceCursor: "log-7", SourceRevision: "source-r7", AsOf: asOf, Freshness: contract.QueryFresh},
		"next": {Data: json.RawMessage(`[{"amount":99,"id":"fact-1","internal_note":"hidden"},{"amount":9,"id":"fact-2","internal_note":"hidden"}]`), SnapshotToken: "snapshot-r7", SourceCursor: "log-7", SourceRevision: "source-r7", AsOf: asOf, Freshness: contract.QueryFresh},
	}, replay: map[string]contract.QueryReplayPage{
		"":            {Changes: []contract.QueryReplayChange{{FactKey: "fact-1", Deleted: true}, {FactKey: "fact-3", Data: json.RawMessage(`{"id":"fact-3","amount":4}`)}}, NextCursor: "replay-next", ThroughCursor: "log-8", SourceRevision: "source-r8", AsOf: asOf.Add(time.Minute), Freshness: contract.QueryFresh},
		"replay-next": {Changes: []contract.QueryReplayChange{{FactKey: "fact-3", Data: json.RawMessage(`{"id":"fact-3","amount":6}`)}}, ThroughCursor: "log-8", SourceRevision: "source-r8", AsOf: asOf.Add(time.Minute), Freshness: contract.QueryFresh},
	}, afterFirstPage: func() { concurrentSourceRevision = "source-r8" }}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "sales-internal", OwnerModuleID: source.ID, Kind: ProviderInternal, Offers: source.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: pageExecutor}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
		if !json.Valid(data) {
			return errors.New("invalid JSON")
		}
		return nil
	}}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: source.ID, CapabilityName: query.Capability}, ProviderID: provider.ProviderID}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{source, owner}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 9)
	if err != nil {
		t.Fatal("resolve plan:", err)
	}
	store := &testProjectionStore{}
	if err := plan.RebuildPublishedProjection(context.Background(), store, scope, owner.ID, projection.ID, nil); err != nil {
		t.Fatal("rebuild projection:", err)
	}
	if !store.saved || store.stale || string(store.snapshot.Data) != `[{"amount":9,"id":"fact-2"},{"amount":6,"id":"fact-3"}]` || store.snapshot.SourceRevision == "source-r7" || !store.snapshot.AsOf.Equal(asOf.Add(time.Minute)) || !strings.Contains(store.snapshot.Cursor, "log-8") || concurrentSourceRevision != "source-r8" {
		t.Fatalf("stored projection snapshot = %#v, saved=%v stale=%v", store.snapshot, store.saved, store.stale)
	}
	provider.Queries[query.Name] = testPublishedQueryExecutor{contract.QueryResult{Data: json.RawMessage(`[]`), SnapshotToken: "snapshot-r8", SourceCursor: "log-8", SourceRevision: "source-r8", AsOf: asOf, Freshness: contract.QueryFresh}}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{source, owner}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 10)
	if err != nil {
		t.Fatal("resolve provider without replay:", err)
	}
	store.saved, store.stale = false, false
	if err := plan.RebuildPublishedProjection(context.Background(), store, scope, owner.ID, projection.ID, nil); err == nil || store.saved || !store.stale {
		t.Fatalf("provider without replay support was not failed closed: err=%v saved=%v stale=%v", err, store.saved, store.stale)
	}
	provider.Queries[query.Name] = projectionPageExecutor{pages: map[string]contract.QueryResult{
		"":     {Data: json.RawMessage(`[{"amount":13,"id":"fact-3"}]`), NextCursor: "next", SnapshotToken: "snapshot-r8", SourceRevision: "source-r8", AsOf: asOf, Freshness: contract.QueryFresh},
		"next": {Data: json.RawMessage(`[{"amount":10,"id":"fact-4"}]`), SnapshotToken: "snapshot-r8", SourceRevision: "source-r9", AsOf: asOf, Freshness: contract.QueryFresh},
	}}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{source, owner}, []ProviderRegistration{provider}, []ProviderSelection{selection}, 10)
	if err != nil {
		t.Fatal("resolve changed plan:", err)
	}
	store.saved, store.stale = false, false
	if err := plan.RebuildPublishedProjection(context.Background(), store, scope, owner.ID, projection.ID, nil); err == nil {
		t.Fatal("mixed source revisions were accepted")
	}
	if store.saved || !store.stale {
		t.Fatalf("failed rebuild did not retain and mark stale prior snapshot: saved=%v stale=%v", store.saved, store.stale)
	}
}

func TestPublishedProjectionRejectsUncoordinatedSourceSnapshots(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	inputSchema, outputSchema := contract.SchemaRef{ID: "report.input", Version: 1}, contract.SchemaRef{ID: "report.output", Version: 1}
	operation := contract.OperationSpec{Name: "list", Input: inputSchema, Output: outputSchema, Effect: contract.EffectQuery}
	providers := make([]ProviderRegistration, 0, 2)
	modules := make([]contract.ModuleDescriptor, 0, 3)
	selections := make([]ProviderSelection, 0, 2)
	sources := make([]contract.ProjectionSource, 0, 2)
	asOf := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for i, id := range []contract.ModuleID{"sales.orders", "inventory.items"} {
		query := contract.PublishedQuery{OwnerModuleID: id, Name: "facts", Capability: contract.CapabilityName(string(id) + ".report"), Operation: operation.Name, InputSchema: inputSchema, OutputSchema: outputSchema, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"id", "amount"}, PageLimit: 10}
		module := contract.ModuleDescriptor{ID: id, Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: outputSchema, Operations: []contract.OperationSpec{operation}}}, Queries: []contract.PublishedQuery{query}}
		providerID := "provider-" + string(id)
		token := "snapshot-" + string(id)
		provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: providerID, OwnerModuleID: id, Kind: ProviderInternal, Offers: module.Provides, Queries: map[string]PublishedQueryExecutor{query.Name: testPublishedQueryExecutor{contract.QueryResult{Data: json.RawMessage(`[ {"id":"fact","amount":1} ]`), SnapshotToken: token, SourceRevision: "revision-1", AsOf: asOf, Freshness: contract.QueryFresh}}}, ValidateSchema: func(_ contract.SchemaRef, data []byte) error {
			if !json.Valid(data) {
				return errors.New("invalid JSON")
			}
			return nil
		}}
		providers = append(providers, provider)
		modules = append(modules, module)
		selections = append(selections, ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: id, CapabilityName: query.Capability}, ProviderID: providerID})
		sources = append(sources, contract.ProjectionSource{OwnerModuleID: id, QueryName: query.Name, OutputSchema: outputSchema})
		if i == 1 {
			continue
		}
	}
	projection := contract.ProjectionDefinition{ID: "combined", OwnerModuleID: "reports.finance", Sources: sources, ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"id", "amount"}, OutputSchema: outputSchema, RebuildStrategy: "consistent_snapshot"}
	modules = append(modules, contract.ModuleDescriptor{ID: projection.OwnerModuleID, Version: "1.0.0", Projections: []contract.ProjectionDefinition{projection}})
	plan, err := ResolveModules(scope, modules, providers, selections, 1)
	if err != nil {
		t.Fatal("resolve providers:", err)
	}
	store := &testProjectionStore{}
	if err := plan.RebuildPublishedProjection(context.Background(), store, scope, projection.OwnerModuleID, projection.ID, nil); err == nil {
		t.Fatal("cross-source rebuild accepted inconsistent global snapshot tokens")
	}
	if store.saved || !store.stale {
		t.Fatalf("inconsistent snapshot was published or not marked stale: saved=%v stale=%v", store.saved, store.stale)
	}
}

func TestPublishedQueryProviderOutageRetainsProjectionDefinitionForHistory(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	in, out := contract.SchemaRef{ID: "query.input", Version: 1}, contract.SchemaRef{ID: "query.output", Version: 1}
	query := contract.PublishedQuery{OwnerModuleID: "sales.orders", Name: "balances", Capability: "sales.report", Operation: "list", InputSchema: in, OutputSchema: out, ScopePolicy: contract.ScopeEntityOwned, AllowedFields: []string{"amount", "id"}, PageLimit: 10}
	module := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Provides: []contract.CapabilityOffer{{Name: query.Capability, Contract: out, Operations: []contract.OperationSpec{{Name: query.Operation, Input: in, Output: out, Effect: contract.EffectQuery}}}}, Queries: []contract.PublishedQuery{query}}
	projection := contract.ModuleDescriptor{ID: "sales.reporting", Version: "1.0.0", Projections: []contract.ProjectionDefinition{{ID: "sales_balance", OwnerModuleID: "sales.reporting", Sources: []contract.ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: query.Name, OutputSchema: out}}, ScopePolicy: contract.ScopeEntityOwned, FactKey: "id", AllowedFields: []string{"id", "amount"}, OutputSchema: out, RebuildStrategy: "consistent_snapshot"}}}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{module, projection}, nil, nil, 1)
	if err != nil {
		t.Fatal("provider outage prevented module plan resolution:", err)
	}
	if _, ok := plan.QueryDefinitions[PublishedQueryKey{Scope: scope, OwnerModuleID: module.ID, Name: query.Name}]; !ok {
		t.Fatal("query definition was lost during provider outage")
	}
	key := PublishedProjectionKey{OwnerModuleID: projection.ID, ID: "sales_balance"}
	if _, ok := plan.Projections[key]; !ok || plan.ProjectionScopes[key] != contract.ScopeEntityOwned {
		t.Fatal("retained projection contract or its legal-entity scope was lost during provider outage")
	}
	if len(plan.Warnings) == 0 || len(plan.Blockers) != 0 {
		t.Fatalf("provider outage diagnostics = blockers %v warnings %v", plan.Blockers, plan.Warnings)
	}
	removedSource, err := ResolveModules(scope, []contract.ModuleDescriptor{projection}, nil, nil, 2)
	if err != nil {
		t.Fatal("source removal prevented historical projection plan resolution:", err)
	}
	if _, ok := removedSource.Projections[key]; !ok || removedSource.ProjectionScopes[key] != contract.ScopeEntityOwned {
		t.Fatal("historical projection metadata did not survive source removal")
	}
}

type projectionPageExecutor struct {
	pages          map[string]contract.QueryResult
	replay         map[string]contract.QueryReplayPage
	afterFirstPage func()
}

func (e projectionPageExecutor) ReplayPublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ json.RawMessage, _ string, _ string, cursor string, _ uint16) (contract.QueryReplayPage, error) {
	page, ok := e.replay[cursor]
	if !ok {
		return contract.QueryReplayPage{}, errors.New("replay cursor unavailable")
	}
	return page, nil
}

func (e projectionPageExecutor) ExecutePublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ json.RawMessage, cursor string, _ uint16) (contract.QueryResult, error) {
	if cursor == "" && e.afterFirstPage != nil {
		e.afterFirstPage()
	}
	result, ok := e.pages[cursor]
	if !ok {
		return contract.QueryResult{}, errors.New("cursor unavailable")
	}
	return result, nil
}

func TestResolveModulesOptionalAbsentAndWrongSchema(t *testing.T) {
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: false}}}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || len(plan.Blockers) != 0 {
		t.Fatalf("optional absence not degraded locally: %#v", plan)
	}
	provider := ProviderRegistration{TenantID: "tenant-a", ProviderID: "p", OwnerModuleID: "billing.ar", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 2}}}}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 {
		t.Fatalf("wrong schema should be unavailable: %#v", plan)
	}
}

func TestResolveModulesPlanIsStableAcrossProviderOrder(t *testing.T) {
	schema := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: schema, Required: true}}}
	provider := func(id string) ProviderRegistration {
		return ProviderRegistration{TenantID: "tenant-a", ProviderID: id, OwnerModuleID: "billing.ar", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: schema}}, Factories: map[contract.CapabilityName]ProviderFactory{"billing.issue": NewStaticFactory("billing.issue", nil)}}
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}, ProviderID: "provider-b"}
	first, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("provider-a"), provider("provider-b")}, []ProviderSelection{selection}, 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("provider-b"), provider("provider-a")}, []ProviderSelection{selection}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Bindings) != 1 || len(second.Bindings) != 1 || first.Bindings[0].ProviderID != second.Bindings[0].ProviderID || first.Bindings[0].ProviderID != "provider-b" {
		t.Fatalf("provider ordering changed plan: %#v %#v", first, second)
	}
}

func TestResolveModulesSelectsExternalProviderPerLegalEntity(t *testing.T) {
	schema := contract.SchemaRef{ID: "warehouse.stock", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "inventory.stock", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "warehouse.lookup", Contract: schema, Required: true}}}
	scopeA := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	scopeB := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-b"}
	provider := func(id string, scope contract.BusinessScope) ProviderRegistration {
		return ProviderRegistration{TenantID: "tenant-a", Scope: scope, ProviderID: id, OwnerModuleID: "warehouse.external", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "warehouse.lookup", Contract: schema}}, Factories: map[contract.CapabilityName]ProviderFactory{"warehouse.lookup": NewStaticFactory("warehouse.lookup", nil)}}
	}
	keyA := BindingKey{Scope: scopeA, ConsumerModuleID: consumer.ID, CapabilityName: "warehouse.lookup"}
	keyB := BindingKey{Scope: scopeB, ConsumerModuleID: consumer.ID, CapabilityName: "warehouse.lookup"}
	providers := []ProviderRegistration{provider("warehouse-a", scopeA), provider("warehouse-b", scopeB)}
	planA, err := ResolveModules(scopeA, []contract.ModuleDescriptor{consumer}, providers, []ProviderSelection{{Key: keyA, ProviderID: "warehouse-a"}}, 3)
	if err != nil || len(planA.Bindings) != 1 || planA.Bindings[0].ProviderID != "warehouse-a" {
		t.Fatalf("entity A binding = %#v, %v", planA, err)
	}
	planB, err := ResolveModules(scopeB, []contract.ModuleDescriptor{consumer}, providers, []ProviderSelection{{Key: keyB, ProviderID: "warehouse-b"}}, 3)
	if err != nil || len(planB.Bindings) != 1 || planB.Bindings[0].ProviderID != "warehouse-b" {
		t.Fatalf("entity B binding = %#v, %v", planB, err)
	}
	if _, err := ResolveModules(scopeA, []contract.ModuleDescriptor{consumer}, providers, []ProviderSelection{{Key: keyA, ProviderID: "warehouse-b"}}, 3); err == nil {
		t.Fatal("entity A selected entity B provider")
	}
	operation := contract.OperationContext{Site: "tenant-a", Scope: scopeA}
	if _, err := planA.Bindings[0].Open(operation); err != nil {
		t.Fatalf("open entity A provider under matching scope: %v", err)
	}
	operation.Scope = scopeB
	if _, err := planA.Bindings[0].Open(operation); err == nil {
		t.Fatal("entity A provider opened under entity B operation scope")
	}
}
