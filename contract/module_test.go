package contract

import "testing"

func TestValidateModuleSetRejectsDuplicateOwnersAndUnknownSchemas(t *testing.T) {
	resource := OwnedResource{Ref: ResourceRef{Namespace: "tenant", Name: "invoice", Version: 1}, Access: ModulePrivate, MutationMode: MutationCommands}
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
	valid := `{"schema_version":1,"package_id":"finance","release_id":"finance-1","version":"1.0.0","kind":"module","public":{"slug":"finance","name":"Finance","summary":"Finance"},"modules":[{"id":"finance.ar","version":"2.0.0","resources":[{"ref":{"namespace":"tenant","name":"invoice","version":1},"access":"private","mutation_mode":"commands_only"}]}],"engine":{"min_version":"1.0.0"}}`
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
	ref := BusinessRef{ProviderID: "warehouse-a", ResourceType: "warehouse.location", ID: "same-id"}
	resolver := &referenceResolverFixture{result: ResolvedBusinessRef{Canonical: ref, Revision: 4, Display: "Main"}}
	resolved, err := ValidateBusinessReference(resolver, ScopeContext{TenantID: "tenant-a", LegalEntityID: "entity-a"}, ref, "warehouse.location")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Revision != 4 || resolver.scope.LegalEntityID != "entity-a" {
		t.Fatalf("scope or revision lost: %#v %#v", resolved, resolver.scope)
	}
	other := ref
	other.ProviderID = "warehouse-b"
	if other == ref {
		t.Fatal("provider did not participate in identity")
	}
	if _, err := ValidateBusinessReference(resolver, ScopeContext{TenantID: "tenant-a"}, ref, "account.customer"); err == nil {
		t.Fatal("wrong resource type accepted")
	}
	if _, err := ValidateBusinessReference(nil, ScopeContext{TenantID: "tenant-a"}, ref, "warehouse.location"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
