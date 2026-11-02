package contract

import (
	"testing"
	"time"
)

func TestScheduledJobEntityScopeRequiresTypedScopeAndGrant(t *testing.T) {
	base := ScheduledJobDefinition{Name: "entity-reconcile", Type: "reconcile", Schedule: "0 1 * * *"}
	entityScope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	base.Scope = &entityScope
	if err := ValidateScheduledJobDefinitions([]ScheduledJobDefinition{base}); err == nil {
		t.Fatal("entity-scoped scheduled work accepted without explicit grant")
	}
	base.RequiredResource = "sales.invoice"
	base.RequiredOperation = "write"
	if err := ValidateScheduledJobDefinitions([]ScheduledJobDefinition{base}); err != nil {
		t.Fatal("valid entity-scoped schedule rejected:", err)
	}
	invalidScope := entityScope
	invalidScope.LegalEntityID = ""
	base.Scope = &invalidScope
	if err := ValidateScheduledJobDefinitions([]ScheduledJobDefinition{base}); err == nil {
		t.Fatal("invalid entity scope accepted")
	}
}

func TestValidateModuleSetRejectsDuplicateOwnersAndUnknownSchemas(t *testing.T) {
	resource := OwnedResource{Ref: ResourceRef{Namespace: "tenant", Name: "invoice", Version: 1}, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}
	base := ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Resources: []OwnedResource{resource}}
	if err := ValidateModuleSet([]ModuleDescriptor{base}, nil); err != nil {
		t.Fatal(err)
	}
	other := base
	other.ID = "commerce.sales"
	if err := ValidateModuleSet([]ModuleDescriptor{base, other}, nil); err == nil {
		t.Fatal("duplicate resource owners accepted")
	}
	bad := base
	bad.Provides = []CapabilityOffer{{Name: "finance.issue", Contract: SchemaRef{ID: "invoice", Version: 1}, Operations: []OperationSpec{{Name: "Issue", Input: SchemaRef{ID: "input", Version: 1}, Output: SchemaRef{ID: "output", Version: 1}, Effect: "unknown"}}}}
	if err := bad.Validate(func(SchemaRef) bool { return true }); err == nil {
		t.Fatal("unknown effect accepted")
	}
	if err := bad.Validate(func(SchemaRef) bool { return false }); err == nil {
		t.Fatal("unknown schema accepted")
	}
}

func TestDecodeMarketplacePackageManifestIsStrictAndValidatesModules(t *testing.T) {
	valid := `{"schema_version":1,"package_id":"finance","release_id":"finance-1","version":"1.0.0","kind":"module","public":{"slug":"finance","name":"Finance","summary":"Finance"},"modules":[{"id":"finance.ar","version":"2.0.0","resources":[{"ref":{"namespace":"tenant","name":"invoice","version":1},"access":"private","mutation_mode":"commands_only","scope_kind":"tenant_shared"}]}],"engine":{"min_version":"1.0.0"}}`
	if _, err := DecodeMarketplacePackageManifest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMarketplacePackageManifest([]byte(valid[:len(valid)-1] + `,"surprise":true}`)); err == nil {
		t.Fatal("unknown top-level field accepted")
	}
	invalid := `{"schema_version":1,"package_id":"finance","release_id":"finance-1","version":"1.0.0","kind":"module","public":{"slug":"finance","name":"Finance","summary":"Finance"},"modules":[{"id":"INVALID","version":"2.0.0"}],"engine":{"min_version":"1.0.0"}}`
	if _, err := DecodeMarketplacePackageManifest([]byte(invalid)); err == nil {
		t.Fatal("invalid module identity accepted")
	}
}

type referenceResolverFixture struct {
	result ResolvedBusinessRef
	err    error
	scope  ScopeContext
}

func (r *referenceResolverFixture) Resolve(scope ScopeContext, ref BusinessRef) (ResolvedBusinessRef, error) {
	r.scope = scope
	if r.err != nil {
		return ResolvedBusinessRef{}, r.err
	}
	return r.result, nil
}

func TestBusinessReferenceProviderQualificationAndScope(t *testing.T) {
	entityScope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	ref := BusinessRef{ProviderID: "warehouse-a", ResourceType: "warehouse.location", ID: "same-id", Scope: entityScope}
	resolver := &referenceResolverFixture{result: ResolvedBusinessRef{Canonical: ref, Revision: 4, Display: "Main"}}
	resolved, err := ValidateBusinessReference(resolver, ScopeContext{Scope: entityScope}, ref, "warehouse.location")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Revision != 4 || resolver.scope.Scope != entityScope {
		t.Fatalf("scope or revision lost: %#v %#v", resolved, resolver.scope)
	}
	otherEntityRef := ref
	otherEntityRef.Scope.LegalEntityID = "entity-b"
	if _, err := ValidateBusinessReference(resolver, ScopeContext{Scope: entityScope}, otherEntityRef, "warehouse.location"); err == nil {
		t.Fatal("business reference from a different entity scope accepted")
	}
	other := ref
	other.ProviderID = "warehouse-b"
	if other == ref {
		t.Fatal("provider did not participate in identity")
	}
	if _, err := ValidateBusinessReference(resolver, ScopeContext{Scope: BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}}, ref, "account.customer"); err == nil {
		t.Fatal("wrong resource type accepted")
	}
	if _, err := ValidateBusinessReference(nil, ScopeContext{Scope: BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}}, ref, "warehouse.location"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestHistoricalSnapshotRetainsAndValidatesBusinessScope(t *testing.T) {
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	snapshot := HistoricalSnapshot{Source: BusinessRef{ProviderID: "warehouse-a", ResourceType: "warehouse.location", ID: "loc-1", Scope: scope}, BusinessScope: scope, CapturedAt: time.Now().UTC(), Schema: SchemaRef{ID: "warehouse-location", Version: 1}, Data: []byte(`{"label":"Main"}`)}
	if err := snapshot.Validate(); err != nil {
		t.Fatal("valid scoped snapshot rejected:", err)
	}
	snapshot.BusinessScope.LegalEntityID = "entity-b"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("snapshot scope detached from provider reference accepted")
	}
}

func TestBusinessEventRequiresTrustedOwnerAndExactPayloadSchema(t *testing.T) {
	def := BusinessEventDefinition{Type: "finance.invoice.issued", PayloadSchema: SchemaRef{ID: "finance.invoice-issued", Version: 1}, OwnerModuleID: "finance.ar"}
	if err := ValidateBusinessEvent(def, "finance.ar", def.PayloadSchema, def.Type); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBusinessEvent(def, "sales.orders", def.PayloadSchema, def.Type); err == nil {
		t.Fatal("non-owner published authoritative event")
	}
	if err := ValidateBusinessEvent(def, "finance.ar", SchemaRef{ID: "finance.invoice-issued", Version: 2}, def.Type); err == nil {
		t.Fatal("schema version mismatch accepted")
	}
}

func TestModuleLifecycleDrainRequiresCurrentUnblockedPlan(t *testing.T) {
	if err := ValidateModuleLifecycleTransition(ModuleActive, ModuleDraining); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModuleLifecycleTransition(ModuleActive, ModuleRemoved); err == nil {
		t.Fatal("active module skipped drain")
	}
	plan := ModuleChangePlan{PlanHash: "hash", BaseRevision: 7}
	if err := ValidateModuleChangePlan(plan, 7, nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModuleChangePlan(plan, 8, nil); err == nil {
		t.Fatal("stale plan accepted")
	}
	if err := ValidateModuleChangePlan(plan, 7, []ModuleBlocker{{Code: "workflow.unknown_outcome", Count: 1}}); err == nil {
		t.Fatal("open work blocker ignored")
	}
}

func TestOwnerControlledModuleExtensionsValidateScopeSchemaAndOrder(t *testing.T) {
	owner := ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", ExtensionPoints: []ExtensionPoint{{OwnerModuleID: "finance.ar", ID: "invoice.validation", Version: 2, Target: ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}, AllowedKinds: []string{"validation"}, InputSchema: SchemaRef{ID: "invoice", Version: 1}, OutputSchema: SchemaRef{ID: "validation", Version: 1}, ExecutionPhase: "before_commit", FailurePolicy: "reject"}}}
	extender := ModuleDescriptor{ID: "tax.vat", Version: "1.0.0", Extensions: []ExtensionRegistration{{ExtenderModuleID: "tax.vat", PointID: "finance.ar/invoice.validation", PointVersion: 2, ArtifactDigest: "digest", HandlerRef: ResourceRef{Namespace: "tenant", Name: "tax.validate", Version: 1}, Order: 10, AddedFields: []ExtensionField{{Key: "tax.vat.code", Schema: SchemaRef{ID: "tax-code", Version: 1}}}}}}
	if err := ValidateModuleExtensions([]ModuleDescriptor{owner, extender}, func(SchemaRef) bool { return true }); err != nil {
		t.Fatal(err)
	}
	bad := extender
	bad.Extensions = append([]ExtensionRegistration(nil), extender.Extensions...)
	bad.Extensions[0].PointID = "finance.ar/undeclared"
	if err := ValidateModuleExtensions([]ModuleDescriptor{owner, bad}, nil); err == nil {
		t.Fatal("undeclared extension point accepted")
	}
	bad = extender
	bad.Extensions = append([]ExtensionRegistration(nil), extender.Extensions...)
	bad.Extensions[0].AddedFields = []ExtensionField{{Key: "invoice.status", Schema: SchemaRef{ID: "tax-code", Version: 1}}}
	if err := ValidateModuleExtensions([]ModuleDescriptor{owner, bad}, nil); err == nil {
		t.Fatal("core field override accepted")
	}
}

func TestPlanCompositionResolvesTransitiveProvidersAndStableOrder(t *testing.T) {
	capA := CapabilityOffer{Name: "accounting.post", Contract: SchemaRef{ID: "journal", Version: 1}}
	capB := CapabilityOffer{Name: "inventory.reserve", Contract: SchemaRef{ID: "reservation", Version: 1}}
	modules := []ModuleDescriptor{
		{ID: "sales.order", Version: "1.0.0", Requires: []CapabilityRequirement{{Name: "inventory.reserve", Contract: capB.Contract, Required: true}}},
		{ID: "inventory.stock", Version: "1.0.0", Provides: []CapabilityOffer{capB}, Requires: []CapabilityRequirement{{Name: "accounting.post", Contract: capA.Contract, Required: true}}},
		{ID: "accounting.ledger", Version: "1.0.0", Provides: []CapabilityOffer{capA}},
	}
	providers := []ProviderRegistration{{TenantID: "tenant-a", ProviderID: "acct", OwnerModuleID: "accounting.ledger", Kind: "internal", Offers: []CapabilityOffer{capA}}, {TenantID: "tenant-a", ProviderID: "stock", OwnerModuleID: "inventory.stock", Kind: "internal", Offers: []CapabilityOffer{capB}}}
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	first, err := PlanComposition(modules, providers, nil, scope, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(modules)-1; i < j; i, j = i+1, j-1 {
		modules[i], modules[j] = modules[j], modules[i]
	}
	second, err := PlanComposition(modules, providers, nil, scope, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanHash != second.PlanHash || len(first.ActivationOrder) != 3 || first.ActivationOrder[0] != "accounting.ledger" || first.ActivationOrder[2] != "sales.order" {
		t.Fatalf("composition order/hash unstable: %#v %#v", first, second)
	}
	cycle := []ModuleDescriptor{{ID: "alpha.one", Version: "1.0.0", Requires: []CapabilityRequirement{{Name: "beta", Contract: SchemaRef{ID: "b", Version: 1}, Required: true}}}, {ID: "beta.two", Version: "1.0.0", Provides: []CapabilityOffer{{Name: "beta", Contract: SchemaRef{ID: "b", Version: 1}}, {Name: "alpha", Contract: SchemaRef{ID: "a", Version: 1}}}, Requires: []CapabilityRequirement{{Name: "alpha", Contract: SchemaRef{ID: "a", Version: 1}, Required: true}}}}
	cycleProviders := []ProviderRegistration{{TenantID: "tenant-a", ProviderID: "alpha-provider", OwnerModuleID: "alpha.one", Kind: "internal", Offers: []CapabilityOffer{{Name: "alpha", Contract: SchemaRef{ID: "a", Version: 1}}}}, {TenantID: "tenant-a", ProviderID: "beta-provider", OwnerModuleID: "beta.two", Kind: "internal", Offers: []CapabilityOffer{{Name: "beta", Contract: SchemaRef{ID: "b", Version: 1}}}}}
	if _, err := PlanComposition(cycle, cycleProviders, nil, scope, 3); err == nil {
		t.Fatal("multi-hop initialization cycle accepted")
	}
}
