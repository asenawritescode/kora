package plugin

import (
	"context"
	"encoding/json"
	"errors"
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

type testPublishedQueryExecutor struct{ result contract.QueryResult }

func (e testPublishedQueryExecutor) ExecutePublishedQuery(_ context.Context, _ contract.ModuleExecution, _ contract.PublishedQuery, _ json.RawMessage, _ string, _ uint16) (contract.QueryResult, error) {
	return e.result, nil
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
		"":     {Data: json.RawMessage(`[{"amount":12,"id":"fact-1","internal_note":"hidden"}]`), NextCursor: "next", SnapshotToken: "snapshot-r7", SourceRevision: "source-r7", AsOf: asOf, Freshness: contract.QueryFresh},
		"next": {Data: json.RawMessage(`[{"amount":99,"id":"fact-1","internal_note":"hidden"},{"amount":9,"id":"fact-2","internal_note":"hidden"}]`), SnapshotToken: "snapshot-r7", SourceRevision: "source-r7", AsOf: asOf, Freshness: contract.QueryFresh},
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
	if !store.saved || store.stale || string(store.snapshot.Data) != `[{"amount":12,"id":"fact-1"},{"amount":9,"id":"fact-2"}]` || store.snapshot.SourceRevision == "source-r7" || store.snapshot.AsOf != asOf || concurrentSourceRevision != "source-r8" {
		t.Fatalf("stored projection snapshot = %#v, saved=%v stale=%v", store.snapshot, store.saved, store.stale)
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
	afterFirstPage func()
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
