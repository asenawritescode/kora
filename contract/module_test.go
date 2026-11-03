package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

func TestPublishedQueriesAndProjectionSourcesAreValidated(t *testing.T) {
	schemaIn, schemaOut := SchemaRef{ID: "query.input", Version: 1}, SchemaRef{ID: "query.output", Version: 1}
	query := PublishedQuery{OwnerModuleID: "sales.orders", Name: "totals", Capability: "sales.reporting", Operation: "list", InputSchema: schemaIn, OutputSchema: schemaOut, ScopePolicy: ScopeEntityOwned, AllowedFields: []string{"amount"}, PageLimit: 25}
	module := ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Provides: []CapabilityOffer{{Name: query.Capability, Contract: schemaOut, Operations: []OperationSpec{{Name: "list", Input: schemaIn, Output: schemaOut, Effect: EffectQuery}}}}, Queries: []PublishedQuery{query}, Projections: []ProjectionDefinition{{ID: "sales_totals", OwnerModuleID: "sales.orders", Sources: []ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "totals", OutputSchema: schemaOut}}, ScopePolicy: ScopeEntityOwned, FactKey: "amount", AllowedFields: []string{"amount"}, OutputSchema: schemaOut, RebuildStrategy: "consistent_snapshot"}}}
	registered := func(SchemaRef) bool { return true }
	if err := ValidateModuleSet([]ModuleDescriptor{module}, registered); err != nil {
		t.Fatal("valid published query rejected:", err)
	}
	bad := module
	bad.Queries = append([]PublishedQuery(nil), module.Queries...)
	bad.Queries[0].AllowedFields = []string{"amount", "amount"}
	if err := ValidateModuleSet([]ModuleDescriptor{bad}, registered); err == nil {
		t.Fatal("duplicate allowed fields accepted")
	}
	bad = module
	bad.Projections = append([]ProjectionDefinition(nil), module.Projections...)
	bad.Projections[0].Sources = []ProjectionSource{{OwnerModuleID: "sales.orders", QueryName: "totals", OutputSchema: schemaIn}}
	if err := ValidateModuleSet([]ModuleDescriptor{bad}, registered); err == nil {
		t.Fatal("projection source with an incompatible published query accepted")
	}
	bad = module
	bad.Projections = append([]ProjectionDefinition(nil), module.Projections...)
	bad.Projections[0].AllowedFields = []string{"amount", "private_note"}
	if err := ValidateModuleSet([]ModuleDescriptor{bad}, registered); err == nil {
		t.Fatal("projection exposed a field its source query does not publish")
	}
}

func TestModuleExecutionSealCoversEveryAuthorityField(t *testing.T) {
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 7, "finance.invoice.issue", "finance.invoice.issue")
	if err != nil {
		t.Fatal("create module execution:", err)
	}
	if !execution.Verified() {
		t.Fatal("new module execution is not verified")
	}
	mutations := map[string]func(*ModuleExecution){
		"tenant":             func(e *ModuleExecution) { e.TenantID = "tenant-b" },
		"caller":             func(e *ModuleExecution) { e.CallerModuleID = "attacker.module" },
		"owner":              func(e *ModuleExecution) { e.OwnerModuleID = "attacker.module" },
		"binding revision":   func(e *ModuleExecution) { e.BindingRevision++ },
		"operation":          func(e *ModuleExecution) { e.AllowedOperation = "finance.invoice.cancel" },
		"authorized command": func(e *ModuleExecution) { e.AuthorizedCommand = "finance.invoice.cancel" },
		"scope":              func(e *ModuleExecution) { e.Scope.LegalEntityID = "entity-b" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := execution
			mutate(&changed)
			if changed.Verified() {
				t.Fatal("mutated module execution retained verification")
			}
		})
	}
	if _, err := NewCommandModuleExecution(scope, "sales.orders", "finance.ar", 0, "finance.invoice.issue", "finance.invoice.issue"); err == nil {
		t.Fatal("zero binding revision minted trusted module execution")
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

func TestManifestPackageReferencesRetainPinnedRequirementsAndConflicts(t *testing.T) {
	valid := MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: "retail", ReleaseID: "retail-1.0.0", Version: "1.0.0", Kind: "template",
		Requires:  []ManifestPackRef{{PackageID: "inventory", ReleaseID: "inventory-1.0.0"}},
		Conflicts: []ManifestPackRef{{PackageID: "legacy-stock"}, {ReleaseID: "legacy-stock-1.0.0"}},
	}
	if err := ValidateManifestPackRefs(valid.Requires, valid.Conflicts); err != nil {
		t.Fatalf("valid pinned package references: %v", err)
	}
	if err := ValidateManifestPackRefs([]ManifestPackRef{{PackageID: "inventory"}}, nil); err == nil {
		t.Fatal("unversioned package requirement accepted")
	}
	if err := ValidateManifestPackRefs(nil, []ManifestPackRef{{}}); err == nil {
		t.Fatal("empty package conflict accepted")
	}
	if err := ValidateManifestPackRefs(nil, []ManifestPackRef{{ReleaseID: "../escape"}}); err == nil {
		t.Fatal("unsafe package conflict reference accepted")
	}
}

func TestValidateInstalledPackagesChecksRequirementsConflictsAndPins(t *testing.T) {
	base := InstalledPackage{PackageID: "base", ReleaseID: "base-2", Version: "2.0.0", Digest: "digest-base"}
	consumer := InstalledPackage{PackageID: "sales", ReleaseID: "sales-1", Version: "1.0.0", Digest: "digest-sales", Requires: []ManifestPackRef{{PackageID: "base", ReleaseID: "base-2", Version: "2.0.0"}}}
	if err := ValidateInstalledPackages([]InstalledPackage{consumer, base}); err != nil {
		t.Fatalf("valid pinned dependency: %v", err)
	}
	if err := ValidateInstalledPackages([]InstalledPackage{consumer}); err == nil {
		t.Fatal("missing required package accepted")
	}
	wrong := base
	wrong.Version = "1.0.0"
	if err := ValidateInstalledPackages([]InstalledPackage{consumer, wrong}); err == nil {
		t.Fatal("version-mismatched dependency accepted")
	}
	conflicting := base
	conflicting.Conflicts = []ManifestPackRef{{PackageID: "sales"}}
	if err := ValidateInstalledPackages([]InstalledPackage{consumer, conflicting}); err == nil {
		t.Fatal("declared package conflict accepted")
	}
}

func TestValidateInstalledPackagesChecksTransitiveAndReleaseConflicts(t *testing.T) {
	root := InstalledPackage{PackageID: "sales", ReleaseID: "sales-1", Version: "1.0.0", Digest: "sales-digest", Requires: []ManifestPackRef{{PackageID: "inventory", ReleaseID: "inventory-1", Version: "1.0.0"}}}
	middle := InstalledPackage{PackageID: "inventory", ReleaseID: "inventory-1", Version: "1.0.0", Digest: "inventory-digest", Requires: []ManifestPackRef{{PackageID: "warehouse", ReleaseID: "warehouse-1", Version: "1.0.0"}}}
	if err := ValidateInstalledPackages([]InstalledPackage{root, middle}); err == nil {
		t.Fatal("missing transitive package requirement accepted")
	}
	warehouse := InstalledPackage{PackageID: "warehouse", ReleaseID: "warehouse-1", Version: "1.0.0", Digest: "warehouse-digest"}
	if err := ValidateInstalledPackages([]InstalledPackage{root, middle, warehouse}); err != nil {
		t.Fatalf("complete transitive package chain rejected: %v", err)
	}
	root.Conflicts = []ManifestPackRef{{ReleaseID: "warehouse-1"}}
	if err := ValidateInstalledPackages([]InstalledPackage{root, middle, warehouse}); err == nil {
		t.Fatal("conflict pinned to an installed release accepted")
	}
}

type referenceResolverFixture struct {
	result ResolvedBusinessRef
	err    error
	scope  ScopeContext
}

func (r *referenceResolverFixture) Resolve(_ context.Context, scope ScopeContext, ref BusinessRef) (ResolvedBusinessRef, error) {
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
	resolved, err := ValidateBusinessReference(context.Background(), resolver, ScopeContext{Scope: entityScope, ProviderID: "warehouse-a"}, ref, "warehouse.location")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Revision != 4 || resolver.scope.Scope != entityScope {
		t.Fatalf("scope or revision lost: %#v %#v", resolved, resolver.scope)
	}
	otherEntityRef := ref
	otherEntityRef.Scope.LegalEntityID = "entity-b"
	if _, err := ValidateBusinessReference(context.Background(), resolver, ScopeContext{Scope: entityScope, ProviderID: "warehouse-a"}, otherEntityRef, "warehouse.location"); err == nil {
		t.Fatal("business reference from a different entity scope accepted")
	}
	other := ref
	other.ProviderID = "warehouse-b"
	if other == ref {
		t.Fatal("provider did not participate in identity")
	}
	if _, err := ValidateBusinessReference(context.Background(), resolver, ScopeContext{Scope: BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}, ProviderID: "warehouse-a"}, ref, "account.customer"); err == nil {
		t.Fatal("wrong resource type accepted")
	}
	if _, err := ValidateBusinessReference(context.Background(), nil, ScopeContext{Scope: BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}, ProviderID: "warehouse-a"}, ref, "warehouse.location"); err == nil {
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

func TestConsumerDefinitionRequiresOwnerAndPublishedSchema(t *testing.T) {
	schema := SchemaRef{ID: "finance.invoice-issued", Version: 2}
	event := BusinessEventDefinition{Type: "finance.invoice.issued", PayloadSchema: schema, OwnerModuleID: "finance.ar"}
	consumer := ConsumerDefinition{ConsumerID: "sales.invoice-copy", OwnerModuleID: "sales.orders", AcceptedEventSchemas: map[string]SchemaRef{event.Type: schema}}
	if err := ValidateConsumerDefinition(consumer, "sales.orders", map[string]BusinessEventDefinition{event.Type: event}); err != nil {
		t.Fatal(err)
	}
	consumer.OwnerModuleID = "finance.ar"
	if err := ValidateConsumerDefinition(consumer, "sales.orders", map[string]BusinessEventDefinition{event.Type: event}); err == nil {
		t.Fatal("foreign consumer owner accepted")
	}
	consumer.OwnerModuleID = "sales.orders"
	consumer.AcceptedEventSchemas[event.Type] = SchemaRef{ID: schema.ID, Version: 1}
	if err := ValidateConsumerDefinition(consumer, "sales.orders", map[string]BusinessEventDefinition{event.Type: event}); err == nil {
		t.Fatal("consumer accepted an incompatible event payload schema")
	}
}

func TestModuleLifecycleDrainRequiresCurrentUnblockedPlan(t *testing.T) {
	if err := ValidateModuleLifecycleTransition(ModuleInstalled, ModuleDisabled); err == nil {
		t.Fatal("installed module skipped activation and drain")
	}
	if err := ValidateModuleLifecycleTransition(ModuleInstalled, ModuleActive); err != nil {
		t.Fatalf("installed module could not activate: %v", err)
	}
	if err := ValidateModuleLifecycleTransition(ModuleActive, ModuleDraining); err != nil {
		t.Fatal(err)
	}
	if err := ValidateModuleLifecycleTransition(ModuleDraining, ModuleDisabled); err != nil {
		t.Fatalf("draining module could not disable: %v", err)
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
	invoice := ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}
	owner := ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Resources: []OwnedResource{{Ref: invoice, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}}, ExtensionPoints: []ExtensionPoint{{OwnerModuleID: "finance.ar", ID: "invoice.validation", Version: 2, Target: invoice, AllowedKinds: []string{string(ExtensionValidation)}, ProtectedFields: []string{"status"}, InputSchema: SchemaRef{ID: "invoice", Version: 1}, OutputSchema: SchemaRef{ID: "validation", Version: 1}, ExecutionPhase: "before_commit", FailurePolicy: "reject"}}}
	handlerA := ResourceRef{Namespace: "tenant", Name: "tax.validate", Version: 1}
	handlerB := ResourceRef{Namespace: "tenant", Name: "tax.validate.discount", Version: 1}
	extenderA := ModuleDescriptor{ID: "tax.vat", Version: "1.0.0", Resources: []OwnedResource{{Ref: handlerA, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}}, Extensions: []ExtensionRegistration{{ID: "vat-check", ExtenderModuleID: "tax.vat", PointID: "finance.ar/invoice.validation", PointVersion: 2, Kind: ExtensionValidation, ArtifactDigest: strings.Repeat("a", 64), HandlerRef: handlerA, Order: 10}}}
	extenderB := ModuleDescriptor{ID: "tax.discount", Version: "1.0.0", Resources: []OwnedResource{{Ref: handlerB, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}}, Extensions: []ExtensionRegistration{{ID: "discount-check", ExtenderModuleID: "tax.discount", PointID: "finance.ar/invoice.validation", PointVersion: 2, Kind: ExtensionValidation, ArtifactDigest: strings.Repeat("b", 64), HandlerRef: handlerB, Order: 20, DependsOn: []string{"tax.vat/vat-check"}}}}
	registered := func(ref SchemaRef) bool { return ref.ID != "missing" }
	modules := []ModuleDescriptor{extenderB, owner, extenderA}
	ordered, err := OrderModuleExtensions(modules, registered)
	if err != nil {
		t.Fatal("valid extension plan rejected:", err)
	}
	if len(ordered) != 2 || ordered[0].Registration.ID != "vat-check" || ordered[1].Registration.ID != "discount-check" {
		t.Fatalf("extension order = %+v, want dependency-ordered registrations", ordered)
	}
	orderedAgain, err := OrderModuleExtensions([]ModuleDescriptor{owner, extenderA, extenderB}, registered)
	if err != nil || len(orderedAgain) != len(ordered) || orderedAgain[0].Registration.ID != ordered[0].Registration.ID || orderedAgain[1].Registration.ID != ordered[1].Registration.ID {
		t.Fatalf("extension plan changed with input permutation: %+v, err=%v", orderedAgain, err)
	}
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	compositionA, err := PlanComposition(modules, nil, nil, scope, 7)
	if err != nil {
		t.Fatal("plan extension composition:", err)
	}
	compositionB, err := PlanComposition([]ModuleDescriptor{extenderA, owner, extenderB}, nil, nil, scope, 7)
	if err != nil || compositionA.PlanHash != compositionB.PlanHash {
		t.Fatalf("composition plan hash changed with module order: %q != %q, err=%v", compositionA.PlanHash, compositionB.PlanHash, err)
	}
	if err := ValidateModuleSet(modules, registered); err != nil {
		t.Fatal("valid module composition rejected:", err)
	}

	assertRejected := func(name string, candidate []ModuleDescriptor) {
		t.Helper()
		if err := ValidateModuleSet(candidate, registered); err == nil {
			t.Fatalf("%s accepted before activation", name)
		}
	}
	bad := append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].PointID = "finance.ar/undeclared"
	assertRejected("undeclared extension point", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].PointVersion++
	assertRejected("incompatible extension version", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].Kind = ExtensionPostCommit
	assertRejected("undeclared extension kind", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].Event = "after_save"
	assertRejected("validation extension with post-commit event", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].HandlerRef = ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}
	assertRejected("handler owned by another module", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[1].ExtensionPoints = append([]ExtensionPoint(nil), owner.ExtensionPoints...)
	bad[1].ExtensionPoints[0].Target = ResourceRef{Namespace: "tenant", Name: "Other", Version: 1}
	assertRejected("target not owned by declaring module", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].ArtifactDigest = "bad-digest"
	assertRejected("invalid artifact digest", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].Order = extenderA.Extensions[0].Order
	assertRejected("ambiguous extension ordering", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].DependsOn = []string{"unknown/extension"}
	assertRejected("unknown extension dependency", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[0].Extensions = append([]ExtensionRegistration(nil), extenderB.Extensions...)
	bad[0].Extensions[0].DependsOn = []string{"tax.vat/vat-check"}
	bad[2].Extensions = append([]ExtensionRegistration(nil), extenderA.Extensions...)
	bad[2].Extensions[0].DependsOn = []string{"tax.discount/discount-check"}
	assertRejected("extension dependency cycle", bad)
	bad = append([]ModuleDescriptor(nil), modules...)
	bad[1].ExtensionPoints = append([]ExtensionPoint(nil), owner.ExtensionPoints...)
	bad[1].ExtensionPoints[0].InputSchema.ID = "missing"
	assertRejected("unregistered extension schema", bad)
	if err := ValidateModuleSet(modules, func(SchemaRef) bool { return false }); err == nil {
		t.Fatal("unregistered point schema was accepted")
	}

	withoutExtension := []ModuleDescriptor{owner, extenderA}
	withoutExtension[1].Extensions = nil
	disabled, err := OrderModuleExtensions(withoutExtension, registered)
	if err != nil || len(disabled) != 0 {
		t.Fatalf("disabled extension remained in runtime plan: %+v, err=%v", disabled, err)
	}
	updatedOwner := owner
	updatedOwner.Version = "1.1.0"
	updatedOwner.ExtensionPoints = append([]ExtensionPoint(nil), owner.ExtensionPoints...)
	updatedOwner.ExtensionPoints[0].Version++
	assertRejected("owner update incompatible with active extension", []ModuleDescriptor{updatedOwner, extenderA})
}

func TestOwnerModuleFieldExtensionsRequireNamespacedRegisteredFields(t *testing.T) {
	target := ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}
	handler := ResourceRef{Namespace: "tenant", Name: "tax.fields", Version: 1}
	owner := ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Resources: []OwnedResource{{Ref: target, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}}, ExtensionPoints: []ExtensionPoint{{
		OwnerModuleID: "finance.ar", ID: "invoice.fields", Version: 1, Target: target, AllowedKinds: []string{string(ExtensionFields)}, ProtectedFields: []string{"status", "tax.vat.status"},
		InputSchema: SchemaRef{ID: "invoice-fields", Version: 1}, OutputSchema: SchemaRef{ID: "invoice-fields-result", Version: 1}, ExecutionPhase: "activation", FailurePolicy: "reject",
	}}}
	extension := ModuleDescriptor{ID: "tax.vat", Version: "1.0.0", Resources: []OwnedResource{{Ref: handler, Access: ModulePrivate, MutationMode: MutationCommands, ScopeKind: ScopeTenantShared}}, Extensions: []ExtensionRegistration{{
		ID: "vat-fields", ExtenderModuleID: "tax.vat", PointID: "finance.ar/invoice.fields", PointVersion: 1, Kind: ExtensionFields,
		ArtifactDigest: strings.Repeat("e", 64), HandlerRef: handler, Order: 1,
		AddedFields: []ExtensionField{{Key: "tax.vat.registration_code", Schema: SchemaRef{ID: "registration-code", Version: 2}}},
	}}}
	modules := []ModuleDescriptor{owner, extension}
	if err := ValidateModuleSet(modules, func(ref SchemaRef) bool { return ref.ID != "missing" }); err != nil {
		t.Fatal("valid namespaced field extension rejected:", err)
	}
	bad := extension
	bad.Extensions = append([]ExtensionRegistration(nil), extension.Extensions...)
	bad.Extensions[0].AddedFields = []ExtensionField{{Key: "status", Schema: SchemaRef{ID: "registration-code", Version: 2}}}
	if err := ValidateModuleSet([]ModuleDescriptor{owner, bad}, nil); err == nil {
		t.Fatal("extension overrode an owner field")
	}
	bad = extension
	bad.Extensions = append([]ExtensionRegistration(nil), extension.Extensions...)
	bad.Extensions[0].AddedFields = []ExtensionField{{Key: "tax.vat.status", Schema: SchemaRef{ID: "registration-code", Version: 2}}}
	if err := ValidateModuleSet([]ModuleDescriptor{owner, bad}, nil); err == nil {
		t.Fatal("extension overrode an owner-reserved namespaced field")
	}
	bad = extension
	bad.Extensions = append([]ExtensionRegistration(nil), extension.Extensions...)
	bad.Extensions[0].AddedFields = []ExtensionField{{Key: "tax.vat.registration_code", Schema: SchemaRef{ID: "missing", Version: 1}}}
	if err := ValidateModuleSet([]ModuleDescriptor{owner, bad}, func(ref SchemaRef) bool { return ref.ID != "missing" }); err == nil {
		t.Fatal("unregistered field schema accepted")
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

func TestPlanCompositionRejectsMissingAndSelfInitializationRequirements(t *testing.T) {
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	missing := ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []CapabilityRequirement{{Name: "billing.post", Contract: SchemaRef{ID: "journal", Version: 1}, Required: true}}}
	if _, err := PlanComposition([]ModuleDescriptor{missing}, nil, nil, scope, 1); err == nil {
		t.Fatal("missing mandatory capability accepted")
	}
	offer := CapabilityOffer{Name: "self.calculate", Contract: SchemaRef{ID: "calculation", Version: 1}}
	self := ModuleDescriptor{ID: "finance.calc", Version: "1.0.0", Provides: []CapabilityOffer{offer}, Requires: []CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "calculator", OwnerModuleID: self.ID, Kind: "internal", Offers: []CapabilityOffer{offer}}
	if _, err := PlanComposition([]ModuleDescriptor{self}, []ProviderRegistration{provider}, nil, scope, 1); err == nil {
		t.Fatal("self dependency accepted")
	}
}

func TestPlanCompositionBoundsConcreteCycleDiagnostics(t *testing.T) {
	const size = 24
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	modules := make([]ModuleDescriptor, 0, size)
	providers := make([]ProviderRegistration, 0, size)
	for i := 0; i < size; i++ {
		id := ModuleID(fmt.Sprintf("module.%02d", i))
		next := (i + 1) % size
		capability := CapabilityName(fmt.Sprintf("capability.%02d", i))
		nextCapability := CapabilityName(fmt.Sprintf("capability.%02d", next))
		currentSchema := SchemaRef{ID: fmt.Sprintf("schema.%02d", i), Version: 1}
		nextSchema := SchemaRef{ID: fmt.Sprintf("schema.%02d", next), Version: 1}
		modules = append(modules, ModuleDescriptor{ID: id, Version: "1.0.0", Provides: []CapabilityOffer{{Name: capability, Contract: currentSchema}}, Requires: []CapabilityRequirement{{Name: nextCapability, Contract: nextSchema, Required: true}}})
		providers = append(providers, ProviderRegistration{TenantID: scope.TenantID, ProviderID: string(id), OwnerModuleID: id, Kind: "internal", Offers: []CapabilityOffer{{Name: capability, Contract: currentSchema}}})
	}
	plan, err := PlanComposition(modules, providers, nil, scope, 2)
	if err == nil || len(plan.CyclePaths) != 1 || len(plan.CyclePaths[0]) > 16 {
		t.Fatalf("cycle diagnostic should be concrete and bounded: plan=%+v err=%v", plan, err)
	}
}

func TestApplicationCatalogValidatesAndBoundsSchemaDocuments(t *testing.T) {
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	in := SchemaRef{ID: "sales.input", Version: 1}
	out := SchemaRef{ID: "sales.output", Version: 1}
	catalog := ApplicationCatalog{Revision: 1, BusinessScope: scope,
		Modules:              []ApplicationCatalogModule{{ID: "sales.orders", Version: "1.0.0", State: string(ModuleActive)}},
		PublishedQueries:     []ApplicationCatalogQuery{{OwnerModuleID: "sales.orders", Name: "list", Capability: "sales.read", Route: "/api/v1/modules/sales.orders/queries/list", InputSchema: in, OutputSchema: out, ScopePolicy: ScopeTenantShared, AllowedFields: []string{"id"}, PageLimit: 10}},
		PublishedProjections: []ApplicationCatalogProjection{},
		Schemas:              []ModuleSchemaDocument{{Ref: in, Schema: []byte(`{"type":"object"}`)}, {Ref: out, Schema: []byte(`{"type":"array","items":{"type":"object"}}`)}},
		AllowedEntityChoices: []BusinessScope{}, Blockers: []ModuleBlocker{}}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
	}
	catalog.Schemas[0].Schema = []byte(`{"$ref":"https://example.com/schema.json"}`)
	if err := catalog.Validate(); err == nil {
		t.Fatal("remote schema reference accepted")
	}
}

func TestPublishedOperationWireValidationRequiresBoundedIntentAndCompletedOutput(t *testing.T) {
	request := ExecutePublishedOperation{Meta: CommandMeta{IdempotencyKey: "retry-1"}, InputSchema: SchemaRef{ID: "sales.input", Version: 1}, Input: []byte(`{"amount":3}`)}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid execute request rejected: %v", err)
	}
	request.Input = []byte("{")
	if err := request.Validate(); err == nil {
		t.Fatal("invalid JSON input accepted")
	}
	scope := BusinessScope{TenantID: "tenant-a", Kind: ScopeKindTenant}
	receipt := OperationReceipt{ExecutionID: "exec-1", OperationRef: "sales~orders~issue", BusinessScope: scope, Status: ReceiptCompleted, OutputSchema: &SchemaRef{ID: "sales.output", Version: 1}, Output: []byte(`{"ok":true}`), Revision: "1"}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("valid completed receipt rejected: %v", err)
	}
	receipt.Output = nil
	if err := receipt.Validate(); err == nil {
		t.Fatal("completed receipt without output accepted")
	}
	for _, status := range []OperationReceiptStatus{ReceiptRejected, ReceiptConflict, ReceiptFailed, ReceiptOutcomeUnknown} {
		terminal := OperationReceipt{ExecutionID: "exec-2", OperationRef: "sales~orders~issue", BusinessScope: scope, Status: status, Revision: "1"}
		if err := terminal.Validate(); err == nil {
			t.Fatalf("%s receipt without typed error accepted", status)
		}
		terminal.Error = NewError(CodeConflict, "operation requires recovery")
		if err := terminal.Validate(); err != nil {
			t.Fatalf("%s receipt with typed error rejected: %v", status, err)
		}
	}
	accepted := OperationReceipt{ExecutionID: "exec-3", OperationRef: "sales~orders~issue", BusinessScope: scope, Status: ReceiptAccepted, Revision: "1"}
	if err := accepted.Validate(); err != nil {
		t.Fatalf("accepted receipt without output rejected: %v", err)
	}
}

func TestPublishedOperationLookupValidationRejectsBlankKeys(t *testing.T) {
	if err := (LookupPublishedOperation{OperationRef: "sales.orders/issue", IdempotencyKey: "retry-1"}).Validate(); err != nil {
		t.Fatalf("valid lookup rejected: %v", err)
	}
	if err := (LookupPublishedOperation{OperationRef: "sales.orders/issue"}).Validate(); err == nil {
		t.Fatal("lookup without idempotency key accepted")
	}
}

func TestPublishedOperationsRequireExplicitCallerAllowlist(t *testing.T) {
	input := SchemaRef{ID: "billing.issue.input", Version: 1}
	output := SchemaRef{ID: "billing.issue.output", Version: 1}
	module := ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []CapabilityOffer{{Name: "billing.invoice", Contract: output, Operations: []OperationSpec{{Name: "issue", Input: input, Output: output, Effect: EffectLocal}}}}, PublishedOperations: []PublishedOperation{{Name: "issue", Capability: "billing.invoice", Operation: "issue", ScopePolicy: ScopeTenantShared, AllowedCallers: []ModuleID{"sales.orders"}}}}
	if err := module.Validate(nil); err != nil {
		t.Fatalf("valid caller allowlist rejected: %v", err)
	}
	module.PublishedOperations[0].AllowedCallers = nil
	if err := module.Validate(nil); err == nil {
		t.Fatal("published operation without trusted caller allowlist accepted")
	}
	module.PublishedOperations[0].AllowedCallers = []ModuleID{"sales.orders", "sales.orders"}
	if err := module.Validate(nil); err == nil {
		t.Fatal("duplicate trusted caller accepted")
	}
}

func TestPublishedQueryRequestBoundsInputAndPagination(t *testing.T) {
	request := PublishedQueryRequest{Input: json.RawMessage(`{"status":"open"}`), Cursor: "cursor-1", Limit: 25}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid published query request rejected: %v", err)
	}
	request.Input = json.RawMessage(`{`)
	if err := request.Validate(); err == nil {
		t.Fatal("invalid query input JSON accepted")
	}
	request = PublishedQueryRequest{Input: json.RawMessage(`{}`), Limit: 1001}
	if err := request.Validate(); err == nil {
		t.Fatal("query limit above public bound accepted")
	}
}
