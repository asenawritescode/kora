package engineadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/scheduler"
)

func TestNextSubscriptionBillingDateAnchorsMonthEnd(t *testing.T) {
	start := time.Date(2026, time.January, 31, 0, 0, 0, 0, time.UTC)
	february, err := nextSubscriptionBillingDate(start, start, "Monthly", 1)
	if err != nil || february.Format("2006-01-02") != "2026-02-28" {
		t.Fatalf("February billing date = %s, %v", february.Format("2006-01-02"), err)
	}
	march, err := nextSubscriptionBillingDate(february, start, "Monthly", 1)
	if err != nil || march.Format("2006-01-02") != "2026-03-31" {
		t.Fatalf("March billing date = %s, %v; want anchored month end", march.Format("2006-01-02"), err)
	}
}

func TestScheduledJobHandlersAreOptInPerTenantPackage(t *testing.T) {
	registry := doctype.NewRegistry()
	runtime := engine.TenantJobRuntime{Database: &sql.DB{}, Registry: registry, Kernel: &kernel.Kernel{}}
	if got := ScheduledJobHandlers(runtime); len(got) != 0 {
		t.Fatalf("handlers without subscription doctypes = %#v, want none", got)
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		registry.Register(&doctype.DocType{Name: name})
	}
	got := ScheduledJobHandlers(runtime)
	if got[JobType] == nil || len(got) != 1 {
		t.Fatalf("handlers with subscription doctypes = %#v, want a clear fail-closed handler", got)
	}
	if err := got[JobType](context.Background(), nil); err == nil || !strings.Contains(err.Error(), "billing.invoice capability is unavailable") {
		t.Fatalf("handler without billing provider returned %v; want explicit capability error", err)
	}
	if scheduler.JobType("subscription_invoice") != JobType {
		t.Fatal("invoice handler type does not match its configured job type")
	}
}

func TestScheduledJobHandlersWithBillingNeedsOnlySubscriptionOwnedTables(t *testing.T) {
	registry := doctype.NewRegistry()
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		registry.Register(&doctype.DocType{Name: name})
	}
	runtime := engine.TenantJobRuntime{Database: &sql.DB{}, Registry: registry, Kernel: &kernel.Kernel{}}
	provider := billingProviderStub{}
	if handlers := ScheduledJobHandlersWithBilling(runtime, provider); handlers[JobType] == nil {
		t.Fatalf("external billing handler unexpectedly required local invoice tables: %#v", handlers)
	}
}

func TestScheduledJobHandlersWithModuleBillingFailsClearlyWithoutSelectedBinding(t *testing.T) {
	registry := doctype.NewRegistry()
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		registry.Register(&doctype.DocType{Name: name})
	}
	runtime := engine.TenantJobRuntime{Database: &sql.DB{}, Registry: registry, Kernel: &kernel.Kernel{}, TenantID: "tenant-a"}
	handler := ScheduledJobHandlersWithModuleBilling(runtime)[JobType]
	if handler == nil {
		t.Fatal("missing billing binding should keep a fail-closed scheduler handler registered")
	}
	if err := handler(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "required billing.invoice capability is unavailable") {
		t.Fatalf("handler without selected binding returned %v", err)
	}
}

type billingProviderStub struct{}

func (billingProviderStub) CreateObligation(context.Context, policy.Execution, contract.ScopeContext, BillingObligation) (BillingResult, error) {
	return BillingResult{}, nil
}
func (billingProviderStub) ReconcileObligation(context.Context, policy.Execution, contract.ScopeContext, string) (BillingResult, error) {
	return BillingResult{State: BillingStateNotFound}, nil
}

func TestBillingObligationValidatesProviderContractInputs(t *testing.T) {
	tenantScope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	valid := BillingObligation{Source: contract.BusinessRef{ProviderID: "subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: tenantScope}, BillingPeriod: BillingPeriod{Start: "2026-10-01", End: "2026-10-31"}, Customer: contract.BusinessRef{ProviderID: "parties", ResourceType: "party", ID: "party-1", Scope: tenantScope}, Currency: "USD", Lines: []BillingLine{{Description: "Monthly service", Quantity: "1.00", UnitPrice: "12.50"}}, IdempotencyKey: "tenant-sub-period"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := valid
	bad.Lines = []BillingLine{{Description: "x", Quantity: "NaN", UnitPrice: "1"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("non-decimal quantity accepted")
	}
	bad = valid
	bad.BillingPeriod.End = "2026-09-30"
	if err := bad.Validate(); err == nil {
		t.Fatal("reversed period accepted")
	}
}

type contractBillingProvider struct {
	ledger            map[string]BillingResult
	creates           int
	loseFirstResponse bool
}

func (p *contractBillingProvider) CreateObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	p.creates++
	result := BillingResult{Obligation: contract.BusinessRef{ProviderID: "billing", ResourceType: "obligation", ID: fmt.Sprintf("obl-%d", p.creates), Scope: obligation.Source.Scope}, State: BillingStateAccepted}
	p.ledger[obligation.IdempotencyKey] = result
	if p.loseFirstResponse {
		p.loseFirstResponse = false
		return BillingResult{}, errors.New("provider response lost")
	}
	return result, nil
}

func (p *contractBillingProvider) ReconcileObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, key string) (BillingResult, error) {
	if result, ok := p.ledger[key]; ok {
		return result, nil
	}
	return BillingResult{State: BillingStateNotFound}, nil
}

func TestBillingProviderContractRecoversAmbiguousResultWithoutDuplicate(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: scope}, BillingPeriod: BillingPeriod{Start: "2026-10-01", End: "2026-10-31"}, Customer: contract.BusinessRef{ProviderID: "parties", ResourceType: "party", ID: "party-1", Scope: scope}, Currency: "USD", Lines: []BillingLine{{Description: "Monthly service", Quantity: "1", UnitPrice: "12.50"}}, IdempotencyKey: recurringInvoiceIdempotencyKey("tenant-a", "sub-1", "2026-10-01")}
	for _, adapterName := range []string{"internal", "external"} {
		t.Run(adapterName, func(t *testing.T) {
			fixture := &contractBillingProvider{ledger: map[string]BillingResult{}, loseFirstResponse: true}
			var provider BillingProvider = fixture
			if adapterName == "internal" {
				provider = InternalBillingAdapter{Commands: fixture}
			}
			generator := &recurringInvoiceGenerator{billing: provider}
			scopeContext := contract.ScopeContext{Scope: scope, ProviderID: "subscriptions"}
			if _, err := generator.createOrRecoverBillingObligation(context.Background(), policy.Execution{}, scopeContext, obligation); err == nil {
				t.Fatal("ambiguous create response was treated as accepted")
			}
			result, err := generator.createOrRecoverBillingObligation(context.Background(), policy.Execution{}, scopeContext, obligation)
			if err != nil || result.State != BillingStateAccepted {
				t.Fatalf("retry result = %#v, %v", result, err)
			}
			if fixture.creates != 1 {
				t.Fatalf("provider created %d obligations, want exactly 1", fixture.creates)
			}
		})
	}
}

type durableBillingFixture struct {
	mu               sync.Mutex
	ledger           map[string]BillingResult
	effects          int
	loseNextResponse bool
}

func (p *durableBillingFixture) CreateObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, exists := p.ledger[obligation.IdempotencyKey]; exists {
		return result, nil
	}
	p.effects++
	result := BillingResult{Obligation: contract.BusinessRef{ProviderID: "billing", ResourceType: "obligation", ID: fmt.Sprintf("obl-%d", p.effects), Scope: obligation.Source.Scope}, State: BillingStateAccepted}
	p.ledger[obligation.IdempotencyKey] = result
	if p.loseNextResponse {
		p.loseNextResponse = false
		return BillingResult{}, errors.New("provider response lost after durable effect")
	}
	return result, nil
}

func (p *durableBillingFixture) ReconcileObligation(_ context.Context, _ policy.Execution, _ contract.ScopeContext, key string) (BillingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if result, exists := p.ledger[key]; exists {
		return result, nil
	}
	return BillingResult{State: BillingStateNotFound}, nil
}

func TestBillingProviderConcurrentRetryAfterRestartHasOneEffect(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: scope}, BillingPeriod: BillingPeriod{Start: "2026-10-01", End: "2026-10-31"}, Customer: contract.BusinessRef{ProviderID: "parties", ResourceType: "party", ID: "party-1", Scope: scope}, Currency: "USD", Lines: []BillingLine{{Description: "Monthly service", Quantity: "1", UnitPrice: "12.50"}}, IdempotencyKey: recurringInvoiceIdempotencyKey("tenant-a", "sub-1", "2026-10-01")}
	fixture := &durableBillingFixture{ledger: map[string]BillingResult{}, loseNextResponse: true}
	scopeContext := contract.ScopeContext{Scope: scope, ProviderID: "subscriptions"}
	if _, err := (&recurringInvoiceGenerator{billing: fixture}).createOrRecoverBillingObligation(context.Background(), policy.Execution{}, scopeContext, obligation); err == nil {
		t.Fatal("lost provider response was treated as confirmed")
	}
	// New adapter instances model scheduler restart while sharing only the durable provider ledger.
	const retries = 12
	errorsByRetry := make(chan error, retries)
	var wait sync.WaitGroup
	for i := 0; i < retries; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := (&recurringInvoiceGenerator{billing: fixture}).createOrRecoverBillingObligation(context.Background(), policy.Execution{}, scopeContext, obligation)
			errorsByRetry <- err
		}()
	}
	wait.Wait()
	close(errorsByRetry)
	for err := range errorsByRetry {
		if err != nil {
			t.Errorf("concurrent retry after restart failed: %v", err)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.effects != 1 {
		t.Fatalf("provider committed %d effects, want exactly one", fixture.effects)
	}
}

func TestBillingProviderContractDoesNotCreateWhilePendingOrUnknown(t *testing.T) {
	for _, state := range []string{BillingStatePending, BillingStateUnknown} {
		t.Run(state, func(t *testing.T) {
			provider := &fixedStateBillingProvider{state: state}
			generator := &recurringInvoiceGenerator{billing: provider}
			scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
			obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: scope}, BillingPeriod: BillingPeriod{Start: "2026-10-01", End: "2026-10-31"}, Customer: contract.BusinessRef{ProviderID: "parties", ResourceType: "party", ID: "party-1", Scope: scope}, Currency: "USD", Lines: []BillingLine{{Description: "Service", Quantity: "1", UnitPrice: "1"}}, IdempotencyKey: "key"}
			if _, err := generator.createOrRecoverBillingObligation(context.Background(), policy.Execution{}, contract.ScopeContext{Scope: scope, ProviderID: "subscriptions"}, obligation); err == nil {
				t.Fatal("unresolved obligation was treated as successful")
			}
			if provider.creates != 0 {
				t.Fatalf("created %d obligations while state was %s", provider.creates, state)
			}
		})
	}
}

type fixedStateBillingProvider struct {
	state   string
	creates int
}

func (p *fixedStateBillingProvider) CreateObligation(context.Context, policy.Execution, contract.ScopeContext, BillingObligation) (BillingResult, error) {
	p.creates++
	return BillingResult{State: BillingStateAccepted}, nil
}

func (p *fixedStateBillingProvider) ReconcileObligation(context.Context, policy.Execution, contract.ScopeContext, string) (BillingResult, error) {
	return BillingResult{State: p.state}, nil
}

func TestBillingObligationRejectsNonDecimalAndCrossScopeInputs(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: scope}, BillingPeriod: BillingPeriod{Start: "2026-10-01", End: "2026-10-31"}, Customer: contract.BusinessRef{ProviderID: "parties", ResourceType: "party", ID: "party-1", Scope: scope}, Currency: "USD", Lines: []BillingLine{{Description: "Service", Quantity: "1", UnitPrice: "1.25"}}, IdempotencyKey: "key"}
	for _, mutate := range []func(*BillingObligation){
		func(o *BillingObligation) { o.Lines[0].UnitPrice = "1e3" },
		func(o *BillingObligation) { o.Lines[0].Quantity = "+1" },
		func(o *BillingObligation) { o.Currency = "usd" },
		func(o *BillingObligation) { o.Customer.Scope.TenantID = "tenant-b" },
	} {
		bad := obligation
		bad.Lines = append([]BillingLine(nil), obligation.Lines...)
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatal("invalid billing input accepted")
		}
	}
}

func TestRecurringBillingIdempotencyKeyIncludesTenantSourceAndPeriod(t *testing.T) {
	base := recurringInvoiceIdempotencyKey("tenant-a", "sub-1", "2026-10-01")
	if base != recurringInvoiceIdempotencyKey("tenant-a", "sub-1", "2026-10-01") {
		t.Fatal("same billing period produced a non-deterministic key")
	}
	for _, distinct := range []string{
		recurringInvoiceIdempotencyKey("tenant-b", "sub-1", "2026-10-01"),
		recurringInvoiceIdempotencyKey("tenant-a", "sub-2", "2026-10-01"),
		recurringInvoiceIdempotencyKey("tenant-a", "sub-1", "2026-11-01"),
	} {
		if distinct == base {
			t.Fatal("tenant, source, and period must each distinguish the key")
		}
	}
}

type billingLeaseStoreStub struct {
	leases   map[string]contract.CapabilityLease
	attempts *billingAttemptAdmissionStub
}

func (s billingLeaseStoreStub) LoadCapabilityLease(_ context.Context, tenant contract.TenantID, id string) (contract.CapabilityLease, bool, error) {
	lease, ok := s.leases[id]
	return lease, ok && lease.TenantID == tenant, nil
}

func (s billingLeaseStoreStub) AdmitCapabilityAttempt(ctx context.Context, lease contract.CapabilityLease, now time.Time) error {
	return s.attempts.AdmitCapabilityAttempt(ctx, lease, now)
}

type billingAttemptAdmissionStub struct{ leases []contract.CapabilityLease }

func (s *billingAttemptAdmissionStub) AdmitCapabilityAttempt(_ context.Context, lease contract.CapabilityLease, _ time.Time) error {
	s.leases = append(s.leases, lease)
	return nil
}

type billingCapabilityFactoryStub struct{ handle *billingCapabilityHandleStub }

func (f billingCapabilityFactoryStub) Capability() contract.CapabilityName {
	return BillingCapabilityName
}
func (f billingCapabilityFactoryStub) Open(contract.OperationContext) (contract.CapabilityHandle, error) {
	return f.handle, nil
}

type billingCapabilityHandleStub struct {
	result      BillingResult
	invocations []plugin.CapabilityInvocation
}

func (h *billingCapabilityHandleStub) Name() contract.CapabilityName { return BillingCapabilityName }
func (*billingCapabilityHandleStub) Scope() contract.GrantScope      { return contract.GrantScopeOperation }
func (*billingCapabilityHandleStub) Revoke()                         {}
func (*billingCapabilityHandleStub) Evidence() contract.CleanupEvidence {
	return contract.CleanupEvidence{}
}
func (h *billingCapabilityHandleStub) InvokeCapability(_ context.Context, invocation plugin.CapabilityInvocation) (plugin.CapabilityInvocationResult, error) {
	h.invocations = append(h.invocations, invocation)
	output, err := json.Marshal(h.result)
	return plugin.CapabilityInvocationResult{ExecutionID: "billing-operation-1", Output: output}, err
}

func TestCapabilityBillingAdapterUsesSelectedBindingAndDurableOperationLeases(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}
	inputSchema := contract.SchemaRef{ID: "billing.obligation.input", Version: 1}
	outputSchema := contract.SchemaRef{ID: "billing.obligation.output", Version: 1}
	operations := []contract.OperationSpec{
		{Name: BillingCreateOperation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectExternal},
		{Name: BillingReconcileOperation, Input: inputSchema, Output: outputSchema, Effect: contract.EffectExternal},
	}
	offer := contract.CapabilityOffer{Name: BillingCapabilityName, Contract: outputSchema, Operations: operations}
	consumer := contract.ModuleDescriptor{ID: "subscriptions.core", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: BillingCapabilityName, Contract: outputSchema, Required: true}}}
	owner := contract.ModuleDescriptor{ID: "billing.ar", Version: "1.0.0", Provides: []contract.CapabilityOffer{offer}}
	handle := &billingCapabilityHandleStub{result: BillingResult{Obligation: contract.BusinessRef{ProviderID: "fixture.billing", ResourceType: "obligation", ID: "obl-1", Scope: scope}, State: BillingStateAccepted}}
	factory := billingCapabilityFactoryStub{handle: handle}
	provider := plugin.ProviderRegistration{TenantID: scope.TenantID, Scope: scope, ProviderID: "billing.external", OwnerModuleID: owner.ID, Kind: plugin.ProviderExternal, Offers: []contract.CapabilityOffer{offer}, Factories: map[contract.CapabilityName]plugin.ProviderFactory{BillingCapabilityName: factory}, ValidateSchema: func(contract.SchemaRef, []byte) error { return nil }}
	plan, err := plugin.ResolveModules(scope, []contract.ModuleDescriptor{consumer, owner}, []plugin.ProviderRegistration{provider}, nil, 7)
	if err != nil {
		t.Fatal("resolve selected billing provider:", err)
	}
	attempts := &billingAttemptAdmissionStub{}
	plan.LeaseAdmission = attempts
	selected, ok := plan.Resolve(plugin.BindingKey{Scope: scope, ConsumerModuleID: consumer.ID, CapabilityName: BillingCapabilityName})
	if !ok {
		t.Fatal("resolved binding is missing")
	}
	leases := make(map[string]contract.CapabilityLease)
	for _, operation := range []string{BillingCreateOperation, BillingReconcileOperation} {
		leaseID, err := BillingCapabilityLeaseID(scope, selected, operation)
		if err != nil {
			t.Fatal("derive durable billing capability lease ID:", err)
		}
		lease := contract.CapabilityLease{ID: leaseID, TenantID: scope.TenantID, Scope: scope, CallerModuleID: consumer.ID, ProviderModuleID: owner.ID, ProviderID: selected.ProviderID, Operation: operation, BindingRevision: selected.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "active"}
		leases[lease.ID] = lease
	}
	plan.LeaseAdmission = billingLeaseStoreStub{leases: leases, attempts: attempts}
	runtime := engine.TenantJobRuntime{TenantID: string(scope.TenantID), ModuleBindings: &plan}
	adapter, err := NewCapabilityBillingAdapter(runtime)
	if err != nil {
		t.Fatal("construct billing capability adapter:", err)
	}
	execution, err := policy.NewVerifiedExecution(scope.TenantID, contract.ActorContext{PrincipalID: "scheduler", PrincipalType: contract.PrincipalService, Site: string(scope.TenantID), AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal("construct scheduler execution:", err)
	}
	obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "kora.subscriptions", ResourceType: "subscription", ID: "sub-1", Scope: scope}, BillingPeriod: BillingPeriod{Start: "2026-11-01", End: "2026-11-30"}, Customer: contract.BusinessRef{ProviderID: "kora.parties", ResourceType: "party", ID: "customer-1", Scope: scope}, Currency: "USD", Lines: []BillingLine{{Description: "Subscription sub-1", Quantity: "1", UnitPrice: "20.00"}}, IdempotencyKey: recurringInvoiceIdempotencyKey(string(scope.TenantID), "sub-1", "2026-11-01")}
	got, err := adapter.CreateObligation(context.Background(), execution, contract.ScopeContext{Scope: scope, ProviderID: "kora.subscriptions"}, obligation)
	if err != nil || got.State != BillingStateAccepted {
		t.Fatalf("capability create result = %#v, %v", got, err)
	}
	if len(handle.invocations) != 1 || handle.invocations[0].Operation.Name != BillingCreateOperation || handle.invocations[0].Meta.IdempotencyKey != contract.IdempotencyKey(obligation.IdempotencyKey) {
		t.Fatalf("capability invocation = %#v", handle.invocations)
	}
	if len(attempts.leases) != 1 || attempts.leases[0].Operation != BillingCreateOperation {
		t.Fatalf("durable lease admission = %#v", attempts.leases)
	}
	createLease := leases[adapter.Create.ID]
	createLease.State = "revoked"
	leases[adapter.Create.ID] = createLease
	if _, err := adapter.CreateObligation(context.Background(), execution, contract.ScopeContext{Scope: scope, ProviderID: "kora.subscriptions"}, obligation); err == nil {
		t.Fatal("billing adapter reused an in-memory lease after durable revocation")
	}
	if len(handle.invocations) != 1 {
		t.Fatalf("revoked billing lease reached provider; invocations=%d", len(handle.invocations))
	}
}
