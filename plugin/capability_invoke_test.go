package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

type operationTestFactory struct{ handle *operationTestHandle }

func (f operationTestFactory) Capability() contract.CapabilityName { return f.handle.name }
func (f operationTestFactory) Open(context contract.OperationContext) (contract.CapabilityHandle, error) {
	f.handle.opened++
	return f.handle, nil
}

type operationTestHandle struct {
	name        contract.CapabilityName
	scope       contract.GrantScope
	opened      int
	calls       int
	revoked     bool
	invocation  CapabilityInvocation
	executionID contract.OperationID
	output      json.RawMessage
	err         error
}

type operationTestLeaseAdmission struct {
	count int
	err   error
}

func (a *operationTestLeaseAdmission) AdmitCapabilityAttempt(context.Context, contract.CapabilityLease, time.Time) error {
	a.count++
	return a.err
}

type referenceTestHandle struct {
	name    contract.CapabilityName
	scope   contract.BusinessScope
	opened  int
	result  contract.ResolvedBusinessRef
	err     error
	revoked bool
}

func (h *referenceTestHandle) Name() contract.CapabilityName      { return h.name }
func (h *referenceTestHandle) Scope() contract.GrantScope         { return contract.GrantScopeOperation }
func (h *referenceTestHandle) Revoke()                            { h.revoked = true }
func (h *referenceTestHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
func (h *referenceTestHandle) Resolve(_ context.Context, scope contract.ScopeContext, ref contract.BusinessRef) (contract.ResolvedBusinessRef, error) {
	if scope.Scope != h.scope {
		return contract.ResolvedBusinessRef{}, errors.New("scope mismatch")
	}
	if h.err != nil {
		return contract.ResolvedBusinessRef{}, h.err
	}
	return h.result, nil
}

type referenceTestFactory struct {
	name   contract.CapabilityName
	handle *referenceTestHandle
}

func (f referenceTestFactory) Capability() contract.CapabilityName { return f.name }
func (f referenceTestFactory) Open(contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.handle, nil
}
func (f referenceTestFactory) OpenReferenceResolver(context.Context, contract.OperationContext) (contract.BusinessReferenceProviderHandle, error) {
	f.handle.opened++
	return f.handle, nil
}

func TestBindingPlanResolvesReferencesOnlyThroughSelectedScopedProvider(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	capability := contract.CapabilityName("warehouse.lookup")
	contractRef := contract.SchemaRef{ID: "warehouse.contract", Version: 1}
	offer := contract.CapabilityOffer{Name: capability, Contract: contractRef}
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: capability, Contract: contractRef, Required: true}}}
	ownerA := contract.ModuleDescriptor{ID: "warehouse.one", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	ownerB := contract.ModuleDescriptor{ID: "warehouse.two", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	ref := contract.BusinessRef{ProviderID: "warehouse-a", ResourceType: "warehouse.location", ID: "same-id", Scope: scope}
	handleA := &referenceTestHandle{name: capability, scope: scope, result: contract.ResolvedBusinessRef{Canonical: ref, Revision: 1, Display: "A"}}
	refB := ref
	refB.ProviderID = "warehouse-b"
	handleB := &referenceTestHandle{name: capability, scope: scope, result: contract.ResolvedBusinessRef{Canonical: refB, Revision: 9, Display: "B"}}
	providers := []ProviderRegistration{
		{TenantID: scope.TenantID, ProviderID: "warehouse-a", OwnerModuleID: ownerA.ID, Kind: ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{capability: referenceTestFactory{capability, handleA}}},
		{TenantID: scope.TenantID, ProviderID: "warehouse-b", OwnerModuleID: ownerB.ID, Kind: ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{capability: referenceTestFactory{capability, handleB}}},
	}
	modules := []contract.ModuleDescriptor{consumer, ownerA, ownerB}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: capability}, ProviderID: "warehouse-a"}
	plan, err := ResolveModules(scope, modules, providers, []ProviderSelection{selection}, 7)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	rule := contract.ReferenceRule{Kind: "published_business_ref", CapabilityName: capability, ResourceType: "warehouse.location"}
	resolved, err := plan.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, ref)
	if err != nil || resolved.Display != "A" || !handleA.revoked {
		t.Fatalf("selected provider resolution = %#v, %v, revoked=%v", resolved, err, handleA.revoked)
	}
	if _, err := plan.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, refB); err == nil {
		t.Fatal("reference from an unselected provider was accepted")
	}
	providerBSelection := ProviderSelection{Key: selection.Key, ProviderID: "warehouse-b"}
	rebound, err := ResolveModules(scope, modules, providers, []ProviderSelection{providerBSelection}, 7)
	if err != nil {
		t.Fatal(err)
	}
	resolvedB, err := rebound.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, refB)
	if err != nil || resolvedB.Display != "B" {
		t.Fatalf("replacement provider did not resolve its own same-id identity: %#v, %v", resolvedB, err)
	}
	if _, err := rebound.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, ref); err == nil {
		t.Fatal("provider replacement silently reinterpreted historical provider-A identity")
	}
	wrongType := ref
	wrongType.ResourceType = "account.customer"
	if _, err := plan.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, wrongType); err == nil {
		t.Fatal("wrong resource type was accepted")
	}
	otherScope := scope
	otherScope.LegalEntityID = "entity-b"
	if _, err := plan.ResolveBusinessReference(context.Background(), execution, otherScope, consumer.ID, rule, ref); err == nil {
		t.Fatal("cross-scope reference was accepted")
	}
	handleA.err = errors.New("provider offline")
	if _, err := plan.ResolveBusinessReference(context.Background(), execution, scope, consumer.ID, rule, ref); err == nil {
		t.Fatal("selected provider outage was accepted")
	}
	if handleB.opened != 1 {
		t.Fatalf("outage silently rerouted to another provider; provider B opens=%d", handleB.opened)
	}
}

func (h *operationTestHandle) Name() contract.CapabilityName      { return h.name }
func (h *operationTestHandle) Scope() contract.GrantScope         { return h.scope }
func (h *operationTestHandle) Revoke()                            { h.revoked = true }
func (h *operationTestHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
func (h *operationTestHandle) InvokeCapability(_ context.Context, invocation CapabilityInvocation) (CapabilityInvocationResult, error) {
	h.calls++
	h.invocation = invocation
	return CapabilityInvocationResult{ExecutionID: h.executionID, Output: append(json.RawMessage(nil), h.output...)}, h.err
}

func TestBindingPlanInvokesPinnedCapabilityWithSchemasAndProvenance(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "invoice.issue.input", Version: 1}, contract.SchemaRef{ID: "invoice.issue.output", Version: 2}
	offer := contract.CapabilityOffer{Name: "billing.issue", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectLocal}}}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	handle := &operationTestHandle{name: offer.Name, scope: contract.GrantScopeOperation, executionID: "provider-execution-1", output: json.RawMessage(`{"invoice_id":"INV-1"}`)}
	validate := func(schema contract.SchemaRef, data []byte) error {
		if !json.Valid(data) {
			return errors.New("invalid JSON")
		}
		if schema == input && string(data) != `{"order_id":"ORD-1"}` {
			return errors.New("input contract mismatch")
		}
		if schema == output && string(data) != `{"invoice_id":"INV-1"}` {
			return errors.New("output contract mismatch")
		}
		return nil
	}
	provider := ProviderRegistration{
		TenantID: scope.TenantID, ProviderID: "ar-internal", OwnerModuleID: owner.ID,
		Kind: ProviderInternal, Offers: []contract.CapabilityOffer{offer},
		Factories: map[contract.CapabilityName]ProviderFactory{offer.Name: operationTestFactory{handle}}, ValidateSchema: validate,
	}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, owner}, []ProviderRegistration{provider}, nil, 12)
	if err != nil {
		t.Fatal("resolve capability binding:", err)
	}
	leaseAdmission := &operationTestLeaseAdmission{}
	plan.LeaseAdmission = leaseAdmission
	lease := contract.CapabilityLease{ID: "lease-issue", TenantID: scope.TenantID, Scope: scope, CallerModuleID: consumer.ID, ProviderID: "ar-internal", Operation: "issue", BindingRevision: plan.Revision, GrantEpoch: 2, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{
		PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("verified actor execution:", err)
	}
	meta := contract.CommandMeta{IdempotencyKey: "issue-ord-1"}
	result, err := plan.InvokeCapability(context.Background(), execution, lease, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"order_id":"ORD-1"}`))
	if err != nil {
		t.Fatal("invoke pinned capability:", err)
	}
	if result.Status != "completed" || result.ExecutionID != "provider-execution-1" || result.OutputSchema != output || string(result.Output) != `{"invoice_id":"INV-1"}` {
		t.Fatalf("capability result = %+v", result)
	}
	if handle.calls != 1 || !handle.revoked || handle.invocation.Operation != offer.Operations[0] || handle.invocation.Meta != meta || string(handle.invocation.Payload) != `{"order_id":"ORD-1"}` {
		t.Fatalf("provider invocation or cleanup = calls:%d revoked:%t invocation:%+v", handle.calls, handle.revoked, handle.invocation)
	}
	if leaseAdmission.count != 1 {
		t.Fatalf("capability effect admissions=%d, want one", leaseAdmission.count)
	}
	moduleExecution := handle.invocation.ModuleExecution
	if !moduleExecution.Verified() || moduleExecution.TenantID != scope.TenantID || moduleExecution.CallerModuleID != consumer.ID || moduleExecution.OwnerModuleID != owner.ID || moduleExecution.BindingRevision != plan.Revision || moduleExecution.AllowedOperation != "issue" {
		t.Fatalf("provider received unpinned module provenance: %+v", moduleExecution)
	}
	if handle.invocation.BusinessReferenceValidator == nil {
		t.Fatal("provider operation did not receive the plan-scoped reference validator")
	}
}

func TestBindingPlanRejectsBadCapabilitySchemasBeforeAndAfterProviderCall(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "input", Version: 1}, contract.SchemaRef{ID: "output", Version: 1}
	offer := contract.CapabilityOffer{Name: "billing.issue", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectExternal}}}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	handle := &operationTestHandle{name: offer.Name, executionID: "provider-execution-2", output: json.RawMessage(`{"wrong":true}`)}
	validate := func(schema contract.SchemaRef, data []byte) error {
		want := `{"order_id":"ORD-1"}`
		if schema == output {
			want = `{"invoice_id":"INV-1"}`
		}
		if string(data) != want {
			return errors.New("schema mismatch")
		}
		return nil
	}
	provider := ProviderRegistration{TenantID: scope.TenantID, ProviderID: "ar-external", OwnerModuleID: owner.ID, Kind: ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{offer.Name: operationTestFactory{handle}}, ValidateSchema: validate}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumer, owner}, []ProviderRegistration{provider}, nil, 3)
	if err != nil {
		t.Fatal("resolve external capability:", err)
	}
	plan.LeaseAdmission = &operationTestLeaseAdmission{}
	lease := contract.CapabilityLease{ID: "lease-issue", TenantID: scope.TenantID, Scope: scope, CallerModuleID: consumer.ID, ProviderID: "ar-external", Operation: "issue", BindingRevision: plan.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("verified actor execution:", err)
	}
	meta := contract.CommandMeta{IdempotencyKey: "issue-ord-2"}
	if _, err := plan.InvokeCapability(context.Background(), execution, lease, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"bad":"input"}`)); err == nil {
		t.Fatal("input schema mismatch reached provider")
	}
	if handle.opened != 0 || handle.calls != 0 {
		t.Fatalf("invalid input opened provider: opens=%d calls=%d", handle.opened, handle.calls)
	}
	if _, err := plan.InvokeCapability(context.Background(), execution, lease, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"order_id":"ORD-1"}`)); err == nil {
		t.Fatal("output schema mismatch was accepted")
	}
	if handle.calls != 1 || !handle.revoked {
		t.Fatalf("bad output provider call/cleanup = calls:%d revoked:%t", handle.calls, handle.revoked)
	}
	handle.executionID = ""
	handle.output = json.RawMessage(`{"invoice_id":"INV-1"}`)
	if _, err := plan.InvokeCapability(context.Background(), execution, lease, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"order_id":"ORD-1"}`)); err == nil {
		t.Fatal("provider result without an execution identity was accepted")
	}
	if handle.calls != 2 || !handle.revoked {
		t.Fatalf("missing execution identity provider call/cleanup = calls:%d revoked:%t", handle.calls, handle.revoked)
	}
}
