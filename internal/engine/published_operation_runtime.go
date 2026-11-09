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

func executePublishedOperation(ctx context.Context, store *configstore.ModuleStore, resolve moduleBindingResolver, commandKernel *kernel.Kernel, commandRegistry *kernel.CommandRegistry, registry *doctype.Registry, execution policy.Execution, scope contract.BusinessScope, owner contract.ModuleID, published contract.PublishedOperation, inputSchema contract.SchemaRef, meta contract.CommandMeta, input []byte) (contract.OperationReceipt, error) {
	if ctx == nil || store == nil || resolve == nil || commandKernel == nil || commandRegistry == nil || registry == nil || !execution.Valid() || execution.Tenant() != scope.TenantID || scope.Validate() != nil || meta.IdempotencyKey == "" || len(input) == 0 || len(input) > 1<<20 || !json.Valid(input) {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation invocation identity is invalid")
	}
	snapshot, err := store.Load(scope.TenantID)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "module operation registration is unavailable")
	}
	module, installed := installedModule(snapshot, owner)
	if !installed || !containsPublishedOperation(module.PublishedOperations, published) {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeNotFound, "published operation is not registered")
	}
	if err := contract.ValidateResourceBusinessScope(scope, published.ScopePolicy); err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation scope does not match its registration")
	}
	resource := "module." + string(owner) + ".operation." + published.Name
	if _, authErr := policy.DecideForBusinessScope(execution, registry, resource, "write", scope, published.ScopePolicy == contract.ScopeEntityOwned); authErr != nil {
		return contract.OperationReceipt{}, authErr
	}
	spec, found := registeredOperation(module, published)
	if !found || spec.Input != inputSchema {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation input schema does not match its registration")
	}
	if spec.Effect != contract.EffectLocal && spec.Effect != contract.EffectExternal {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation is not a registered command")
	}
	operationRef := contract.PublishedOperationRef(owner, published.Capability, published.Operation)
	intentKey, err := contract.PublishedOperationIntentKey(scope, operationRef, meta.IdempotencyKey)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "published operation intent is invalid")
	}
	intentHash := strings.TrimPrefix(string(intentKey), "published:")
	inputHash := configstore.PublishedOperationInputHash(input)
	actor := execution.Actor()
	dispatchOwner := false
	prior, exists, err := store.LoadPublishedOperationAttempt(ctx, scope.TenantID, intentHash, actor, scope)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation intent is unavailable")
	}
	if exists {
		if prior.InputHash != inputHash || prior.Receipt.OperationRef != operationRef {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeIdempotencyKeyReused, "operation idempotency key was reused for a different intent")
		}
		if meta.ExpectedRevision != "" && string(meta.ExpectedRevision) != strconv.FormatUint(snapshot.Revision, 10) {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeStaleSubject, "operation registration revision changed")
		}
		if prior.Receipt.Status == contract.ReceiptCompleted || prior.Receipt.Status == contract.ReceiptRejected || prior.Receipt.Status == contract.ReceiptConflict || prior.Receipt.Status == contract.ReceiptFailed {
			return prior.Receipt, nil
		}
		if spec.Effect == contract.EffectLocal {
			if recovered, found, recoveryErr := recoverLocalPublishedOperation(ctx, store, commandKernel, commandRegistry, execution, scope, module, published, intentKey, snapshot.Revision, prior); found {
				return recovered, recoveryErr
			}
		}
		if spec.Effect == contract.EffectExternal && prior.Claimed {
			claimed, claimErr := store.ClaimPublishedOperationDispatch(ctx, actor, prior)
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

	plan, err := resolve(scope)
	if err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation provider plan is unavailable")
	}
	var selected plugin.ResolvedBinding
	var caller contract.ModuleID
	for _, candidate := range published.AllowedCallers {
		binding, ok := plan.Resolve(plugin.BindingKey{Scope: scope, ConsumerModuleID: candidate, CapabilityName: published.Capability})
		if ok && binding.OwnerModuleID == owner {
			selected, caller = binding, candidate
			break
		}
	}
	if caller == "" {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "no registered caller has the selected provider binding")
	}
	if spec.Effect == contract.EffectExternal {
		providerContext := contract.OperationContext{Site: string(scope.TenantID), Actor: execution.Actor().PrincipalID, Scope: scope}
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
		if pin.CallerModuleID != caller || pin.ProviderID != selected.ProviderID || pin.ProviderModuleID != selected.OwnerModuleID || pin.BindingRevision != selected.Revision || pin.Operation.OperationName != published.Operation {
			return publishedOperationUnknown(ctx, store, actor, prior, contract.NewError(contract.CodeOutcomeUnknown, "the original provider binding is no longer available"))
		}
	} else {
		if meta.ExpectedRevision != "" && string(meta.ExpectedRevision) != strconv.FormatUint(snapshot.Revision, 10) {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeStaleSubject, "operation registration revision changed")
		}
		if installedModuleState(snapshot, owner) != contract.ModuleActive {
			return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation owner is not accepting new work")
		}
		leaseID := publicOperationLeaseID(scope, caller, selected, published.Operation)
		var leaseFound bool
		lease, leaseFound, err = store.LoadCapabilityLease(ctx, scope.TenantID, leaseID)
		if err != nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease is unavailable")
		}
		if !leaseFound {
			lease = contract.CapabilityLease{ID: leaseID, TenantID: scope.TenantID, Scope: scope, CallerModuleID: caller, ProviderModuleID: selected.OwnerModuleID, ProviderID: selected.ProviderID, Operation: published.Operation, BindingRevision: selected.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().AddDate(20, 0, 0), State: "active"}
			ownerAdmission, ok := plan.LeaseAdmission.(plugin.CapabilityLeaseOwnerAdmission)
			if !ok {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease issuer is unavailable")
			}
			if err := ownerAdmission.EnsureCapabilityLease(ctx, lease); err != nil {
				return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation lease could not be issued")
			}
			lease, leaseFound, err = store.LoadCapabilityLease(ctx, scope.TenantID, leaseID)
			if err != nil || !leaseFound {
				return contract.OperationReceipt{}, contract.NewError(contract.CodeDependencyUnavailable, "published operation lease could not be loaded")
			}
		}
		if lease.State != "active" {
			return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation lease is revoked or expired")
		}
		publicID := contract.OperationID(contract.NewOperationID())
		pin = contract.PinnedDispatch{ExecutionID: string(publicID), Scope: scope, CallerModuleID: caller, ProviderModuleID: selected.OwnerModuleID, ProviderID: selected.ProviderID, BindingRevision: selected.Revision, Operation: contract.CapabilityAction{CapabilityName: published.Capability, OperationName: published.Operation, InputSchema: spec.Input, OutputSchema: spec.Output}, IdempotencyKey: intentKey, InputHash: inputHash}
		if pin.Validate() != nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "published operation provider pin is invalid")
		}
		receipt := contract.OperationReceipt{ExecutionID: publicID, OperationRef: operationRef, BusinessScope: scope, Status: contract.ReceiptRunning, Revision: strconv.FormatUint(snapshot.Revision, 10)}
		attempt := configstore.PublishedOperationAttempt{Receipt: receipt, IntentHash: intentHash, InputHash: inputHash, Dispatch: &pin, Lease: &lease}
		var created bool
		attempt, created, err = store.BeginPublishedOperationAttempt(ctx, actor, attempt)
		if err != nil {
			return contract.OperationReceipt{}, err
		}
		if attempt.Receipt.Status != contract.ReceiptRunning {
			return attempt.Receipt, nil
		}
		pin, lease = *attempt.Dispatch, *attempt.Lease
		attempt.Claimed = true
		if pin.CallerModuleID != caller || pin.ProviderID != selected.ProviderID || pin.ProviderModuleID != selected.OwnerModuleID || pin.BindingRevision != selected.Revision {
			return publishedOperationUnknown(ctx, store, actor, attempt, contract.NewError(contract.CodeOutcomeUnknown, "the original provider binding is no longer available"))
		}
		prior = attempt
		exists = !created
		if spec.Effect == contract.EffectExternal && created {
			claimed, claimErr := store.ClaimPublishedOperationDispatch(ctx, actor, prior)
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
			recovered, reconcileErr = plan.InvokeCapability(ctx, execution, lease, scope, caller, published.Capability, published.Operation, contract.CommandMeta{IdempotencyKey: pin.IdempotencyKey}, input)
			if reconcileErr == nil {
				outcome = contract.ExternalApplied
			} else {
				outcome = contract.ExternalUnknown
			}
		} else if exists {
			outcome, recovered, reconcileErr = plan.ReconcileCapability(ctx, execution, pin, lease)
		} else {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "external operation dispatch was not claimed")
		}
		if reconcileErr != nil {
			return publishedOperationUnknown(ctx, store, actor, prior, reconcileErr)
		}
		if outcome == contract.ExternalApplied {
			return completePublishedOperation(ctx, store, actor, prior, recovered, snapshot.Revision)
		}
		if outcome == contract.ExternalUnknown {
			return publishedOperationUnknown(ctx, store, actor, prior, contract.NewError(contract.CodeOutcomeUnknown, "the original provider outcome is still unknown"))
		}
		return publishedOperationFailure(ctx, store, actor, prior, contract.ReceiptRejected, contract.NewError(contract.CodeConflict, "the provider confirmed the operation was not applied"))
	}

	meta.IdempotencyKey = intentKey
	// Published capability providers receive the business payload described by
	// the immutable operation schema. The selected local command provider adds
	// its established kernel envelope at its own implementation boundary.
	result, invokeErr := plan.InvokeCapability(ctx, execution, lease, scope, caller, published.Capability, published.Operation, meta, input)
	if invokeErr != nil {
		status, operationErr := receiptFailureForError(invokeErr, spec.Effect == contract.EffectExternal)
		if status == contract.ReceiptOutcomeUnknown {
			return publishedOperationUnknown(ctx, store, actor, prior, operationErr)
		}
		return publishedOperationFailure(ctx, store, actor, prior, status, operationErr)
	}
	return completePublishedOperation(ctx, store, actor, prior, result, snapshot.Revision)
}

func lookupPublishedOperation(ctx context.Context, store *configstore.ModuleStore, commandKernel *kernel.Kernel, commandRegistry *kernel.CommandRegistry, resolve moduleBindingResolver, execution policy.Execution, scope contract.BusinessScope, owner contract.ModuleID, published contract.PublishedOperation, key contract.IdempotencyKey) (contract.OperationReceipt, bool, error) {
	if ctx == nil || store == nil || commandKernel == nil || commandRegistry == nil || !execution.Valid() || execution.Tenant() != scope.TenantID || scope.Validate() != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeValidationFailed, "published operation lookup identity is invalid")
	}
	snapshot, err := store.Load(scope.TenantID)
	if err != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeDependencyUnavailable, "module operation registration is unavailable")
	}
	module, installed := installedModule(snapshot, owner)
	if !installed || !containsPublishedOperation(module.PublishedOperations, published) {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeNotFound, "published operation is not registered")
	}
	operationRef := contract.PublishedOperationRef(owner, published.Capability, published.Operation)
	intentKey, err := contract.PublishedOperationIntentKey(scope, operationRef, key)
	if err != nil {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeValidationFailed, "published operation lookup identity is invalid")
	}
	intentHash := strings.TrimPrefix(string(intentKey), "published:")
	attempt, found, err := store.LoadPublishedOperationAttempt(ctx, scope.TenantID, intentHash, execution.Actor(), scope)
	if err != nil || !found {
		return contract.OperationReceipt{}, found, err
	}
	if attempt.Receipt.Status == contract.ReceiptCompleted || attempt.Receipt.Status == contract.ReceiptRejected || attempt.Receipt.Status == contract.ReceiptConflict || attempt.Receipt.Status == contract.ReceiptFailed {
		return attempt.Receipt, true, nil
	}
	spec, specFound := registeredOperation(module, published)
	if !specFound {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "published operation contract is unavailable")
	}
	if spec.Effect == contract.EffectLocal {
		if recovered, found, recoveryErr := recoverLocalPublishedOperation(ctx, store, commandKernel, commandRegistry, execution, scope, module, published, intentKey, snapshot.Revision, attempt); found || recoveryErr != nil {
			return recovered, true, recoveryErr
		}
		return attempt.Receipt, true, nil
	}
	if spec.Effect != contract.EffectExternal || resolve == nil || attempt.Dispatch == nil || attempt.Lease == nil {
		return attempt.Receipt, true, nil
	}
	plan, err := resolve(scope)
	if err != nil {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "pinned provider plan is unavailable")
	}
	outcome, result, reconcileErr := plan.ReconcileCapability(ctx, execution, *attempt.Dispatch, *attempt.Lease)
	if reconcileErr != nil || outcome == contract.ExternalUnknown {
		if reconcileErr == nil {
			reconcileErr = contract.NewError(contract.CodeOutcomeUnknown, "the original provider outcome is still unknown")
		}
		receipt, updateErr := publishedOperationUnknown(ctx, store, execution.Actor(), attempt, reconcileErr)
		return receipt, true, updateErr
	}
	if outcome == contract.ExternalNotApplied {
		receipt, updateErr := publishedOperationFailure(ctx, store, execution.Actor(), attempt, contract.ReceiptRejected, contract.NewError(contract.CodeConflict, "the provider confirmed the operation was not applied"))
		return receipt, true, updateErr
	}
	receipt, updateErr := completePublishedOperation(ctx, store, execution.Actor(), attempt, result, snapshot.Revision)
	return receipt, true, updateErr
}

func recoverLocalPublishedOperation(ctx context.Context, store *configstore.ModuleStore, commandKernel *kernel.Kernel, commandRegistry *kernel.CommandRegistry, execution policy.Execution, scope contract.BusinessScope, module contract.ModuleDescriptor, published contract.PublishedOperation, intentKey contract.IdempotencyKey, revisionNumber uint64, attempt configstore.PublishedOperationAttempt) (contract.OperationReceipt, bool, error) {
	command, ok := commandRegistry.LookupCapability(published.Capability, published.Operation)
	if !ok || command.ModuleOwnerID != module.ID {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodeNotFound, "published operation command is not registered")
	}
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}
	result, found, lookupErr := commandKernel.LookupReceiptByIntent(ctx, store.DB, execution, user, command.FullName(), string(intentKey))
	if lookupErr != nil {
		if typed := asContractError(lookupErr); typed != nil && typed.Type == contract.CodeOutcomeUnknown {
			receipt, updateErr := publishedOperationUnknown(ctx, store, actor, attempt, typed)
			return receipt, true, updateErr
		}
		return contract.OperationReceipt{}, true, lookupErr
	}
	if !found {
		return contract.OperationReceipt{}, false, nil
	}
	spec, ok := registeredOperation(module, published)
	if !ok {
		return contract.OperationReceipt{}, true, contract.NewError(contract.CodeDependencyUnavailable, "published operation output schema is unavailable")
	}
	receipt, completeErr := completePublishedOperation(ctx, store, actor, attempt, plugin.CapabilityOperationResult{ExecutionID: contract.OperationID(result.OperationID), Status: "completed", OutputSchema: spec.Output, Output: result.Data}, revisionNumber)
	return receipt, true, completeErr
}

func completePublishedOperation(ctx context.Context, store *configstore.ModuleStore, actor contract.ActorContext, attempt configstore.PublishedOperationAttempt, result plugin.CapabilityOperationResult, revisionNumber uint64) (contract.OperationReceipt, error) {
	schema := result.OutputSchema
	receipt := contract.OperationReceipt{ExecutionID: attempt.Receipt.ExecutionID, OperationRef: attempt.Receipt.OperationRef, BusinessScope: attempt.Receipt.BusinessScope, Status: contract.ReceiptCompleted, OutputSchema: &schema, Output: append(json.RawMessage(nil), result.Output...), CorrelationID: string(result.ExecutionID), Revision: strconv.FormatUint(revisionNumber, 10)}
	if err := receipt.Validate(); err != nil {
		return contract.OperationReceipt{}, contract.NewError(contract.CodeInternal, "published operation receipt is invalid")
	}
	attempt.Receipt = receipt
	if err := store.CompletePublishedOperationAttempt(ctx, actor, attempt); err != nil {
		loaded, found, loadErr := store.LoadPublishedOperationReceipt(ctx, receipt.BusinessScope.TenantID, receipt.ExecutionID, actor, receipt.BusinessScope)
		if loadErr == nil && found && loaded.Status == contract.ReceiptCompleted {
			return loaded, nil
		}
		return contract.OperationReceipt{}, contract.NewError(contract.CodeOutcomeUnknown, "operation completed but its public receipt could not be persisted")
	}
	return receipt, nil
}

func publishedOperationUnknown(ctx context.Context, store *configstore.ModuleStore, actor contract.ActorContext, attempt configstore.PublishedOperationAttempt, cause error) (contract.OperationReceipt, error) {
	_, operationErr := receiptFailureForError(cause, true)
	return publishedOperationFailure(ctx, store, actor, attempt, contract.ReceiptOutcomeUnknown, operationErr)
}

func publishedOperationFailure(ctx context.Context, store *configstore.ModuleStore, actor contract.ActorContext, attempt configstore.PublishedOperationAttempt, status contract.OperationReceiptStatus, cause *contract.Error) (contract.OperationReceipt, error) {
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
	if err := store.CompletePublishedOperationAttempt(ctx, actor, attempt); err != nil {
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
