package engineadapter

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/scheduler"
)

const recurringInvoiceCatchUpLimit = 24

const JobType scheduler.JobType = "subscription_invoice"

const (
	BillingCapabilityName     contract.CapabilityName = "billing.invoice"
	BillingCreateOperation                            = "create_obligation@1"
	BillingReconcileOperation                         = "reconcile_obligation@1"
)

type recurringInvoiceGenerator struct {
	database       *sql.DB
	registry       *doctype.Registry
	dialect        kdb.Dialect
	kernel         *kernel.Kernel
	tenantID       string
	siteName       string
	configRevision uint64
	billing        BillingProvider
}

type BillingObligation struct {
	Source         contract.BusinessRef `json:"source"`
	BillingPeriod  BillingPeriod        `json:"billingPeriod"`
	Customer       contract.BusinessRef `json:"customer"`
	Currency       string               `json:"currency"`
	Lines          []BillingLine        `json:"lines"`
	IdempotencyKey string               `json:"idempotencyKey"`
}

type BillingPeriod struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type BillingLine struct {
	Description string `json:"description"`
	Quantity    string `json:"quantityDecimal"`
	UnitPrice   string `json:"unitPriceDecimal"`
}
type BillingResult struct {
	Obligation        contract.BusinessRef `json:"obligation"`
	State             string               `json:"state"`
	ExternalReference string               `json:"externalReference,omitempty"`
}

const (
	BillingStateAccepted = "accepted"
	BillingStateIssued   = "issued"
	BillingStatePending  = "pending"
	BillingStateUnknown  = "unknown"
	BillingStateNotFound = "not_found"
)

type BillingProvider interface {
	// CreateObligation must durably deduplicate by obligation.IdempotencyKey.
	// Repeating the same key must return the same obligation identity and must
	// never create a second business effect.
	CreateObligation(context.Context, policy.Execution, contract.ScopeContext, BillingObligation) (BillingResult, error)
	// ReconcileObligation may return not_found only when no effect for key can
	// still commit. pending/unknown must remain unresolved until authoritative.
	ReconcileObligation(context.Context, policy.Execution, contract.ScopeContext, string) (BillingResult, error)
}

// BillingOwnerCommands is the owner module's command boundary for internal
// billing. The adapter carries no invoice tables or accounting rules; owner
// commands must enforce durable uniqueness on the tenant-scoped idempotency
// key and expose authoritative reconciliation for that key.
type BillingOwnerCommands interface {
	CreateObligation(context.Context, policy.Execution, contract.ScopeContext, BillingObligation) (BillingResult, error)
	ReconcileObligation(context.Context, policy.Execution, contract.ScopeContext, string) (BillingResult, error)
}

type InternalBillingAdapter struct{ Commands BillingOwnerCommands }

func (a InternalBillingAdapter) CreateObligation(ctx context.Context, execution policy.Execution, scope contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	if a.Commands == nil {
		return BillingResult{}, fmt.Errorf("internal billing owner commands are unavailable")
	}
	if err := obligation.Validate(); err != nil {
		return BillingResult{}, err
	}
	return a.Commands.CreateObligation(ctx, execution, scope, obligation)
}

func (a InternalBillingAdapter) ReconcileObligation(ctx context.Context, execution policy.Execution, scope contract.ScopeContext, key string) (BillingResult, error) {
	if a.Commands == nil || key == "" || len(key) > 160 {
		return BillingResult{}, fmt.Errorf("internal billing reconciliation identity is invalid")
	}
	return a.Commands.ReconcileObligation(ctx, execution, scope, key)
}

// CapabilityBillingAdapter invokes the provider selected in the tenant's
// immutable module BindingPlan. Its durable leases fence each effect attempt.
type CapabilityBillingAdapter struct {
	Plan       plugin.BindingPlan
	Caller     contract.ModuleID
	Scope      contract.BusinessScope
	LeaseStore plugin.CapabilityLeaseReader
	Create     contract.CapabilityLease
	Reconcile  contract.CapabilityLease
}

func NewCapabilityBillingAdapter(runtime engine.TenantJobRuntime) (*CapabilityBillingAdapter, error) {
	if runtime.ModuleBindings == nil {
		return nil, fmt.Errorf("billing.invoice module binding or durable lease owner is unavailable")
	}
	leaseStore, ok := runtime.ModuleBindings.LeaseAdmission.(plugin.CapabilityLeaseReader)
	if !ok {
		return nil, fmt.Errorf("billing.invoice durable lease lookup is unavailable")
	}
	tenant, err := contract.ParseTenantID(runtime.TenantID)
	if err != nil {
		return nil, fmt.Errorf("billing capability tenant identity: %w", err)
	}
	scope := contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
	var subscriptionOwner contract.ModuleID
	if runtime.Registry != nil {
		if subscription := runtime.Registry.Get("Subscription"); subscription != nil {
			subscriptionOwner = contract.ModuleID(subscription.ModuleOwnerID)
		}
	}
	var selected *plugin.ResolvedBinding
	for index := range runtime.ModuleBindings.Bindings {
		binding := &runtime.ModuleBindings.Bindings[index]
		if binding.Key.Scope != scope || binding.Key.CapabilityName != BillingCapabilityName {
			continue
		}
		if subscriptionOwner != "" && binding.Key.ConsumerModuleID != subscriptionOwner {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("billing.invoice has multiple consumer bindings for the tenant")
		}
		selected = binding
	}
	if selected == nil || selected.Revision != runtime.ModuleBindings.Revision || selected.OwnerModuleID == "" || selected.ProviderID == "" {
		return nil, fmt.Errorf("selected billing.invoice binding is unavailable or unpinned")
	}
	if !hasCapabilityOperation(selected.Operations, BillingCreateOperation) || !hasCapabilityOperation(selected.Operations, BillingReconcileOperation) {
		return nil, fmt.Errorf("billing.invoice provider must implement %s and %s", BillingCreateOperation, BillingReconcileOperation)
	}
	createLease, err := loadBillingCapabilityLease(context.Background(), leaseStore, scope, selected, BillingCreateOperation)
	if err != nil {
		return nil, err
	}
	reconcileLease, err := loadBillingCapabilityLease(context.Background(), leaseStore, scope, selected, BillingReconcileOperation)
	if err != nil {
		return nil, err
	}
	return &CapabilityBillingAdapter{Plan: *runtime.ModuleBindings, Caller: selected.Key.ConsumerModuleID, Scope: scope, LeaseStore: leaseStore, Create: createLease, Reconcile: reconcileLease}, nil
}

func hasCapabilityOperation(operations []contract.OperationSpec, name string) bool {
	for _, operation := range operations {
		if operation.Name == name && (operation.Effect == contract.EffectLocal || operation.Effect == contract.EffectExternal) {
			return true
		}
	}
	return false
}

// BillingCapabilityLeaseID returns the stable ID used to provision the
// scheduler grant through the tenant's existing capability lease store.
func BillingCapabilityLeaseID(scope contract.BusinessScope, binding plugin.ResolvedBinding, operation string) (string, error) {
	if scope.Validate() != nil || binding.Key.Scope != scope || binding.Key.ConsumerModuleID == "" || binding.Key.CapabilityName != BillingCapabilityName || binding.ProviderID == "" || binding.OwnerModuleID == "" || binding.Revision == 0 || operation != BillingCreateOperation && operation != BillingReconcileOperation {
		return "", fmt.Errorf("billing capability lease identity is invalid")
	}
	identity := string(scope.TenantID) + "\x00" + string(binding.Key.ConsumerModuleID) + "\x00" + binding.ProviderID + "\x00" + fmt.Sprint(binding.Revision) + "\x00" + operation
	digest := sha256.Sum256([]byte(identity))
	return "billing-scheduler-" + hex.EncodeToString(digest[:]), nil
}

func loadBillingCapabilityLease(ctx context.Context, store plugin.CapabilityLeaseReader, scope contract.BusinessScope, binding *plugin.ResolvedBinding, operation string) (contract.CapabilityLease, error) {
	leaseID, err := BillingCapabilityLeaseID(scope, *binding, operation)
	if err != nil {
		return contract.CapabilityLease{}, err
	}
	lease, found, err := store.LoadCapabilityLease(ctx, scope.TenantID, leaseID)
	if err != nil {
		return contract.CapabilityLease{}, fmt.Errorf("load durable billing capability lease: %w", err)
	}
	if !found || lease.State != "active" || lease.Scope != scope || lease.CallerModuleID != binding.Key.ConsumerModuleID || lease.ProviderModuleID != binding.OwnerModuleID || lease.ProviderID != binding.ProviderID || lease.Operation != operation || lease.BindingRevision != binding.Revision {
		return contract.CapabilityLease{}, contract.NewError(contract.CodePermissionDenied, "active durable billing capability grant is unavailable")
	}
	return lease, nil
}

func (a *CapabilityBillingAdapter) CreateObligation(ctx context.Context, execution policy.Execution, scope contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	if scope.Scope != a.Scope || scope.ProviderID == "" || scope.ProviderID != obligation.Source.ProviderID {
		return BillingResult{}, fmt.Errorf("billing capability invocation scope does not match selected binding")
	}
	payload, err := json.Marshal(obligation)
	if err != nil {
		return BillingResult{}, err
	}
	return a.invoke(ctx, execution, a.Create, BillingCreateOperation, obligation.IdempotencyKey, payload)
}

func (a *CapabilityBillingAdapter) ReconcileObligation(ctx context.Context, execution policy.Execution, scope contract.ScopeContext, key string) (BillingResult, error) {
	if scope.Scope != a.Scope || scope.ProviderID == "" || key == "" {
		return BillingResult{}, fmt.Errorf("billing reconciliation scope or identity is invalid")
	}
	payload, err := json.Marshal(struct {
		IdempotencyKey string `json:"idempotencyKey"`
	}{IdempotencyKey: key})
	if err != nil {
		return BillingResult{}, err
	}
	digest := sha256.Sum256([]byte(key))
	return a.invoke(ctx, execution, a.Reconcile, BillingReconcileOperation, "billing-reconcile-"+hex.EncodeToString(digest[:]), payload)
}

func (a *CapabilityBillingAdapter) invoke(ctx context.Context, execution policy.Execution, lease contract.CapabilityLease, operation, idempotencyKey string, payload []byte) (BillingResult, error) {
	if a == nil || a.LeaseStore == nil {
		return BillingResult{}, fmt.Errorf("billing capability durable lease admission is unavailable")
	}
	current, found, err := a.LeaseStore.LoadCapabilityLease(ctx, lease.TenantID, lease.ID)
	if err != nil || !found || current != lease {
		return BillingResult{}, contract.NewError(contract.CodePermissionDenied, "durable billing capability grant changed or is unavailable")
	}
	result, err := a.Plan.InvokeCapability(ctx, execution, lease, a.Scope, a.Caller, BillingCapabilityName, operation, contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey(idempotencyKey)}, payload)
	if err != nil {
		return BillingResult{}, err
	}
	var billingResult BillingResult
	if err := json.Unmarshal(result.Output, &billingResult); err != nil {
		return BillingResult{}, fmt.Errorf("decode billing capability result: %w", err)
	}
	return billingResult, nil
}

func (o BillingObligation) Validate() error {
	if !validBillingReference(o.Source) || !validBillingReference(o.Customer) || o.Source.Scope != o.Customer.Scope || !validBillingCurrency(o.Currency) || o.IdempotencyKey == "" || len(o.IdempotencyKey) > 160 || len(o.Lines) == 0 {
		return fmt.Errorf("billing obligation identity, customer, currency, lines and idempotency key are required")
	}
	start, err := time.Parse("2006-01-02", o.BillingPeriod.Start)
	if err != nil {
		return fmt.Errorf("invalid billing period start")
	}
	end, err := time.Parse("2006-01-02", o.BillingPeriod.End)
	if err != nil || end.Before(start) {
		return fmt.Errorf("invalid billing period end")
	}
	if len(o.Lines) > 100 {
		return fmt.Errorf("billing obligation has too many lines")
	}
	for _, line := range o.Lines {
		quantity, err := parseBillingDecimal(line.Quantity)
		if err != nil || quantity.Sign() <= 0 {
			return fmt.Errorf("billing quantity must be a positive decimal")
		}
		price, err := parseBillingDecimal(line.UnitPrice)
		if err != nil || price.Sign() < 0 {
			return fmt.Errorf("billing unit price must be a nonnegative decimal")
		}
		if line.Description == "" || len(line.Description) > 500 {
			return fmt.Errorf("billing line description is invalid")
		}
	}
	encoded, err := json.Marshal(o)
	if err != nil || len(encoded) > 64<<10 {
		return fmt.Errorf("billing obligation exceeds the bounded payload size")
	}
	return nil
}

func validBillingReference(reference contract.BusinessRef) bool {
	return reference.Scope.Validate() == nil && reference.ProviderID != "" && len(reference.ProviderID) <= 160 &&
		reference.ResourceType != "" && len(reference.ResourceType) <= 160 && reference.ID != "" && len(reference.ID) <= 200
}

func validBillingCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, character := range currency {
		if character < 'A' || character > 'Z' {
			return false
		}
	}
	return true
}

func parseBillingDecimal(value string) (*big.Rat, error) {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value {
		return nil, fmt.Errorf("decimal string is empty, untrimmed, or too long")
	}
	index := 0
	if value[0] == '-' {
		index++
	}
	if index == len(value) {
		return nil, fmt.Errorf("decimal has no digits")
	}
	seenPoint, seenDigit := false, false
	for ; index < len(value); index++ {
		character := value[index]
		if character >= '0' && character <= '9' {
			seenDigit = true
			continue
		}
		if character == '.' && !seenPoint {
			seenPoint = true
			continue
		}
		return nil, fmt.Errorf("decimal must use plain base-10 notation")
	}
	if !seenDigit || value[len(value)-1] == '.' {
		return nil, fmt.Errorf("decimal has an invalid fractional part")
	}
	parsed, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("decimal could not be parsed")
	}
	return parsed, nil
}

func (g *recurringInvoiceGenerator) createOrRecoverBillingObligation(ctx context.Context, execution policy.Execution, scope contract.ScopeContext, obligation BillingObligation) (BillingResult, error) {
	if g == nil || g.billing == nil {
		return BillingResult{}, fmt.Errorf("billing.invoice capability is unavailable")
	}
	if err := obligation.Validate(); err != nil {
		return BillingResult{}, err
	}
	if ctx == nil || scope.Scope.Validate() != nil || scope.Scope != obligation.Source.Scope || scope.ProviderID != obligation.Source.ProviderID {
		return BillingResult{}, fmt.Errorf("billing capability invocation scope does not match the source obligation")
	}
	result, err := g.billing.ReconcileObligation(ctx, execution, scope, obligation.IdempotencyKey)
	if err != nil {
		return BillingResult{}, fmt.Errorf("reconcile billing obligation before retry: %w", err)
	}
	switch result.State {
	case BillingStateAccepted, BillingStateIssued:
		return validateBillingResult(result, obligation)
	case BillingStateNotFound:
		// Only a definitive not-found permits starting a new provider effect.
	case BillingStatePending, BillingStateUnknown:
		return result, fmt.Errorf("billing obligation %s remains %s and requires reconciliation", obligation.IdempotencyKey, result.State)
	default:
		return result, fmt.Errorf("billing reconciliation returned unsupported state %q", result.State)
	}
	result, err = g.billing.CreateObligation(ctx, execution, scope, obligation)
	if err != nil {
		return BillingResult{}, fmt.Errorf("create billing obligation: %w", err)
	}
	switch result.State {
	case BillingStateAccepted, BillingStateIssued:
		return validateBillingResult(result, obligation)
	case BillingStatePending, BillingStateUnknown:
		return result, fmt.Errorf("billing obligation %s remains %s and requires reconciliation", obligation.IdempotencyKey, result.State)
	default:
		return result, fmt.Errorf("billing provider returned unsupported state %q", result.State)
	}
}

func validateBillingResult(result BillingResult, obligation BillingObligation) (BillingResult, error) {
	if !validBillingReference(result.Obligation) || result.Obligation.Scope != obligation.Source.Scope || len(result.ExternalReference) > 256 {
		return BillingResult{}, fmt.Errorf("billing provider returned an invalid or cross-scope obligation reference")
	}
	return result, nil
}

// ScheduledJobHandlers registers the job for the subscription package while
// leaving billing capability availability to the application composition.
func ScheduledJobHandlers(runtime engine.TenantJobRuntime) map[scheduler.JobType]scheduler.JobHandler {
	if runtime.Database == nil || runtime.Registry == nil || runtime.Kernel == nil {
		return nil
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		if runtime.Registry.Get(name) == nil {
			return nil
		}
	}
	generator := &recurringInvoiceGenerator{
		database: runtime.Database, registry: runtime.Registry, dialect: runtime.Dialect,
		kernel: runtime.Kernel, tenantID: runtime.TenantID, siteName: runtime.SiteName,
		configRevision: runtime.ConfigRevision,
	}
	return map[scheduler.JobType]scheduler.JobHandler{JobType: func(ctx context.Context, job *scheduler.JobConfig) error {
		var config map[string]any
		if job != nil {
			config = job.Config
		}
		return generator.GenerateDueInvoices(ctx, config)
	}}
}

func ScheduledJobHandlersWithBilling(runtime engine.TenantJobRuntime, provider BillingProvider) map[scheduler.JobType]scheduler.JobHandler {
	if provider == nil {
		return nil
	}
	if runtime.Database == nil || runtime.Registry == nil {
		return nil
	}
	for _, name := range []string{"Subscription", "Plan", "Subscription Line"} {
		if runtime.Registry.Get(name) == nil {
			return nil
		}
	}
	generator := &recurringInvoiceGenerator{database: runtime.Database, registry: runtime.Registry, dialect: runtime.Dialect, kernel: runtime.Kernel, tenantID: runtime.TenantID, siteName: runtime.SiteName, configRevision: runtime.ConfigRevision, billing: provider}
	return map[scheduler.JobType]scheduler.JobHandler{JobType: func(ctx context.Context, job *scheduler.JobConfig) error {
		var config map[string]any
		if job != nil {
			config = job.Config
		}
		return generator.GenerateDueInvoices(ctx, config)
	}}
}

// ScheduledJobHandlersWithModuleBilling adapts the tenant's selected provider
// binding into the subscription billing port for this scheduler generation.
func ScheduledJobHandlersWithModuleBilling(runtime engine.TenantJobRuntime) map[scheduler.JobType]scheduler.JobHandler {
	provider, err := NewCapabilityBillingAdapter(runtime)
	if err == nil {
		return ScheduledJobHandlersWithBilling(runtime, provider)
	}
	handlers := ScheduledJobHandlers(runtime)
	if handlers[JobType] == nil {
		return nil
	}
	handlers[JobType] = func(context.Context, *scheduler.JobConfig) error {
		return fmt.Errorf("required billing.invoice capability is unavailable: %w", err)
	}
	return handlers
}

func (g *recurringInvoiceGenerator) GenerateDueInvoices(ctx context.Context, config map[string]any) error {
	_ = config // billing due-date policy belongs to the selected billing provider.
	if ctx == nil || g == nil || g.database == nil || g.registry == nil || g.kernel == nil {
		return errors.New("recurring invoice generator is not fully configured")
	}
	if g.billing == nil {
		return errors.New("required billing.invoice capability is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tenantID, err := contract.ParseTenantID(g.tenantID)
	if err != nil {
		return fmt.Errorf("recurring invoice tenant ID: %w", err)
	}
	tx := &orm.TxManager{DB: g.database, Registry: g.registry, Dialect: g.dialect, SiteName: g.siteName}
	actor := contract.ActorContext{
		PrincipalID: "kora:recurring-invoice-scheduler", PrincipalType: contract.PrincipalService,
		Site: g.siteName, AuthenticatedAt: time.Now().UTC(),
	}
	execution, err := policy.NewVerifiedExecutionWithGrants(tenantID, actor, []policy.Grant{
		{Resource: "Subscription", Operation: "read"},
		{Resource: "Plan", Operation: "read"},
		{Resource: "Subscription Line", Operation: "read"},
		{Resource: "Subscription", Operation: "write"},
	})
	if err != nil {
		return fmt.Errorf("build invoice worker identity: %w", err)
	}
	date := time.Now().UTC().Format("2006-01-02")
	filters, _ := json.Marshal([][]any{{"status", "=", "Active"}, {"next_billing_date", "<=", date}})
	subscriptionType := g.registry.Get("Subscription")
	if subscriptionType == nil {
		return errors.New("Subscription doctype is not installed")
	}
	var cursor string
	var generationErrors []error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageFilters := string(filters)
		if cursor != "" {
			var terms [][]any
			if err := json.Unmarshal(filters, &terms); err != nil {
				return err
			}
			terms = append(terms, []any{"name", ">", cursor})
			page, _ := json.Marshal(terms)
			pageFilters = string(page)
		}
		subscriptions, _, err := tx.GetListWithOptions(subscriptionType, pageFilters, "name ASC", 200, 0, "", nil, "")
		if err != nil {
			return fmt.Errorf("list due subscriptions: %w", err)
		}
		if len(subscriptions) == 0 {
			return errors.Join(generationErrors...)
		}
		for _, listed := range subscriptions {
			if listed == nil || listed.Name == "" {
				continue
			}
			cursor = listed.Name
			if err := g.generateForSubscription(ctx, tx, execution, listed.Name, date); err != nil {
				// A later run retries this due period. The key is deterministic and
				// the schedule reference is unique, so a committed retry is safe.
				slog.Error("recurring invoice generation failed", "tenant", g.siteName, "subscription", listed.Name, "error", err)
				generationErrors = append(generationErrors, fmt.Errorf("subscription %s: %w", listed.Name, err))
			}
		}
		if len(subscriptions) < 200 {
			return errors.Join(generationErrors...)
		}
	}
}

func (g *recurringInvoiceGenerator) generateForSubscription(ctx context.Context, tx *orm.TxManager, execution policy.Execution, name, today string) error {
	subscriptionType := g.registry.Get("Subscription")
	subscription, err := tx.GetDoc(subscriptionType, name, "")
	if err != nil {
		return fmt.Errorf("load subscription: %w", err)
	}
	for generated := 0; generated < recurringInvoiceCatchUpLimit; generated++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		billingDate := subscription.GetString("next_billing_date")
		if billingDate == "" || billingDate > today {
			return nil
		}
		start, err := time.Parse("2006-01-02", subscription.GetString("start_date"))
		if err != nil {
			return fmt.Errorf("invalid subscription start date: %w", err)
		}
		due, err := time.Parse("2006-01-02", billingDate)
		if err != nil {
			return fmt.Errorf("invalid next billing date: %w", err)
		}
		if endDate := subscription.GetString("end_date"); endDate != "" && endDate < billingDate {
			return nil // Do not bill beyond the configured subscription term.
		}
		next, err := nextSubscriptionBillingDate(due, start, subscription.GetString("billing_interval"), int(subscription.GetFloat("interval_count")))
		if err != nil {
			return err
		}
		_, _, amount, err := g.subscriptionTotals(tx, subscription)
		if err != nil {
			return err
		}
		periodEnd := next.AddDate(0, 0, -1).Format("2006-01-02")
		reference := subscription.Name + "-" + due.Format("20060102")
		scope := tx.BusinessScope
		if scope.TenantID == "" && scope.Kind == "" && scope.LegalEntityID == "" {
			scope = contract.BusinessScope{TenantID: contract.TenantID(g.tenantID), Kind: contract.ScopeKindTenant}
		}
		key := recurringInvoiceIdempotencyKey(g.tenantID, subscription.Name, billingDate)
		obligation := BillingObligation{Source: contract.BusinessRef{ProviderID: "kora.subscriptions", ResourceType: "subscription", ID: subscription.Name, Scope: scope}, BillingPeriod: BillingPeriod{Start: billingDate, End: periodEnd}, Customer: contract.BusinessRef{ProviderID: "kora.parties", ResourceType: "party", ID: subscription.GetString("customer"), Scope: scope}, Currency: subscription.GetString("currency"), Lines: []BillingLine{{Description: "Subscription " + subscription.Name, Quantity: "1", UnitPrice: strconv.FormatFloat(amount, 'f', -1, 64)}}, IdempotencyKey: key}
		scopeContext := contract.ScopeContext{Scope: scope, ProviderID: "kora.subscriptions"}
		_, err = g.createOrRecoverBillingObligation(ctx, execution, scopeContext, obligation)
		if err != nil {
			return fmt.Errorf("create billing obligation %s: %w", reference, err)
		}
		advance, err := json.Marshal(map[string]any{"next_billing_date": next.Format("2006-01-02")})
		if err != nil {
			return err
		}
		update, err := json.Marshal(kernel.RecordUpdatePayload{Doctype: "Subscription", Name: subscription.Name, Data: advance})
		if err != nil {
			return err
		}
		operation := kernel.Operation{Command: kernel.CommandRecordUpdate, Payload: update, Context: kernel.OperationContext{Site: g.siteName, TenantID: execution.Tenant(), Execution: execution, Source: kernel.SourceWorker, User: "system", IdempotencyKey: key + "-schedule", ConfigRevision: g.configRevision}}
		commandResult, commandErr := executeScheduleAdvance(ctx, g.kernel, g.database, g.registry, operation)
		if commandErr != nil {
			return fmt.Errorf("advance subscription after accepted billing: %s", commandErr.Message)
		}
		if commandResult.Error != nil {
			return fmt.Errorf("advance subscription after accepted billing: %s", commandResult.Error.Message)
		}
		subscription, err = tx.GetDoc(subscriptionType, name, "")
		if err != nil {
			return fmt.Errorf("reload advanced subscription: %w", err)
		}
		continue
	}
	return nil
}

func executeScheduleAdvance(ctx context.Context, commandKernel *kernel.Kernel, database *sql.DB, registry *doctype.Registry, operation kernel.Operation) (contract.CommandResult, *contract.Error) {
	const attempts = 4
	var result contract.CommandResult
	var commandErr *contract.Error
	for attempt := 0; attempt < attempts; attempt++ {
		result, commandErr = commandKernel.Execute(ctx, database, registry, operation)
		retry := commandErr != nil && commandErr.Type == contract.CodeConflict || result.Error != nil && result.Error.Type == contract.CodeConflict
		if !retry || attempt == attempts-1 {
			return result, commandErr
		}
		delay := time.Duration(attempt+1) * 10 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, contract.NewError(contract.CodeDeadlineExceeded, ctx.Err().Error())
		case <-timer.C:
		}
	}
	return result, commandErr
}

func (g *recurringInvoiceGenerator) subscriptionTotals(tx *orm.TxManager, subscription *doctype.Document) (subtotal, taxAmount, total float64, err error) {
	quantity := subscription.GetFloat("quantity")
	if quantity == 0 {
		quantity = 1
	}
	subtotal = subscription.GetFloat("unit_price") * quantity
	lineType := g.registry.Get("Subscription Line")
	if lineType != nil {
		filters, _ := json.Marshal([][]any{{"subscription", "=", subscription.Name}, {"status", "=", "Active"}})
		lines, total, err := tx.GetListWithOptions(lineType, string(filters), "name ASC", 501, 0, "", []string{"quantity", "unit_price"}, "")
		if err != nil {
			return 0, 0, 0, fmt.Errorf("list active subscription lines: %w", err)
		}
		if total > 500 {
			return 0, 0, 0, fmt.Errorf("subscription %s has more than 500 active lines", subscription.Name)
		}
		for _, line := range lines {
			quantity := line.GetFloat("quantity")
			if quantity == 0 {
				quantity = 1
			}
			subtotal += line.GetFloat("unit_price") * quantity
		}
	}
	var taxRate float64
	planType := g.registry.Get("Plan")
	if planType != nil && subscription.GetString("plan") != "" {
		plan, err := tx.GetDoc(planType, subscription.GetString("plan"), "")
		if err != nil {
			return 0, 0, 0, fmt.Errorf("load subscription plan: %w", err)
		}
		taxRate = plan.GetFloat("tax_rate")
	}
	subtotal = math.Round(subtotal*100) / 100
	taxAmount = math.Round(subtotal*taxRate) / 100
	total = math.Round((subtotal+taxAmount)*100) / 100
	return subtotal, taxAmount, total, nil
}

func recurringInvoiceIdempotencyKey(tenant, subscription, billingDate string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + subscription + "\x00" + billingDate))
	return "recurring-invoice-" + hex.EncodeToString(digest[:])
}

func nextSubscriptionBillingDate(current, start time.Time, interval string, count int) (time.Time, error) {
	if count < 1 {
		return time.Time{}, errors.New("subscription interval_count must be positive")
	}
	switch interval {
	case "Weekly":
		return current.AddDate(0, 0, 7*count), nil
	case "Monthly", "Quarterly", "Annually":
		months := count
		if interval == "Quarterly" {
			months *= 3
		} else if interval == "Annually" {
			months *= 12
		}
		monthOffset := (current.Year()-start.Year())*12 + int(current.Month()-start.Month()) + months
		firstOfTarget := time.Date(start.Year(), start.Month()+time.Month(monthOffset), 1, 0, 0, 0, 0, time.UTC)
		lastDay := time.Date(firstOfTarget.Year(), firstOfTarget.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		day := start.Day()
		if day > lastDay {
			day = lastDay
		}
		return time.Date(firstOfTarget.Year(), firstOfTarget.Month(), day, 0, 0, 0, 0, time.UTC), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported subscription billing interval %q", interval)
	}
}
