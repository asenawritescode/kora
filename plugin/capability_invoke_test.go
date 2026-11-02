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
	name       contract.CapabilityName
	scope      contract.GrantScope
	opened     int
	calls      int
	revoked    bool
	invocation CapabilityInvocation
	output     json.RawMessage
	err        error
}

func (h *operationTestHandle) Name() contract.CapabilityName      { return h.name }
func (h *operationTestHandle) Scope() contract.GrantScope         { return h.scope }
func (h *operationTestHandle) Revoke()                            { h.revoked = true }
func (h *operationTestHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
func (h *operationTestHandle) InvokeCapability(_ context.Context, invocation CapabilityInvocation) (json.RawMessage, error) {
	h.calls++
	h.invocation = invocation
	return append(json.RawMessage(nil), h.output...), h.err
}

func TestBindingPlanInvokesPinnedCapabilityWithSchemasAndProvenance(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "invoice.issue.input", Version: 1}, contract.SchemaRef{ID: "invoice.issue.output", Version: 2}
	offer := contract.CapabilityOffer{Name: "billing.issue", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectLocal}}}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	handle := &operationTestHandle{name: offer.Name, scope: contract.GrantScopeOperation, output: json.RawMessage(`{"invoice_id":"INV-1"}`)}
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
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{
		PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal("verified actor execution:", err)
	}
	meta := contract.CommandMeta{IdempotencyKey: "issue-ord-1"}
	result, err := plan.InvokeCapability(context.Background(), execution, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"order_id":"ORD-1"}`))
	if err != nil {
		t.Fatal("invoke pinned capability:", err)
	}
	if result.Status != "completed" || result.ExecutionID == "" || result.OutputSchema != output || string(result.Output) != `{"invoice_id":"INV-1"}` {
		t.Fatalf("capability result = %+v", result)
	}
	if handle.calls != 1 || !handle.revoked || handle.invocation.Operation != offer.Operations[0] || handle.invocation.Meta != meta || string(handle.invocation.Payload) != `{"order_id":"ORD-1"}` {
		t.Fatalf("provider invocation or cleanup = calls:%d revoked:%t invocation:%+v", handle.calls, handle.revoked, handle.invocation)
	}
	moduleExecution := handle.invocation.ModuleExecution
	if !moduleExecution.Verified() || moduleExecution.TenantID != scope.TenantID || moduleExecution.CallerModuleID != consumer.ID || moduleExecution.OwnerModuleID != owner.ID || moduleExecution.BindingRevision != plan.Revision || moduleExecution.AllowedOperation != "issue" {
		t.Fatalf("provider received unpinned module provenance: %+v", moduleExecution)
	}
}

func TestBindingPlanRejectsBadCapabilitySchemasBeforeAndAfterProviderCall(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	input, output := contract.SchemaRef{ID: "input", Version: 1}, contract.SchemaRef{ID: "output", Version: 1}
	offer := contract.CapabilityOffer{Name: "billing.issue", Contract: output, Operations: []contract.OperationSpec{{Name: "issue", Input: input, Output: output, Effect: contract.EffectExternal}}}
	owner := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	consumer := contract.ModuleDescriptor{ID: "sales.orders", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: offer.Name, Contract: offer.Contract, Required: true}}}
	handle := &operationTestHandle{name: offer.Name, output: json.RawMessage(`{"wrong":true}`)}
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
	execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: "user-1", PrincipalType: contract.PrincipalHuman, Site: "tenant-a", AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("verified actor execution:", err)
	}
	meta := contract.CommandMeta{IdempotencyKey: "issue-ord-2"}
	if _, err := plan.InvokeCapability(context.Background(), execution, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"bad":"input"}`)); err == nil {
		t.Fatal("input schema mismatch reached provider")
	}
	if handle.opened != 0 || handle.calls != 0 {
		t.Fatalf("invalid input opened provider: opens=%d calls=%d", handle.opened, handle.calls)
	}
	if _, err := plan.InvokeCapability(context.Background(), execution, scope, consumer.ID, offer.Name, "issue", meta, json.RawMessage(`{"order_id":"ORD-1"}`)); err == nil {
		t.Fatal("output schema mismatch was accepted")
	}
	if handle.calls != 1 || !handle.revoked {
		t.Fatalf("bad output provider call/cleanup = calls:%d revoked:%t", handle.calls, handle.revoked)
	}
}
