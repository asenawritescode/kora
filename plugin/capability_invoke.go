package plugin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
)

// CapabilityOperationHandle is implemented by provider handles that execute
// a declared capability operation. Provider selection and operation schemas
// come from the immutable BindingPlan, never from the caller payload.
type CapabilityOperationHandle interface {
	contract.CapabilityHandle
	InvokeCapability(context.Context, CapabilityInvocation) (json.RawMessage, error)
}

// CapabilityInvocation is assembled by BindingPlan after resolving a pinned
// provider and operation contract. Implementations must treat Execution as
// the actor authority and ModuleExecution as the bounded module provenance.
type CapabilityInvocation struct {
	Context         contract.OperationContext
	Execution       policy.Execution
	ModuleExecution contract.ModuleExecution
	Operation       contract.OperationSpec
	Meta            contract.CommandMeta
	Payload         json.RawMessage
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
func (p BindingPlan) InvokeCapability(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, caller contract.ModuleID, capability contract.CapabilityName, operation string, meta contract.CommandMeta, payload json.RawMessage) (CapabilityOperationResult, error) {
	if ctx == nil || !execution.Valid() || scope.Validate() != nil || execution.Tenant() != scope.TenantID || caller == "" || capability == "" || operation == "" || meta.IdempotencyKey == "" || len(payload) == 0 || len(payload) > 1<<20 || !json.Valid(payload) {
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
	if err := binding.validateSchema(spec.Input, payload); err != nil {
		return CapabilityOperationResult{}, fmt.Errorf("capability input failed its registered schema: %w", err)
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
	output, err := invoker.InvokeCapability(ctx, CapabilityInvocation{Context: providerContext, Execution: execution, ModuleExecution: moduleExecution, Operation: *spec, Meta: meta, Payload: append(json.RawMessage(nil), payload...)})
	if err != nil {
		return CapabilityOperationResult{}, err
	}
	if len(output) == 0 || len(output) > 1<<20 || !json.Valid(output) {
		return CapabilityOperationResult{}, fmt.Errorf("capability provider returned invalid output")
	}
	if err := binding.validateSchema(spec.Output, output); err != nil {
		return CapabilityOperationResult{}, fmt.Errorf("capability output failed its registered schema: %w", err)
	}
	return CapabilityOperationResult{ExecutionID: contract.OperationID(contract.NewOperationID()), Status: "completed", OutputSchema: spec.Output, Output: append(json.RawMessage(nil), output...)}, nil
}
