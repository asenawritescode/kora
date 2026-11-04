package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

// fixtureCommandStore models an owner's command boundary. It deduplicates at
// the owner by idempotency key, as a persisted Kernel command does.
type fixtureCommandStore struct {
	mu       sync.Mutex
	effects  map[string]json.RawMessage
	count    int
	failOnce bool
}

type fixtureCommandFactory struct {
	capability contract.CapabilityName
	scope      contract.BusinessScope
	owner      contract.ModuleID
	store      *fixtureCommandStore
}

func (f fixtureCommandFactory) Capability() contract.CapabilityName { return f.capability }
func (f fixtureCommandFactory) Open(ctx contract.OperationContext) (contract.CapabilityHandle, error) {
	if ctx.Scope != f.scope {
		return nil, errors.New("fixture owner rejected cross-scope dispatch")
	}
	return &fixtureCommandHandle{name: f.capability, owner: f.owner, scope: f.scope, store: f.store}, nil
}

type fixtureCommandHandle struct {
	name  contract.CapabilityName
	owner contract.ModuleID
	scope contract.BusinessScope
	store *fixtureCommandStore
}

type fixtureLeaseAdmission struct {
	mu    sync.Mutex
	count int
}

func (a *fixtureLeaseAdmission) AdmitCapabilityAttempt(context.Context, contract.CapabilityLease, time.Time) error {
	a.mu.Lock()
	a.count++
	a.mu.Unlock()
	return nil
}

func (h *fixtureCommandHandle) Name() contract.CapabilityName      { return h.name }
func (h *fixtureCommandHandle) Scope() contract.GrantScope         { return contract.GrantScopeOperation }
func (h *fixtureCommandHandle) Revoke()                            {}
func (h *fixtureCommandHandle) Evidence() contract.CleanupEvidence { return contract.CleanupEvidence{} }
func (h *fixtureCommandHandle) InvokeCapability(_ context.Context, invocation CapabilityInvocation) (CapabilityInvocationResult, error) {
	if !invocation.ModuleExecution.Verified() || invocation.ModuleExecution.OwnerModuleID != h.owner || invocation.ModuleExecution.AllowedOperation != invocation.Operation.Name || invocation.Context.Scope != h.scope {
		return CapabilityInvocationResult{}, errors.New("owner rejected untrusted command provenance or scope")
	}
	var input map[string]any
	if err := json.Unmarshal(invocation.Payload, &input); err != nil {
		return CapabilityInvocationResult{}, err
	}
	if entity, ok := input["entity_id"].(string); ok && entity != h.scope.LegalEntityID {
		return CapabilityInvocationResult{}, errors.New("fixture owner rejected caller-supplied entity spoof")
	}
	key := string(invocation.Meta.IdempotencyKey)
	h.store.mu.Lock()
	if h.store.effects == nil {
		h.store.effects = map[string]json.RawMessage{}
	}
	output, exists := h.store.effects[key]
	if !exists {
		state := "settled"
		if h.store.failOnce {
			state = "unknown"
		}
		output, _ = json.Marshal(map[string]any{"effect_id": fmt.Sprintf("effect-%d", h.store.count+1), "state": state, "amount": input["amount"], "currency": input["currency"], "source_id": input["source_id"], "entity_id": h.scope.LegalEntityID, "balanced": true, "source_acknowledged": true})
		h.store.effects[key] = output
		h.store.count++
	}
	failAfterEffect := h.store.failOnce
	if failAfterEffect {
		h.store.failOnce = false
	}
	h.store.mu.Unlock()
	if failAfterEffect {
		return CapabilityInvocationResult{}, errors.New("injected response loss after owner commit")
	}
	return CapabilityInvocationResult{ExecutionID: contract.OperationID(key), Output: append(json.RawMessage(nil), output...)}, nil
}

func fixtureScenarioBinding(t *testing.T, scope contract.BusinessScope, consumer, owner contract.ModuleID, capability contract.CapabilityName, kind ProviderKind, providerID string, store *fixtureCommandStore, revision uint64) (BindingPlan, contract.CapabilityLease, policy.Execution) {
	t.Helper()
	input, output := contract.SchemaRef{ID: "fixture." + string(capability) + ".input", Version: 1}, contract.SchemaRef{ID: "fixture." + string(capability) + ".output", Version: 1}
	effect := contract.EffectLocal
	if kind == ProviderExternal {
		effect = contract.EffectExternal
	}
	offer := contract.CapabilityOffer{Name: capability, Contract: output, Operations: []contract.OperationSpec{{Name: "apply", Input: input, Output: output, Effect: effect}}}
	consumerModule := contract.ModuleDescriptor{ID: consumer, Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: capability, Contract: output, Required: true}}}
	ownerModule := contract.ModuleDescriptor{ID: owner, Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	provider := ProviderRegistration{TenantID: scope.TenantID, Scope: scope, ProviderID: providerID, OwnerModuleID: owner, Kind: kind, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{capability: fixtureCommandFactory{capability: capability, scope: scope, owner: owner, store: store}}, ValidateSchema: func(ref contract.SchemaRef, raw []byte) error {
		if !json.Valid(raw) || ref != input && ref != output {
			return errors.New("fixture operation schema rejected data")
		}
		return nil
	}}
	selection := ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer, CapabilityName: capability}, ProviderID: providerID}
	plan, err := ResolveModules(scope, []contract.ModuleDescriptor{consumerModule, ownerModule}, []ProviderRegistration{provider}, []ProviderSelection{selection}, revision)
	if err != nil {
		t.Fatal("resolve programmatic fixture binding:", err)
	}
	plan.LeaseAdmission = &fixtureLeaseAdmission{}
	lease := contract.CapabilityLease{ID: "lease-" + providerID, TenantID: scope.TenantID, Scope: scope, CallerModuleID: consumer, ProviderModuleID: owner, ProviderID: providerID, Operation: "apply", BindingRevision: revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "fixture-operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("create verified fixture actor:", err)
	}
	return plan, lease, execution
}

func invokeFixture(t *testing.T, plan BindingPlan, lease contract.CapabilityLease, execution policy.Execution, scope contract.BusinessScope, caller contract.ModuleID, capability contract.CapabilityName, key string, payload string) (CapabilityOperationResult, error) {
	t.Helper()
	return plan.InvokeCapability(context.Background(), execution, lease, scope, caller, capability, "apply", contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(key)}, json.RawMessage(payload))
}

func TestProgrammaticFixtureProfilesUseDeclaredOwnerOperations(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-scenario", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	t.Run("standalone AR binds external customer billing and ledger without Sales or Inventory", func(t *testing.T) {
		ar := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0"}
		providers := []ProviderRegistration{}
		selections := []ProviderSelection{}
		for _, capability := range []contract.CapabilityName{"customer.lookup", "billing.inputs", "ledger.post"} {
			owner := contract.ModuleID("external." + string(capability))
			ref := contract.SchemaRef{ID: string(capability) + ".v1", Version: 1}
			ar.Requires = append(ar.Requires, contract.CapabilityRequirement{Name: capability, Contract: ref, Required: true})
			offer := contract.CapabilityOffer{Name: capability, Contract: ref}
			providerID := "ext-" + string(capability)
			providers = append(providers, ProviderRegistration{TenantID: scope.TenantID, Scope: scope, ProviderID: providerID, OwnerModuleID: owner, Kind: ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{capability: NewStaticFactory(capability, nil)}})
			selections = append(selections, ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: "finance.ar", CapabilityName: capability}, ProviderID: providerID})
		}
		modules := []contract.ModuleDescriptor{ar}
		for _, provider := range providers {
			modules = append(modules, contract.ModuleDescriptor{ID: provider.OwnerModuleID, Version: "1.0.0", Provides: provider.Offers})
		}
		plan, err := ResolveModules(scope, modules, providers, selections, 1)
		if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 3 {
			t.Fatalf("standalone AR composition = %+v, %v", plan, err)
		}
		for _, module := range modules {
			if module.ID == "sales.orders" || module.ID == "inventory.stock" {
				t.Fatalf("standalone AR pulled undeclared companion module %q", module.ID)
			}
		}
		missing, err := ResolveModules(scope, modules[:1], nil, nil, 2)
		if err != nil || len(missing.Blockers) != 3 {
			t.Fatalf("missing mandatory standalone dependencies = %+v, %v", missing, err)
		}
	})

	t.Run("Sales to AR is idempotent under concurrent delivery", func(t *testing.T) {
		store := &fixtureCommandStore{}
		plan, lease, execution := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "ar.invoice.issue", ProviderInternal, "ar-internal", store, 4)
		payload := `{"source_id":"sale-101","amount":"12.50","currency":"KES","entity_id":"entity-a","order_status":"whatever"}`
		if _, err := invokeFixture(t, plan, lease, execution, scope, "sales.orders", "ar.invoice.issue", "invoice-sale-102", `{"source_id":"sale-102","amount":"8.00","currency":"KES","entity_id":"entity-a"}`); err != nil {
			t.Fatal("reordered accepted billable fact:", err)
		}
		var group sync.WaitGroup
		for i := 0; i < 8; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				_, err := invokeFixture(t, plan, lease, execution, scope, "sales.orders", "ar.invoice.issue", "invoice-sale-101", payload)
				if err != nil {
					t.Errorf("concurrent invoice command: %v", err)
				}
			}()
		}
		group.Wait()
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.count != 2 || len(store.effects) != 2 {
			t.Fatalf("Sales→AR effects=%d receipts=%d, want one per two billable sources", store.count, len(store.effects))
		}
		sources := map[string]bool{}
		for _, raw := range store.effects {
			var invoice struct {
				SourceID string `json:"source_id"`
			}
			if err := json.Unmarshal(raw, &invoice); err != nil {
				t.Fatal("decode fixture invoice:", err)
			}
			sources[invoice.SourceID] = true
		}
		if !sources["sale-101"] || !sources["sale-102"] {
			t.Fatalf("invoice effects are not keyed to both billable sources: %+v", sources)
		}
	})

	t.Run("Inventory to accounting preserves source amount currency and balanced intent", func(t *testing.T) {
		store := &fixtureCommandStore{}
		plan, lease, execution := fixtureScenarioBinding(t, scope, "inventory.valuation", "accounting.ledger", "ledger.journal.post", ProviderInternal, "ledger-internal", store, 5)
		result, err := invokeFixture(t, plan, lease, execution, scope, "inventory.valuation", "ledger.journal.post", "journal-valuation-77", `{"source_id":"valuation-77","amount":"125.40","currency":"KES","entity_id":"entity-a"}`)
		if err != nil {
			t.Fatal("post valuation journal:", err)
		}
		var journal struct {
			SourceID           string `json:"source_id"`
			Amount             string `json:"amount"`
			Currency           string `json:"currency"`
			EntityID           string `json:"entity_id"`
			Balanced           bool   `json:"balanced"`
			SourceAcknowledged bool   `json:"source_acknowledged"`
		}
		if err := json.Unmarshal(result.Output, &journal); err != nil || journal.SourceID != "valuation-77" || journal.Amount != "125.40" || journal.Currency != "KES" || journal.EntityID != scope.LegalEntityID || !journal.Balanced || !journal.SourceAcknowledged {
			t.Fatalf("fixture journal lost source invariants: %+v, %v", journal, err)
		}
	})

	t.Run("external warehouse event uses scoped stock owner and blocks spoofed entity", func(t *testing.T) {
		store := &fixtureCommandStore{}
		plan, lease, execution := fixtureScenarioBinding(t, scope, "warehouse.adapter", "inventory.stock", "inventory.stock.receive", ProviderExternal, "warehouse-provider", store, 6)
		result, err := invokeFixture(t, plan, lease, execution, scope, "warehouse.adapter", "inventory.stock.receive", "fulfillment-21", `{"source_id":"fulfillment-21","amount":"3","currency":"ITEM","entity_id":"entity-a"}`)
		if err != nil || result.Status != "completed" {
			t.Fatalf("authenticated scoped warehouse command = %+v, %v", result, err)
		}
		other := scope
		other.LegalEntityID = "entity-b"
		if _, err := invokeFixture(t, plan, lease, execution, other, "warehouse.adapter", "inventory.stock.receive", "spoof-entity", `{"source_id":"fulfillment-22","amount":"1","currency":"ITEM","entity_id":"entity-a"}`); err == nil {
			t.Fatal("provider event crossed entity scope")
		}
		if _, err := invokeFixture(t, plan, lease, execution, scope, "warehouse.adapter", "inventory.stock.receive", "payload-spoof", `{"source_id":"fulfillment-23","amount":"1","currency":"ITEM","entity_id":"entity-b"}`); err == nil {
			t.Fatal("provider accepted payload entity that differs from authorized scope")
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.count != 1 {
			t.Fatalf("stock owner accepted %d effects after cross-entity spoof", store.count)
		}
	})

	t.Run("checkout keeps a response-lost external payment deduplicated and unknown", func(t *testing.T) {
		capabilities := []struct {
			name     contract.CapabilityName
			owner    contract.ModuleID
			provider string
			kind     ProviderKind
			store    *fixtureCommandStore
		}{
			{name: "sales.order.confirm", owner: "sales.orders", provider: "sales-internal", kind: ProviderInternal, store: &fixtureCommandStore{}},
			{name: "ar.invoice.issue", owner: "finance.ar", provider: "ar-internal", kind: ProviderInternal, store: &fixtureCommandStore{}},
			{name: "payment.capture", owner: "payments.provider", provider: "payment-external", kind: ProviderExternal, store: &fixtureCommandStore{failOnce: true}},
		}
		consumer := contract.ModuleDescriptor{ID: "checkout.coordinator", Version: "1.0.0"}
		modules := []contract.ModuleDescriptor{consumer}
		providers := []ProviderRegistration{}
		selections := []ProviderSelection{}
		leases := map[contract.CapabilityName]contract.CapabilityLease{}
		for _, item := range capabilities {
			input, output := contract.SchemaRef{ID: "fixture." + string(item.name) + ".input", Version: 1}, contract.SchemaRef{ID: "fixture." + string(item.name) + ".output", Version: 1}
			effect := contract.EffectLocal
			if item.kind == ProviderExternal {
				effect = contract.EffectExternal
			}
			offer := contract.CapabilityOffer{Name: item.name, Contract: output, Operations: []contract.OperationSpec{{Name: "apply", Input: input, Output: output, Effect: effect}}}
			consumer.Requires = append(consumer.Requires, contract.CapabilityRequirement{Name: item.name, Contract: output, Required: true})
			modules = append(modules, contract.ModuleDescriptor{ID: item.owner, Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}})
			providers = append(providers, ProviderRegistration{TenantID: scope.TenantID, Scope: scope, ProviderID: item.provider, OwnerModuleID: item.owner, Kind: item.kind, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]ProviderFactory{item.name: fixtureCommandFactory{capability: item.name, scope: scope, owner: item.owner, store: item.store}}, ValidateSchema: func(ref contract.SchemaRef, raw []byte) error {
				if !json.Valid(raw) || ref != input && ref != output {
					return errors.New("checkout fixture schema rejected data")
				}
				return nil
			}})
			selections = append(selections, ProviderSelection{Key: BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: item.name}, ProviderID: item.provider})
			leases[item.name] = contract.CapabilityLease{ID: "lease-" + item.provider, TenantID: scope.TenantID, Scope: scope, CallerModuleID: consumer.ID, ProviderModuleID: item.owner, ProviderID: item.provider, Operation: "apply", BindingRevision: 8, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
		}
		modules[0] = consumer
		plan, err := ResolveModules(scope, modules, providers, selections, 8)
		if err != nil || len(plan.Blockers) != 0 || len(plan.Bindings) != 3 {
			t.Fatalf("checkout multi-owner composition = %+v, %v", plan, err)
		}
		plan.LeaseAdmission = &fixtureLeaseAdmission{}
		execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "checkout-operator", PrincipalType: contract.PrincipalHuman, Site: string(scope.TenantID), AuthenticatedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal("create checkout actor:", err)
		}
		payload := `{"source_id":"checkout-88","amount":"44.00","currency":"KES","entity_id":"entity-a"}`
		for _, capability := range capabilities[:2] {
			if _, err := invokeFixture(t, plan, leases[capability.name], execution, scope, "checkout.coordinator", capability.name, "checkout-88/"+string(capability.name), payload); err != nil {
				t.Fatalf("local owner %s command: %v", capability.owner, err)
			}
		}
		if _, err := invokeFixture(t, plan, leases["payment.capture"], execution, scope, "checkout.coordinator", "payment.capture", "payment-checkout-88", payload); err == nil {
			t.Fatal("lost provider response was treated as confirmed")
		}
		result, err := invokeFixture(t, plan, leases["payment.capture"], execution, scope, "checkout.coordinator", "payment.capture", "payment-checkout-88", payload)
		if err != nil || result.Status != "completed" {
			t.Fatalf("same-key payment reconciliation retry = %+v, %v", result, err)
		}
		var payment struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(result.Output, &payment); err != nil || payment.State != "unknown" {
			t.Fatalf("lost provider response was hidden by fixture output: %+v, %v", payment, err)
		}
		for _, capability := range capabilities {
			capability.store.mu.Lock()
			count := capability.store.count
			capability.store.mu.Unlock()
			if count != 1 {
				t.Fatalf("checkout owner %s effects=%d, want one", capability.owner, count)
			}
		}
	})

	t.Run("internal and external billing adapters implement the same operation contract", func(t *testing.T) {
		var outputs [][]byte
		for _, kind := range []ProviderKind{ProviderInternal, ProviderExternal} {
			store := &fixtureCommandStore{}
			providerID := "billing-" + string(kind)
			plan, lease, execution := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "billing.invoice.issue", kind, providerID, store, 9)
			result, err := invokeFixture(t, plan, lease, execution, scope, "sales.orders", "billing.invoice.issue", "billing-contract-1", `{"source_id":"sale-contract-1","amount":"18.75","currency":"KES","entity_id":"entity-a"}`)
			if err != nil {
				t.Fatalf("%s adapter contract call: %v", kind, err)
			}
			outputs = append(outputs, result.Output)
		}
		if string(outputs[0]) != string(outputs[1]) {
			t.Fatalf("internal/external adapter contract diverged: %s / %s", outputs[0], outputs[1])
		}
	})

	t.Run("external billing provider can be replaced by an internal owner for the same operation", func(t *testing.T) {
		externalStore := &fixtureCommandStore{}
		internalStore := &fixtureCommandStore{}
		externalPlan, externalLease, execution := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "billing.invoice.issue", ProviderExternal, "billing-external-first", externalStore, 13)
		internalPlan, internalLease, _ := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "billing.invoice.issue", ProviderInternal, "billing-internal-after", internalStore, 14)
		payload := `{"source_id":"sale-provider-cutover","amount":"21.00","currency":"KES","entity_id":"entity-a"}`

		externalResult, err := invokeFixture(t, externalPlan, externalLease, execution, scope, "sales.orders", "billing.invoice.issue", "invoice-external", payload)
		if err != nil || externalResult.Status != "completed" {
			t.Fatalf("external provider operation = %+v, %v", externalResult, err)
		}
		internalResult, err := invokeFixture(t, internalPlan, internalLease, execution, scope, "sales.orders", "billing.invoice.issue", "invoice-internal", payload)
		if err != nil || internalResult.Status != "completed" {
			t.Fatalf("internal provider operation after cutover = %+v, %v", internalResult, err)
		}

		var externalOutput, internalOutput struct {
			Amount             string `json:"amount"`
			Currency           string `json:"currency"`
			SourceID           string `json:"source_id"`
			EntityID           string `json:"entity_id"`
			Balanced           bool   `json:"balanced"`
			SourceAcknowledged bool   `json:"source_acknowledged"`
		}
		if err := json.Unmarshal(externalResult.Output, &externalOutput); err != nil {
			t.Fatal("decode external provider result:", err)
		}
		if err := json.Unmarshal(internalResult.Output, &internalOutput); err != nil {
			t.Fatal("decode internal provider result:", err)
		}
		if externalOutput != internalOutput {
			t.Fatalf("provider replacement changed operation contract: external=%+v internal=%+v", externalOutput, internalOutput)
		}
		if externalStore.count != 1 || internalStore.count != 1 {
			t.Fatalf("provider replacement effects external=%d internal=%d, want one historical and one new effect", externalStore.count, internalStore.count)
		}
	})

	t.Run("pinned provider cutover preserves historical work and revocation blocks new effects", func(t *testing.T) {
		oldStore, newStore := &fixtureCommandStore{}, &fixtureCommandStore{}
		oldPlan, oldLease, execution := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "billing.invoice.issue", ProviderInternal, "billing-old", oldStore, 10)
		newPlan, newLease, _ := fixtureScenarioBinding(t, scope, "sales.orders", "finance.ar", "billing.invoice.issue", ProviderExternal, "billing-new", newStore, 11)
		payload := `{"source_id":"sale-cutover","amount":"19.00","currency":"KES","entity_id":"entity-a"}`
		if _, err := invokeFixture(t, oldPlan, oldLease, execution, scope, "sales.orders", "billing.invoice.issue", "invoice-cutover", payload); err != nil {
			t.Fatal("pinned old provider work after cutover:", err)
		}
		if oldStore.count != 1 || newStore.count != 0 {
			t.Fatalf("cutover rerouted pinned work: old=%d new=%d", oldStore.count, newStore.count)
		}
		if _, err := invokeFixture(t, newPlan, newLease, execution, scope, "sales.orders", "billing.invoice.issue", "invoice-after-cutover", `{"source_id":"sale-after-cutover","amount":"20.00","currency":"KES","entity_id":"entity-a"}`); err != nil || newStore.count != 1 {
			t.Fatalf("new work after cutover used wrong provider: effects=%d err=%v", newStore.count, err)
		}
		removed, err := ResolveModules(scope, []contract.ModuleDescriptor{{ID: "sales.orders", Version: "1.0.0"}}, nil, nil, 12)
		if err != nil || len(removed.Bindings) != 0 {
			t.Fatalf("removed provider remains selectable: %+v, %v", removed, err)
		}
		if len(oldStore.effects) != 1 {
			t.Fatal("provider removal erased completed historical invoice evidence")
		}
		oldPlan.LeaseAdmission = &operationTestLeaseAdmission{err: contract.NewError(contract.CodePermissionDenied, "fixture capability revoked")}
		if _, err := invokeFixture(t, oldPlan, oldLease, execution, scope, "sales.orders", "billing.invoice.issue", "invoice-after-revocation", `{"source_id":"sale-revoked","amount":"2.00","currency":"KES","entity_id":"entity-a"}`); err == nil {
			t.Fatal("security revocation allowed a new effect through the stale pinned plan")
		}
		if oldStore.count != 1 {
			t.Fatalf("revoked attempt changed historical effects: %d", oldStore.count)
		}
	})
}
