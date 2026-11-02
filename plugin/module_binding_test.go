package plugin

import (
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestResolveModulesRequiresExplicitSelectionAndEnforcesScope(t *testing.T) {
	contractRef := contract.SchemaRef{ID: "billing.invoice", Version: 1}
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contractRef, Required: true}}}
	provider := func(id string) ProviderRegistration {
		return ProviderRegistration{TenantID: "tenant-a", ProviderID: id, OwnerModuleID: "billing.ar", Kind: ProviderInternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contractRef}}, Factories: map[contract.CapabilityName]ProviderFactory{"billing.issue": NewStaticFactory("billing.issue", nil)}}
	}
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p2"), provider("p1")}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "provider_selection_required" {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	key := BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}
	plan, err = ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p2"), provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p2"}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].ProviderID != "p2" || plan.Bindings[0].Revision != 7 {
		t.Fatalf("selection not pinned: %#v", plan)
	}
	key.Scope.TenantID = "tenant-b"
	if _, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p1"}}, 7); err == nil {
		t.Fatal("cross-tenant selection accepted")
	}
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
