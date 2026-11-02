package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
)

type referenceAPIHandle struct {
	name    contract.CapabilityName
	ref     contract.BusinessRef
	revoked bool
}

func (h *referenceAPIHandle) Name() contract.CapabilityName      { return h.name }
func (h *referenceAPIHandle) Scope() contract.GrantScope         { return contract.GrantScopeOperation }
func (h *referenceAPIHandle) Revoke()                            { h.revoked = true }
func (h *referenceAPIHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
func (h *referenceAPIHandle) Resolve(_ context.Context, scope contract.ScopeContext, ref contract.BusinessRef) (contract.ResolvedBusinessRef, error) {
	return contract.ResolvedBusinessRef{Canonical: h.ref, Revision: 2, Display: "Main"}, nil
}

type referenceAPIFactory struct {
	name   contract.CapabilityName
	handle *referenceAPIHandle
}

func (f referenceAPIFactory) Capability() contract.CapabilityName { return f.name }
func (f referenceAPIFactory) Open(contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.handle, nil
}
func (f referenceAPIFactory) OpenReferenceResolver(context.Context, contract.OperationContext) (contract.BusinessReferenceProviderHandle, error) {
	return f.handle, nil
}

func TestBusinessReferenceValidatorUsesRequestScopeAndSelectedPlan(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	actor := contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()}
	execution, err := policy.NewVerifiedExecution(scope.TenantID, actor)
	if err != nil {
		t.Fatal(err)
	}
	capability := contract.CapabilityName("warehouse.lookup")
	ref := contract.BusinessRef{ProviderID: "warehouse-a", ResourceType: "warehouse.location", ID: "loc-1", Scope: scope}
	handle := &referenceAPIHandle{name: capability, ref: ref}
	factory := referenceAPIFactory{name: capability, handle: handle}
	plan := plugin.BindingPlan{Revision: 3, Bindings: []plugin.ResolvedBinding{{Key: plugin.BindingKey{Scope: scope, ConsumerModuleID: "sales.orders", CapabilityName: capability}, ProviderID: "warehouse-a", OwnerModuleID: "warehouse.module", Revision: 3, Factory: factory}}}
	services := SiteRuntimeService{ResolveModuleBindings: func(got contract.BusinessScope) (plugin.BindingPlan, error) {
		if got != scope {
			t.Fatalf("resolver received wrong scope: %#v", got)
		}
		return plan, nil
	}}
	validator := newBusinessReferenceValidator(context.Background(), services, execution, scope)
	raw, _ := json.Marshal(ref)
	rule := contract.ReferenceRule{Kind: "published_business_ref", CapabilityName: capability, ResourceType: "warehouse.location"}
	if err := validator("sales.orders", rule, raw); err != nil {
		t.Fatal(err)
	}
	if !handle.revoked {
		t.Fatal("provider handle was not released")
	}
	ref.ProviderID = "warehouse-b"
	raw, _ = json.Marshal(ref)
	if err := validator("sales.orders", rule, raw); err == nil {
		t.Fatal("unselected provider identity was accepted")
	}
	if got := newBusinessReferenceValidator(context.Background(), services, policy.Execution{}, scope); got != nil {
		t.Fatal("unverified execution received a resolver")
	}
}
