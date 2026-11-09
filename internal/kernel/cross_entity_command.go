package kernel

import (
	"github.com/asenawritescode/kora/contract"
)

func prepareCrossEntityCommand(op *Operation, command *CommandResource) *contract.Error {
	if op == nil || command == nil || command.CrossEntity == nil {
		return nil
	}
	source := op.Context.BusinessScope
	if source.Validate() != nil || source.Kind != contract.ScopeKindLegalEntity || source.TenantID != op.Context.Execution.Tenant() {
		return contract.NewError(contract.CodePermissionDenied, "cross-entity command requires verified source legal-entity scope")
	}
	input, err := decodeInput(op.Payload)
	if err != nil {
		return err
	}
	targetID, targetErr := resolveStringRef("$"+command.CrossEntity.TargetEntityField, input)
	if targetErr != nil || targetID == "" {
		return contract.NewError(contract.CodeValidationFailed, "cross-entity target entity is required")
	}
	operation := contract.CrossEntityOperation{
		OperationID:    command.FullName(),
		TenantID:       op.Context.Execution.Tenant(),
		SourceEntityID: source.LegalEntityID,
		TargetEntityID: targetID,
		Resource:       command.CrossEntity.Resource,
		Operation:      command.CrossEntity.Operation,
	}
	if err := operation.Validate(); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "cross-entity command identity is invalid")
	}
	op.Context.crossEntityOperation = &operation
	op.Context.crossEntityTarget = contract.BusinessScope{TenantID: operation.TenantID, Kind: contract.ScopeKindLegalEntity, LegalEntityID: operation.TargetEntityID}
	return nil
}

func operationForCommandStep(op Operation, command *CommandResource, step CommandStep) (Operation, *contract.Error) {
	if command == nil || command.CrossEntity == nil {
		return op, nil
	}
	if op.Context.crossEntityOperation == nil {
		return Operation{}, contract.NewError(contract.CodePermissionDenied, "cross-entity command was not resolved from verified execution context")
	}
	var side string
	if create := step.createSpec(); create != nil {
		side = create.Scope
	} else if step.Update != nil {
		side = step.Update.Scope
	}
	switch side {
	case "source":
		return op, nil
	case "target":
		if op.Context.crossEntityTarget.Validate() != nil || op.Context.crossEntityTarget.TenantID != op.Context.Execution.Tenant() {
			return Operation{}, contract.NewError(contract.CodePermissionDenied, "cross-entity target scope is invalid")
		}
		stepOp := op
		stepOp.Context.BusinessScope = op.Context.crossEntityTarget
		return stepOp, nil
	default:
		return Operation{}, contract.NewError(contract.CodeValidationFailed, "cross-entity command step must declare source or target scope")
	}
}
