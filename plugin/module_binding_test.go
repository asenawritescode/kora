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
	plan, err := ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p2"), provider("p1")}, nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != "provider_selection_required" {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	key := BindingKey{TenantID: "tenant-a", ScopeKey: "entity-a", ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}
	plan, err = ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p2"), provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p2"}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Bindings) != 1 || plan.Bindings[0].ProviderID != "p2" || plan.Bindings[0].Revision != 7 {
		t.Fatalf("selection not pinned: %#v", plan)
	}
	key.TenantID = "tenant-b"
	if _, err := ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("p1")}, []ProviderSelection{{Key: key, ProviderID: "p1"}}, 7); err == nil {
		t.Fatal("cross-tenant selection accepted")
	}
}

func TestResolveModulesOptionalAbsentAndWrongSchema(t *testing.T) {
	consumer := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: false}}}
	plan, err := ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, nil, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || len(plan.Blockers) != 0 {
		t.Fatalf("optional absence not degraded locally: %#v", plan)
	}
	provider := ProviderRegistration{TenantID: "tenant-a", ProviderID: "p", OwnerModuleID: "billing.ar", Kind: ProviderExternal, Offers: []contract.CapabilityOffer{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 2}}}}
	plan, err = ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider}, nil, 1)
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
	selection := ProviderSelection{Key: BindingKey{TenantID: "tenant-a", ScopeKey: "entity-a", ConsumerModuleID: consumer.ID, CapabilityName: "billing.issue"}, ProviderID: "provider-b"}
	first, err := ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("provider-a"), provider("provider-b")}, []ProviderSelection{selection}, 9)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveModules("tenant-a", "entity-a", []contract.ModuleDescriptor{consumer}, []ProviderRegistration{provider("provider-b"), provider("provider-a")}, []ProviderSelection{selection}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Bindings) != 1 || len(second.Bindings) != 1 || first.Bindings[0].ProviderID != second.Bindings[0].ProviderID || first.Bindings[0].ProviderID != "provider-b" {
		t.Fatalf("provider ordering changed plan: %#v %#v", first, second)
	}
}
