package engine

import (
	"context"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/plugin"
)

// previewRequest is one published operation to preview, with the registration it was checked against.
type previewRequest struct {
	target       publishedOperationTarget
	call         publishedOperationInvocation
	spec         contract.OperationSpec
	snapshot     contract.ModuleSnapshot
	operationRef string
}

// resolveCallerBinding returns the provider plan and the caller and binding that serve a
// published operation of the target scope.
func resolveCallerBinding(rt publishedOperationRuntime, target publishedOperationTarget) (plugin.BindingPlan, plugin.ResolvedBinding, contract.ModuleID, error) {
	plan, err := rt.resolve(target.scope)
	if err != nil {
		return plugin.BindingPlan{}, plugin.ResolvedBinding{}, "", contract.NewError(contract.CodeDependencyUnavailable, "published operation provider plan is unavailable")
	}
	for _, candidate := range target.published.AllowedCallers {
		binding, ok := plan.Resolve(plugin.BindingKey{Scope: target.scope, ConsumerModuleID: candidate, CapabilityName: target.published.Capability})
		if ok && binding.OwnerModuleID == target.owner {
			return plan, binding, candidate, nil
		}
	}
	return plan, plugin.ResolvedBinding{}, "", contract.NewError(contract.CodeDependencyUnavailable, "no registered caller has the selected provider binding")
}

// previewPublishedOperation checks a published operation without writing its intent, attempt,
// lease or receipt. A local command runs through its provider, which rolls back the command's
// transaction under the preview context. Any other effect is checked against its input schema only.
func previewPublishedOperation(ctx context.Context, rt publishedOperationRuntime, req previewRequest) (contract.OperationReceipt, error) {
	target := req.target
	receipt := contract.OperationReceipt{OperationRef: req.operationRef, BusinessScope: target.scope, Status: contract.ReceiptCompleted, Revision: strconv.FormatUint(req.snapshot.Revision, 10)}
	plan, selected, caller, err := resolveCallerBinding(rt, target)
	if err != nil {
		return contract.OperationReceipt{}, err
	}
	if req.spec.Effect != contract.EffectLocal {
		if schemaErr := selected.ValidateOperationInput(req.spec.Input, req.call.input); schemaErr != nil {
			return contract.OperationReceipt{}, contract.NewError(contract.CodeValidationFailed, "input does not match the operation schema")
		}
		return receipt, nil
	}
	if installedModuleState(req.snapshot, target.owner) != contract.ModuleActive {
		return contract.OperationReceipt{}, contract.NewError(contract.CodePermissionDenied, "published operation owner is not accepting new work")
	}
	// The lease is built in memory, as the executor builds a new one, but it is not stored.
	leaseID := publicOperationLeaseID(target.scope, caller, selected, target.published.Operation)
	lease := contract.CapabilityLease{ID: leaseID, TenantID: target.scope.TenantID, Scope: target.scope, CallerModuleID: caller, ProviderModuleID: selected.OwnerModuleID, ProviderID: selected.ProviderID, Operation: target.published.Operation, BindingRevision: selected.Revision, GrantEpoch: 1, ExpiresAt: time.Now().UTC().AddDate(20, 0, 0), State: "active"}
	result, err := plan.InvokeCapability(kernel.WithPreview(ctx), target.execution, plugin.CapabilityCall{Lease: lease, Scope: target.scope, Caller: caller, Capability: target.published.Capability, Operation: target.published.Operation, Meta: req.call.meta, Payload: req.call.input})
	if err != nil {
		return contract.OperationReceipt{}, err
	}
	receipt.Output = result.Output
	return receipt, nil
}
