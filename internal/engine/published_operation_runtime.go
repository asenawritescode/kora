package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/plugin"
	"github.com/asenawritescode/kora/internal/policy"
)

type moduleBindingResolver func(contract.BusinessScope) (plugin.BindingPlan, error)

// publishedOperationRuntime holds the tenant dependencies shared by every
// published-operation entry point. lookupPublishedOperation does not use
// registry, and an omitted resolve only skips external reconciliation there.
type publishedOperationRuntime struct {
	store           *configstore.ModuleStore
	resolve         moduleBindingResolver
	commandKernel   *kernel.Kernel
	commandRegistry *kernel.CommandRegistry
	registry        *doctype.Registry
}

// publishedOperationTarget names the operation and the verified caller acting on it.
type publishedOperationTarget struct {
	execution policy.Execution
	scope     contract.BusinessScope
	owner     contract.ModuleID
	published contract.PublishedOperation
}

// publishedOperationInvocation is the request-specific payload of one execution.
type publishedOperationInvocation struct {
	inputSchema contract.SchemaRef
	meta        contract.CommandMeta
	input       []byte
}

// localRecovery is the stored attempt a local command may already have completed.
type localRecovery struct {
	module    contract.ModuleDescriptor
	intentKey contract.IdempotencyKey
	revision  uint64
	attempt   configstore.PublishedOperationAttempt
}

// operationReceipts records the outcome of a published operation attempt on
// behalf of one verified actor.
type operationReceipts struct {
	store *configstore.ModuleStore
	actor contract.ActorContext
}

func (rt publishedOperationRuntime) receipts(actor contract.ActorContext) operationReceipts {
	return operationReceipts{store: rt.store, actor: actor}
}

func executePublishedOperation(ctx context.Context, rt publishedOperationRuntime, target publishedOperationTarget, call publishedOperationInvocation) (contract.OperationReceipt, error) {
	if ctx == nil || rt.store == nil || rt.resolve == nil || rt.commandKernel == nil || rt.commandRegistry == nil || rt.registry == nil || !target.execution.Valid() || target.execution.Tenant() != target.scope.TenantID || target.scope.Validate() != nil || call.meta.IdempotencyKey == "" || len(call.input) == 0 || len(call.input) > 1<<20 || !json.Valid(call.input) {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation invocation identity is invalid")
	}
	snapshot, err := rt.store.Load(target.scope.TenantID)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "module operation registration is unavailable")
	}
	module, installed := installedModule(snapshot, target.owner)
	if !installed || !containsPublishedOperation(module.PublishedOperations, target.published) {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeNotFound, "published operation is not registered")
	}
	if scopeErr := contract.ValidateResourceBusinessScope(target.scope, target.published.ScopePolicy); scopeErr != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation scope does not match its registration")
	}
	resource := "module." + string(target.owner) + ".operation." + target.published.Name
	if _, authErr := policy.DecideForBusinessScope(target.execution, rt.registry, resource, "write", target.scope, target.published.ScopePolicy == contract.ScopeEntityOwned); authErr != nil {
		return contract.OperationReceipt{}, authErr
	}
	spec, found := registeredOperation(module, target.published)
	if !found || spec.Input != call.inputSchema {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation input schema does not match its registration")
	}
	if spec.Effect != contract.EffectLocal && spec.Effect != contract.EffectExternal {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation is not a registered command")
	}
	operationRef := contract.PublishedOperationRef(target.owner, target.published.Capability, target.published.Operation)
	intentKey, err := contract.PublishedOperationIntentKey(target.scope, operationRef, call.meta.IdempotencyKey)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation intent is invalid")
	}
	intentHash := strings.TrimPrefix(string(intentKey), "published:")
	inputHash := configstore.PublishedOperationInputHash(call.input)
	actor := target.execution.Actor()
	dispatchOwner := false
	prior, exists, err := rt.store.LoadPublishedOperationAttempt(ctx, target.scope.TenantID, intentHash, actor, target.scope)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation intent is unavailable")
	}
	if exists {
		if prior.InputHash != inputHash || prior.Receipt.OperationRef != operationRef {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeIdempotencyKeyReused, "operation idempotency key was reused for a different intent")
		}
		if call.meta.ExpectedRevision != "" && string(call.meta.ExpectedRevision) != strconv.FormatUint(snapshot.Revision, 10) {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeStaleSubject, "operation registration revision changed")
		}
		if prior.Receipt.Status == contract.ReceiptCompleted || prior.Receipt.Status == contract.ReceiptRejected || prior.Receipt.Status == contract.ReceiptConflict || prior.Receipt.Status == contract.ReceiptFailed {
			return prior.Receipt, nil
		}
		if spec.Effect == contract.EffectLocal {
			if recovered, found, recoveryErr := recoverLocalPublishedOperation(ctx, rt, target, localRecovery{module: module, intentKey: intentKey, revision: snapshot.Revision, attempt: prior}); found {
				return recovered, recoveryErr
			}
		}
		if spec.Effect == contract.EffectExternal && prior.Claimed {
			claimed, claimErr := rt.store.ClaimPublishedOperationDispatch(ctx, actor, prior)
			if claimErr != nil {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation dispatch claim is unavailable")
			}
			if claimed {
				dispatchOwner = true
			} else {
				return prior.Receipt, nil
			}
		}
		if spec.Effect == contract.EffectExternal && prior.Receipt.Status == contract.ReceiptRunning && !dispatchOwner {
			return prior.Receipt, nil
		}
	}

	plan, err := rt.resolve(target.scope)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation provider plan is unavailable")
	}
	var selected plugin.ResolvedBinding
	var caller contract.ModuleID
	for _, candidate := range target.published.AllowedCallers {
		binding, ok := plan.Resolve(plugin.BindingKey{Scope: target.scope, ConsumerModuleID: candidate, CapabilityName: target.published.Capability})
		if ok && binding.OwnerModuleID == target.owner {
			selected, caller = binding, candidate
			break
		}
	}
	if caller == "" {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "no registered caller has the selected provider binding")
	}
	if spec.Effect == contract.EffectExternal {
		providerContext := contract.OperationContext{Site: string(target.scope.TenantID), Actor: target.execution.Actor().PrincipalID, Scope: target.scope}
		handle, openErr := selected.Open(providerContext)
		if openErr != nil || handle == nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider is unavailable")
		}
		_, recoverable := handle.(plugin.CapabilityOperationReconciler)
		handle.Revoke()
		if !recoverable {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "external provider does not support durable outcome reconciliation")
		}
	}

	var lease contract.CapabilityLease
	var pin contract.PinnedDispatch
	if exists {
		if prior.Dispatch == nil || prior.Lease == nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "stored published operation provider pin is missing")
		}
		pin, lease = *prior.Dispatch, *prior.Lease
		if pin.CallerModuleID != caller || pin.ProviderID != selected.ProviderID || pin.ProviderModuleID != selected.OwnerModuleID || pin.BindingRevision != selected.Revision || pin.Operation.OperationName != target.published.Operation {
			return rt.receipts(actor).unknown(ctx, prior, contract.NewError(contract.CodeOutcomeUnknown, "the original provider binding is no longer available"))
		}
	} else {
		if call.meta.ExpectedRevision != "" && string(call.meta.ExpectedRevision) != strconv.FormatUint(snapshot.Revision, 10) {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeStaleSubject, "operation registration revision changed")
		}
		if installedModuleState(snapshot, target.owner) != contract.ModuleActive {
			return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation owner is not accepting new work")
		}
		leaseID := publicOperationLeaseID(target.scope, caller, selected, target.published.Operation)
		var leaseFound bool
		lease, leaseFound, err = rt.store.LoadCapabilityLease(ctx, target.scope.TenantID, leaseID)
		if err != nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease is unavailable")
		}
		if !leaseFound {
			lease = contract.CapabilityLease{ID: leaseID, TenantID: target.scope.TenantID, Scope: target.scope, CallerModuleID: caller, ProviderModuleID: selected.OwnerModuleID, ProviderID: selected.ProviderID, Operation: target.published.Operation, BindingRevision: selected.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().AddDate(20, 0, 0), State: "active"}
			ownerAdmission, ok := plan.LeaseAdmission.(plugin.CapabilityLeaseOwnerAdmission)
			if !ok {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease issuer is unavailable")
			}
			if err := ownerAdmission.EnsureCapabilityLease(ctx, lease); err != nil {
				return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation lease could not be issued")
			}
			lease, leaseFound, err = rt.store.LoadCapabilityLease(ctx, target.scope.TenantID, leaseID)
			if err != nil || !leaseFound {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease could not be loaded")
			}
		}
		if lease.State != "active" {
			return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation lease is revoked or expired")
		}
		publicID := contract.OperationID(contract.NewOperationID())
		pin = contract.PinnedDispatch{ExecutionID: string(publicID), Scope: target.scope, CallerModuleID: caller, ProviderModuleID: selected.OwnerModuleID, ProviderID: selected.ProviderID, BindingRevision: selected.Revision, Operation: contract.CapabilityAction{CapabilityName: target.published.Capability, OperationName: target.published.Operation, InputSchema: spec.Input, OutputSchema: spec.Output}, IdempotencyKey: intentKey, InputHash: inputHash}
		if pin.Validate() != nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "published operation provider pin is invalid")
		}
		receipt := contract.OperationReceipt{ExecutionID: publicID, OperationRef: operationRef, BusinessScope: target.scope, Status: contract.ReceiptRunning, Revision: strconv.FormatUint(snapshot.Revision, 10)}
		attempt := configstore.PublishedOperationAttempt{Receipt: receipt, IntentHash: intentHash, InputHash: inputHash, Dispatch: &pin, Lease: &lease}
		var created bool
		attempt, created, err = rt.store.BeginPublishedOperationAttempt(ctx, actor, attempt)
		if err != nil {
			return contract.OperationReceipt{}, err
		}
		if attempt.Receipt.Status != contract.ReceiptRunning {
			return attempt.Receipt, nil
		}
		pin, lease = *attempt.Dispatch, *attempt.Lease
		attempt.Claimed = true
		if pin.CallerModuleID != caller || pin.ProviderID != selected.ProviderID || pin.ProviderModuleID != selected.OwnerModuleID || pin.BindingRevision != selected.Revision {
			return rt.receipts(actor).unknown(ctx, attempt, contract.NewError(contract.CodeOutcomeUnknown, "the original provider binding is no longer available"))
		}
		prior = attempt
		exists = !created
		if spec.Effect == contract.EffectExternal && created {
			claimed, claimErr := rt.store.ClaimPublishedOperationDispatch(ctx, actor, prior)
			if claimErr != nil {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation dispatch could not be claimed")
			}
			if !claimed {
				return prior.Receipt, nil
			}
			dispatchOwner = true
		}
	}

	if spec.Effect == contract.EffectExternal {
		var outcome contract.ExternalOutcome
		var recovered plugin.CapabilityOperationResult
		var reconcileErr error
		if dispatchOwner {
			recovered, reconcileErr = plan.InvokeCapability(ctx, target.execution, plugin.CapabilityCall{Lease: lease, Scope: target.scope, Caller: caller, Capability: target.published.Capability, Operation: target.published.Operation, Meta: contract.CommandMeta{IdempotencyKey: pin.IdempotencyKey}, Payload: call.input})
			if reconcileErr == nil {
				outcome = contract.ExternalApplied
			} else {
				outcome = contract.ExternalUnknown
			}
		} else if exists {
			outcome, recovered, reconcileErr = plan.ReconcileCapability(ctx, target.execution, pin, lease)
		} else {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "external operation dispatch was not claimed")
		}
		if reconcileErr != nil {
			return rt.receipts(actor).unknown(ctx, prior, reconcileErr)
		}
		if outcome == contract.ExternalApplied {
			return rt.receipts(actor).complete(ctx, prior, recovered, snapshot.Revision)
		}
		if outcome == contract.ExternalUnknown {
			return rt.receipts(actor).unknown(ctx, prior, contract.NewError(contract.CodeOutcomeUnknown, "the original provider outcome is still unknown"))
		}
		return rt.receipts(actor).fail(ctx, prior, contract.ReceiptRejected, contract.NewError(contract.CodeConflict, "the provider confirmed the operation was not applied"))
	}

	call.meta.IdempotencyKey = intentKey
	// Published capability providers receive the business payload described by
	// the immutable operation schema. The selected local command provider adds
	// its established kernel envelope at its own implementation boundary.
	result, invokeErr := plan.InvokeCapability(ctx, target.execution, plugin.CapabilityCall{Lease: lease, Scope: target.scope, Caller: caller, Capability: target.published.Capability, Operation: target.published.Operation, Meta: call.meta, Payload: call.input})
	if invokeErr != nil {
		status, operationErr := receiptFailureForError(invokeErr, spec.Effect == contract.EffectExternal)
		if status == contract.ReceiptOutcomeUnknown {
			return rt.receipts(actor).unknown(ctx, prior, operationErr)
		}
		return rt.receipts(actor).fail(ctx, prior, status, operationErr)
	}
	return rt.receipts(actor).complete(ctx, prior, result, snapshot.Revision)
}

func lookupPublishedOperation(ctx context.Context, rt publishedOperationRuntime, target publishedOperationTarget, key contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
	if ctx == nil || rt.store == nil || rt.commandKernel == nil || rt.commandRegistry == nil || !target.execution.Valid() || target.execution.Tenant() != target.scope.TenantID || target.scope.Validate() != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeValidationFailed, "published operation lookup identity is invalid")
	}
	snapshot, err := rt.store.Load(target.scope.TenantID)
	if err != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeDependencyUnavailable, "module operation registration is unavailable")
	}
	module, installed := installedModule(snapshot, target.owner)
	if !installed || !containsPublishedOperation(module.PublishedOperations, target.published) {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeNotFound, "published operation is not registered")
	}
	operationRef := contract.PublishedOperationRef(target.owner, target.published.Capability, target.published.Operation)
	intentKey, err := contract.PublishedOperationIntentKey(target.scope, operationRef, key)
	if err != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeValidationFailed, "published operation lookup identity is invalid")
	}
	intentHash := strings.TrimPrefix(string(intentKey), "published:")
	attempt, found, err := rt.store.LoadPublishedOperationAttempt(ctx, target.scope.TenantID, intentHash, target.execution.Actor(), target.scope)
	if err != nil || !found {
		return contract.OperationReceipt{}, found, err
	}
	if attempt.Receipt.Status == contract.ReceiptCompleted || attempt.Receipt.Status == contract.ReceiptRejected || attempt.Receipt.Status == contract.ReceiptConflict || attempt.Receipt.Status == contract.ReceiptFailed {
		return attempt.Receipt, true, nil
	}
	spec, specFound := registeredOperation(module, target.published)
	if !specFound {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "published operation contract is unavailable")
	}
	if spec.Effect == contract.EffectLocal {
		if recovered, found, recoveryErr := recoverLocalPublishedOperation(ctx, rt, target, localRecovery{module: module, intentKey: intentKey, revision: snapshot.Revision, attempt: attempt}); found || recoveryErr != nil {
			return recovered, true, recoveryErr
		}
		return attempt.Receipt, true, nil
	}
	if spec.Effect != contract.EffectExternal || rt.resolve == nil || attempt.Dispatch == nil || attempt.Lease == nil {
		return attempt.Receipt, true, nil
	}
	plan, err := rt.resolve(target.scope)
	if err != nil {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "pinned provider plan is unavailable")
	}
	outcome, result, reconcileErr := plan.ReconcileCapability(ctx, target.execution, *attempt.Dispatch, *attempt.Lease)
	if reconcileErr != nil || outcome == contract.ExternalUnknown {
		if reconcileErr == nil {
			reconcileErr = contract.NewError(contract.CodeOutcomeUnknown, "the original provider outcome is still unknown")
		}
		receipt, updateErr := rt.receipts(target.execution.Actor()).unknown(ctx, attempt, reconcileErr)
		return receipt, true, updateErr
	}
	if outcome == contract.ExternalNotApplied {
		receipt, updateErr := rt.receipts(target.execution.Actor()).fail(ctx, attempt, contract.ReceiptRejected, contract.NewError(contract.CodeConflict, "the provider confirmed the operation was not applied"))
		return receipt, true, updateErr
	}
	receipt, updateErr := rt.receipts(target.execution.Actor()).complete(ctx, attempt, result, snapshot.Revision)
	return receipt, true, updateErr
}

func recoverLocalPublishedOperation(ctx context.Context, rt publishedOperationRuntime, target publishedOperationTarget, r localRecovery) (contract.OperationReceipt, bool, error) {
	command, ok := rt.commandRegistry.LookupCapability(target.published.Capability, target.published.Operation)
	if !ok || command.ModuleOwnerID != r.module.ID {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeNotFound, "published operation command is not registered")
	}
	actor := target.execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	result, found, lookupErr := rt.commandKernel.LookupReceiptByIntent(ctx, rt.store.DB, kernel.ReceiptIntent{Execution: target.execution, ActorUser: user, Command: command.FullName(), IdempotencyKey: string(r.intentKey)})
	if lookupErr != nil {
		if typed := asContractError(lookupErr); typed != nil && typed.Type == contract.CodeOutcomeUnknown {
			receipt, updateErr := rt.receipts(actor).unknown(ctx, r.attempt, typed)
			return receipt, true, updateErr
		}
		return contract.OperationReceipt{}, true, lookupErr
	}
	if !found {
		return contract.OperationReceipt{}, false, nil
	}
	spec, ok := registeredOperation(r.module, target.published)
	if !ok {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "published operation output schema is unavailable")
	}
	receipt, completeErr := rt.receipts(actor).complete(ctx, r.attempt, plugin.CapabilityOperationResult{ExecutionID: contract.OperationID(result.OperationID), Status: "completed", OutputSchema: spec.Output, Output: result.Data}, r.revision)
	return receipt, true, completeErr
}

func (r operationReceipts) complete(ctx context.Context, attempt configstore.PublishedOperationAttempt, result plugin.CapabilityOperationResult, revisionNumber uint64) (contract.OperationReceipt, error) {
	schema := result.OutputSchema
	receipt := contract.OperationReceipt{ExecutionID: attempt.Receipt.ExecutionID, OperationRef: attempt.Receipt.OperationRef, BusinessScope: attempt.Receipt.BusinessScope, Status: contract.ReceiptCompleted, OutputSchema: &schema, Output: append(json.RawMessage(nil), result.Output...), CorrelationID: string(result.ExecutionID), Revision: strconv.FormatUint(revisionNumber, 10)}
	if err := receipt.Validate(); err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "published operation receipt is invalid")
	}
	attempt.Receipt = receipt
	if err := r.store.CompletePublishedOperationAttempt(ctx, r.actor, attempt); err != nil {
		loaded, found, loadErr := r.store.LoadPublishedOperationReceipt(ctx, receipt.BusinessScope.TenantID, receipt.ExecutionID, r.actor, receipt.BusinessScope)
		if loadErr == nil && found && loaded.Status == contract.ReceiptCompleted {
			return loaded, nil
		}
		return contract.OperationReceipt{}, contract.NewError(contract.CodeOutcomeUnknown, "operation completed but its public receipt could not be persisted")
	}
	return receipt, nil
}

func (r operationReceipts) unknown(ctx context.Context, attempt configstore.PublishedOperationAttempt, cause error) (contract.OperationReceipt, error) {
	_, operationErr := receiptFailureForError(cause, true)
	return r.fail(ctx, attempt, contract.ReceiptOutcomeUnknown, operationErr)
}

func (r operationReceipts) fail(ctx context.Context, attempt configstore.PublishedOperationAttempt, status contract.OperationReceiptStatus, cause *contract.Error) (contract.OperationReceipt, error) {
	if cause == nil {
		cause = contract.NewError(contract.CodeInternal, "published operation failed")
	}
	attempt.Receipt.Status = status
	attempt.Receipt.OutputSchema = nil
	attempt.Receipt.Output = nil
	attempt.Receipt.Error = cause
	if err := attempt.Receipt.Validate(); err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "published operation failure receipt is invalid")
	}
	if err := r.store.CompletePublishedOperationAttempt(ctx, r.actor, attempt); err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeOutcomeUnknown, "published operation outcome could not be recorded")
	}
	return attempt.Receipt, nil
}

func receiptFailureForError(err error, external bool) (contract.OperationReceiptStatus, *contract.Error) {
	typed := asContractError(err)
	if typed == nil {
		if external {
			return contract.ReceiptOutcomeUnknown, contract.NewError(contract.CodeOutcomeUnknown, "the provider outcome is unknown")
		}
		return contract.ReceiptFailed, contract.NewError(contract.CodeInternal, "published operation failed")
	}
	switch typed.Type {
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeStaleSubject, contract.CodeLeaseLost:
		return contract.ReceiptConflict, typed
	case contract.CodeOutcomeUnknown:
		return contract.ReceiptOutcomeUnknown, typed
	case contract.CodeUnauthenticated, contract.CodePermissionDenied, contract.CodeValidationFailed:
		return contract.ReceiptRejected, typed
	default:
		if external {
			return contract.ReceiptOutcomeUnknown, contract.NewError(contract.CodeOutcomeUnknown, "the provider outcome is unknown")
		}
		return contract.ReceiptFailed, typed
	}
}

func asContractError(err error) *contract.Error {
	var typed *contract.Error
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}

func registeredOperation(module contract.ModuleDescriptor, published contract.PublishedOperation) (contract.OperationSpec, bool) {
	for _, offer := range module.Provides {
		if offer.Name == published.Capability {
			for _, operation := range offer.Operations {
				if operation.Name == published.Operation {
					return operation, true
				}
			}
		}
	}
	return contract.OperationSpec{}, false
}

func publicOperationLeaseID(scope contract.BusinessScope, caller contract.ModuleID, binding plugin.ResolvedBinding, operation string) string {
	data, _ := json.Marshal([]string{string(scope.TenantID), string(scope.Kind), scope.LegalEntityID, string(caller), binding.ProviderID, string(binding.OwnerModuleID), operation, fmt.Sprint(binding.Revision)})
	digest := sha256.Sum256(data)
	return "published-" + hex.EncodeToString(digest[:])
}

func installedModule(snapshot contract.ModuleSnapshot, owner contract.ModuleID) (contract.ModuleDescriptor, bool) {
	for _, module := range snapshot.Modules {
		if module.ID == owner {
			return module, installedModuleState(snapshot, owner) != ""
		}
	}
	return contract.ModuleDescriptor{}, false
}
func installedModuleState(snapshot contract.ModuleSnapshot, owner contract.ModuleID) contract.ModuleLifecycleState {
	for _, installation := range snapshot.Installations {
		if installation.ModuleID == owner {
			return contract.ModuleLifecycleState(installation.State)
		}
	}
	return ""
}
func containsPublishedOperation(operations []contract.PublishedOperation, target contract.PublishedOperation) bool {
	a, _ := json.Marshal(target)
	for _, candidate := range operations {
		b, _ := json.Marshal(candidate)
		if string(a) == string(b) {
			return true
		}
	}
	return false
}
