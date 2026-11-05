package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

// CapabilityOperationHandle is implemented by provider handles that execute
// a declared capability operation. Provider selection and operation schemas
// come from the immutable BindingPlan, never from the caller payload.
type CapabilityOperationHandle interface {
	contract.CapabilityHandle
	InvokeCapability(context.Context, CapabilityInvocation) (CapabilityInvocationResult, error)
}

// CapabilityOperationReconciler resolves a persisted external attempt without
// issuing the effect again. Providers must identify the exact stable key and
// input digest represented by the pin.
type CapabilityOperationReconciler interface {
	ReconcileCapability(context.Context, contract.PinnedDispatch, contract.CapabilityLease) (contract.ExternalOutcome, CapabilityInvocationResult, error)
}

// CapabilityInvocationResult identifies the provider-side execution so
// retries and reconciliation can refer to the effect that actually ran.
type CapabilityInvocationResult struct {
	ExecutionID contract.OperationID
	Output      json.RawMessage
}

// CapabilityInvocation is assembled by BindingPlan after resolving a pinned
// provider and operation contract. Implementations must treat Execution as
// the actor authority and ModuleExecution as the bounded module provenance.
type CapabilityInvocation struct {
	Context                    contract.OperationContext
	Execution                  policy.Execution
	ModuleExecution            contract.ModuleExecution
	Operation                  contract.OperationSpec
	Meta                       contract.CommandMeta
	Payload                    json.RawMessage
	BusinessReferenceValidator contract.BusinessReferenceValidator
}

// CapabilityOperationResult is the schema-bound result of one provider call.
type CapabilityOperationResult struct {
	ExecutionID  contract.OperationID `json:"execution_id"`
	Status       string               `json:"status"`
	OutputSchema contract.SchemaRef   `json:"output_schema"`
	Output       json.RawMessage      `json:"output"`
}

// InvokeCapability dispatches an operation through the provider selected in
// this plan. Provider identity, scope, contract, schema and binding revision
// are pinned before the handle is opened.
func (p BindingPlan) InvokeCapability(ctx context.Context, execution policy.Execution, lease contract.CapabilityLease, scope contract.BusinessScope, caller contract.ModuleID, capability contract.CapabilityName, operation string, meta contract.CommandMeta, payload json.RawMessage) (CapabilityOperationResult, error) {
	if ctx == nil || !execution.Valid() || scope.Validate() != nil || execution.Tenant() != scope.TenantID || caller == "" || capability == "" || operation == "" || meta.IdempotencyKey == "" || len(payload) == 0 || len(payload) > 1<<20 || !json.Valid(payload) || p.LeaseAdmission == nil {
		return CapabilityOperationResult{}, fmt.Errorf("capability invocation identity or payload is invalid")
	}
	key := BindingKey{Scope: scope, ConsumerModuleID: caller, CapabilityName: capability}
	binding, ok := p.Resolve(key)
	if !ok || binding.Revision == 0 || binding.OwnerModuleID == "" || binding.Factory == nil || binding.validateSchema == nil {
		return CapabilityOperationResult{}, fmt.Errorf("capability binding is unavailable or unpinned")
	}
	var spec *contract.OperationSpec
	for index := range binding.Operations {
		if binding.Operations[index].Name == operation {
			spec = &binding.Operations[index]
			break
		}
	}
	if spec == nil || (spec.Effect != contract.EffectLocal && spec.Effect != contract.EffectExternal) {
		return CapabilityOperationResult{}, fmt.Errorf("capability operation is not a declared command")
	}
	if lease.Validate() != nil || lease.State != "active" || lease.TenantID != scope.TenantID || lease.Scope != scope || lease.CallerModuleID != caller || lease.ProviderID != binding.ProviderID || lease.Operation != operation || lease.BindingRevision != binding.Revision {
		return CapabilityOperationResult{}, contract.NewError(contract.CodePermissionDenied, "capability lease does not match the resolved binding")
	}
	if lease.ProviderModuleID != binding.OwnerModuleID {
		return CapabilityOperationResult{}, contract.NewError(contract.CodePermissionDenied, "capability lease provider owner does not match the resolved binding")
	}
	if err := binding.validateSchema(spec.Input, payload); err != nil {
		return CapabilityOperationResult{}, contract.NewError(contract.CodeValidationFailed, "capability input failed its registered schema: "+err.Error())
	}
	actor := execution.Actor()
	providerContext := contract.OperationContext{Site: string(scope.TenantID), Actor: actor.PrincipalID, Scope: scope}
	handle, err := binding.Open(providerContext)
	if err != nil {
		return CapabilityOperationResult{}, fmt.Errorf("open selected capability provider: %w", err)
	}
	if handle == nil {
		return CapabilityOperationResult{}, fmt.Errorf("selected capability provider returned no handle")
	}
	defer handle.Revoke()
	invoker, ok := handle.(CapabilityOperationHandle)
	if !ok {
		return CapabilityOperationResult{}, fmt.Errorf("selected capability provider cannot invoke operations")
	}
	moduleExecution, err := contract.NewModuleExecution(scope, caller, binding.OwnerModuleID, binding.Revision, operation)
	if err != nil {
		return CapabilityOperationResult{}, err
	}
	referenceValidator := contract.BusinessReferenceValidator(func(module contract.ModuleID, rule contract.ReferenceRule, raw json.RawMessage) error {
		var ref contract.BusinessRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return fmt.Errorf("business reference must be a provider-qualified object")
		}
		_, err := p.ResolveBusinessReference(ctx, execution, scope, module, rule, ref)
		return err
	})
	if err := p.LeaseAdmission.AdmitCapabilityAttempt(ctx, lease, time.Now().UTC()); err != nil {
		return CapabilityOperationResult{}, err
	}
	providerResult, err := invoker.InvokeCapability(ctx, CapabilityInvocation{Context: providerContext, Execution: execution, ModuleExecution: moduleExecution, Operation: *spec, Meta: meta, Payload: append(json.RawMessage(nil), payload...), BusinessReferenceValidator: referenceValidator})
	if err != nil {
		return CapabilityOperationResult{}, err
	}
	if providerResult.ExecutionID == "" {
		return CapabilityOperationResult{}, fmt.Errorf("capability provider returned no execution identity")
	}
	output := providerResult.Output
	if len(output) == 0 || len(output) > 1<<20 || !json.Valid(output) {
		return CapabilityOperationResult{}, fmt.Errorf("capability provider returned invalid output")
	}
	if err := binding.validateSchema(spec.Output, output); err != nil {
		return CapabilityOperationResult{}, fmt.Errorf("capability output failed its registered schema: %w", err)
	}
	return CapabilityOperationResult{ExecutionID: providerResult.ExecutionID, Status: "completed", OutputSchema: spec.Output, Output: append(json.RawMessage(nil), output...)}, nil
}

// ReconcileCapability addresses only the provider pinned by a prior durable
// attempt. It never searches another provider and never invokes the effect.
func (p BindingPlan) ReconcileCapability(ctx context.Context, execution policy.Execution, pin contract.PinnedDispatch, lease contract.CapabilityLease) (contract.ExternalOutcome, CapabilityOperationResult, error) {
	if ctx == nil || !execution.Valid() || pin.Validate() != nil || lease.Validate() != nil || execution.Tenant() != pin.Scope.TenantID || lease.Scope != pin.Scope || lease.CallerModuleID != pin.CallerModuleID || lease.ProviderID != pin.ProviderID || lease.ProviderModuleID != pin.ProviderModuleID || lease.BindingRevision != pin.BindingRevision || lease.Operation != pin.Operation.OperationName {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeValidationFailed, "pinned capability reconciliation identity is invalid")
	}
	binding, ok := p.Resolve(BindingKey{Scope: pin.Scope, ConsumerModuleID: pin.CallerModuleID, CapabilityName: pin.Operation.CapabilityName})
	if !ok || binding.ProviderID != pin.ProviderID || binding.OwnerModuleID != pin.ProviderModuleID || binding.Revision != pin.BindingRevision || binding.Factory == nil || binding.validateSchema == nil {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeOutcomeUnknown, "the pinned provider binding is no longer available for reconciliation")
	}
	var spec *contract.OperationSpec
	for i := range binding.Operations {
		if binding.Operations[i].Name == pin.Operation.OperationName {
			spec = &binding.Operations[i]
			break
		}
	}
	if spec == nil || spec.Input != pin.Operation.InputSchema || spec.Output != pin.Operation.OutputSchema || spec.Effect != contract.EffectExternal {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeOutcomeUnknown, "the pinned operation contract is no longer available for reconciliation")
	}
	actor := execution.Actor()
	handle, err := binding.Open(contract.OperationContext{Site: string(pin.Scope.TenantID), Actor: actor.PrincipalID, Scope: pin.Scope})
	if err != nil || handle == nil {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeDependencyUnavailable, "the pinned provider is unavailable for reconciliation")
	}
	defer handle.Revoke()
	reconciler, ok := handle.(CapabilityOperationReconciler)
	if !ok {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeDependencyUnavailable, "the pinned provider does not support outcome reconciliation")
	}
	outcome, result, err := reconciler.ReconcileCapability(ctx, pin, lease)
	if err != nil || (outcome != contract.ExternalApplied && outcome != contract.ExternalNotApplied && outcome != contract.ExternalUnknown) {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeDependencyUnavailable, "the pinned provider outcome is unavailable")
	}
	if outcome != contract.ExternalApplied {
		return outcome, CapabilityOperationResult{}, nil
	}
	if result.ExecutionID == "" || len(result.Output) == 0 || len(result.Output) > 1<<20 || !json.Valid(result.Output) || binding.validateSchema(spec.Output, result.Output) != nil {
		return contract.ExternalUnknown, CapabilityOperationResult{}, contract.NewError(contract.CodeOutcomeUnknown, "the pinned provider returned an invalid reconciliation result")
	}
	return outcome, CapabilityOperationResult{ExecutionID: result.ExecutionID, Status: "completed", OutputSchema: spec.Output, Output: append(json.RawMessage(nil), result.Output...)}, nil
}

// ResolveBusinessReference opens only the provider already selected for the
// consumer capability in this immutable plan. It does not search alternate
// providers when resolution fails.
func (p BindingPlan) ResolveBusinessReference(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, consumer contract.ModuleID, rule contract.ReferenceRule, ref contract.BusinessRef) (contract.ResolvedBusinessRef, error) {
	if ctx == nil || !execution.Valid() || execution.Tenant() != scope.TenantID || scope.Validate() != nil || consumer == "" || rule.Validate() != nil || rule.Kind != "published_business_ref" || rule.ResourceType != ref.ResourceType {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("business reference rule or verified execution is invalid")
	}
	binding, ok := p.Resolve(BindingKey{Scope: scope, ConsumerModuleID: consumer, CapabilityName: rule.CapabilityName})
	if !ok || binding.Factory == nil || binding.Revision == 0 || binding.Revision != p.Revision {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("business reference provider is unavailable or unpinned")
	}
	if ref.ProviderID != binding.ProviderID {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("business reference provider does not match selected binding")
	}
	factory, ok := binding.Factory.(interface {
		OpenReferenceResolver(context.Context, contract.OperationContext) (contract.BusinessReferenceProviderHandle, error)
	})
	if !ok {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("selected provider does not support business reference resolution")
	}
	actor := execution.Actor()
	handle, err := factory.OpenReferenceResolver(ctx, contract.OperationContext{Site: string(scope.TenantID), Actor: actor.PrincipalID, Scope: scope})
	if err != nil {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("open selected reference provider: %w", err)
	}
	if handle == nil {
		return contract.ResolvedBusinessRef{}, fmt.Errorf("selected reference provider returned no resolver")
	}
	defer handle.Revoke()
	return contract.ValidateBusinessReference(ctx, handle, contract.ScopeContext{Scope: scope, ProviderID: binding.ProviderID}, ref, rule.ResourceType)
}
