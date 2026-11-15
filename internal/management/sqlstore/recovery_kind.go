package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/management"
)

// retryKind is what differs between the operator retry paths: the command it
// requeues, the reconciliation digest name, the stored-command recheck, and
// the error texts each path has always returned.
type retryKind struct {
	command    management.CommandKind
	digestName management.CommandKind
	// keyChecked parses the idempotency key before the command is validated.
	keyChecked bool
	// contextErr is returned when no context is given.
	contextErr func() error
	// invalid is returned when the reconciliation command fails validation.
	invalid string
	// conflict is returned when the operation is not a recovery_required
	// operation of this command kind.
	conflict string
	// stored rechecks the original command before it is requeued.
	stored func(management.Operation) error
	text   retryText
}

// retryText holds the prefixes of wrapped storage errors, and changed, the
// conflict when the guarded requeue UPDATE matches no row.
type retryText struct {
	encode, begin, replay, audit, requeue, result, changed, commit string
}

func (k retryKind) check(ctx context.Context, request retryRequest) error {
	if ctx == nil {
		return k.contextErr()
	}
	if _, err := contract.ParsePrincipalID(string(request.caller)); err != nil {
		return contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(request.id)); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if k.keyChecked {
		if _, err := contract.ParseIdempotencyKey(string(request.key)); err != nil {
			return contract.NewError(contract.CodeValidationFailed, "invalid reconciliation idempotency key")
		}
	}
	if err := request.validate(); err != nil {
		return contract.NewError(contract.CodeValidationFailed, k.invalid)
	}
	return nil
}

func (k retryKind) admit(operation management.Operation) error {
	if operation.Kind != k.command || !management.MoveRetry.Allows(operation.State, contract.OperationRequested) {
		return contract.NewError(contract.CodeConflict, k.conflict)
	}
	if k.stored == nil {
		return nil
	}
	return k.stored(operation)
}

func reconciliationContextError() error {
	return fmt.Errorf("management operation reconciliation requires context")
}

var stateRetry = retryKind{
	command: management.CommandSetTenantState, digestName: "reconcile_tenant_state", keyChecked: true,
	contextErr: reconciliationContextError,
	invalid:    "tenant-state reconciliation command is invalid",
	conflict:   "only uncertain tenant-state operations can be reconciled",
	text: retryText{
		encode: "encode operation reconciliation", begin: "begin management operation reconciliation",
		replay: "commit concurrent reconciliation replay", audit: "persist management operation reconciliation audit",
		requeue: "requeue reconciled tenant-state operation", result: "read reconciled operation update result",
		changed: "tenant-state operation changed during reconciliation", commit: "commit tenant-state operation reconciliation",
	},
}

var provisionRetry = retryKind{
	command: management.CommandProvisionTenant, digestName: "reconcile_tenant_provision", keyChecked: true,
	contextErr: reconciliationContextError,
	invalid:    "tenant provisioning reconciliation command is invalid",
	conflict:   "only uncertain tenant provisioning operations can be reconciled",
	stored: func(operation management.Operation) error {
		var command contract.ProvisionTenant
		if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateProvisionTenant(command) != nil || command.Tenant != operation.Tenant || command.PackageDigest != "" {
			return contract.NewError(contract.CodeConflict, "only valid base provisioning operations without package effects can be reconciled")
		}
		return nil
	},
	text: retryText{
		encode: "encode provisioning operation reconciliation", begin: "begin provisioning operation reconciliation",
		replay: "commit concurrent provisioning reconciliation replay", audit: "persist provisioning reconciliation audit",
		requeue: "requeue reconciled tenant provisioning operation", result: "read reconciled provisioning update result",
		changed: "tenant provisioning operation changed during reconciliation", commit: "commit provisioning operation reconciliation",
	},
}

var packageRetry = retryKind{
	command: management.CommandApplyPackage, digestName: "reconcile_tenant_package", keyChecked: true,
	contextErr: reconciliationContextError,
	invalid:    "package application reconciliation command is invalid",
	conflict:   "only uncertain package application operations can be reconciled",
	stored: func(operation management.Operation) error {
		var command contract.ApplyPackage
		if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateApplyPackage(command) != nil || command.Tenant != operation.Tenant {
			return contract.NewError(contract.CodeConflict, "stored package application command is invalid or does not match its operation")
		}
		return nil
	},
	text: retryText{
		encode: "encode package operation reconciliation", begin: "begin package operation reconciliation",
		replay: "commit concurrent package reconciliation replay", audit: "persist package reconciliation audit",
		requeue: "requeue reconciled package operation", result: "read reconciled package update result",
		changed: "package operation changed during reconciliation", commit: "commit package operation reconciliation",
	},
}

var deleteRetry = retryKind{
	command: management.CommandDeleteTenant, digestName: "reconcile_tenant_delete", keyChecked: true,
	contextErr: reconciliationContextError,
	invalid:    "tenant deletion reconciliation command is invalid",
	conflict:   "only uncertain tenant deletion operations can be reconciled",
	stored: func(operation management.Operation) error {
		var command contract.DeleteTenant
		if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateDeleteTenant(command) != nil || command.Meta.IdempotencyKey != operation.IdempotencyKey {
			return contract.NewError(contract.CodeConflict, "only valid tenant deletion commands can be reconciled")
		}
		if _, err := contract.ParseTenantID(string(operation.Tenant)); err != nil {
			return contract.NewError(contract.CodeConflict, "tenant deletion operation has an invalid tenant identity")
		}
		return nil
	},
	text: retryText{
		encode: "encode deletion operation reconciliation", begin: "begin deletion operation reconciliation",
		replay: "commit concurrent deletion reconciliation replay", audit: "persist deletion reconciliation audit",
		requeue: "requeue reconciled tenant deletion operation", result: "read reconciled deletion update result",
		changed: "tenant deletion operation changed during reconciliation", commit: "commit tenant deletion operation reconciliation",
	},
}

// invitationRetry validates its idempotency key only through the command, and
// reports a missing context as a validation failure.
var invitationRetry = retryKind{
	command: management.CommandTenantInvitation, digestName: "reconcile_tenant_invitation",
	contextErr: func() error {
		return contract.NewError(contract.CodeValidationFailed, "management reconciliation requires context")
	},
	invalid:  "tenant invitation reconciliation command is invalid",
	conflict: "only uncertain tenant invitation operations can be reconciled",
	stored: func(operation management.Operation) error {
		var original contract.RequestTenantInvitation
		if err := json.Unmarshal(operation.Command, &original); err != nil || contract.ValidateRequestTenantInvitation(original) != nil || original.Meta.IdempotencyKey != operation.IdempotencyKey {
			return contract.NewError(contract.CodeConflict, "only valid tenant invitation commands can be reconciled")
		}
		if _, err := contract.ParseTenantID(string(operation.Tenant)); err != nil {
			return contract.NewError(contract.CodeConflict, "tenant invitation operation has an invalid tenant identity")
		}
		return nil
	},
	text: retryText{
		encode: "encode invitation reconciliation", begin: "begin invitation reconciliation",
		replay: "commit invitation reconciliation replay", audit: "persist invitation reconciliation audit",
		requeue: "requeue reconciled invitation operation", result: "read reconciled invitation update result",
		changed: "tenant invitation operation changed during reconciliation", commit: "commit tenant invitation reconciliation",
	},
}
