package kernel

import (
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// validateContext enforces fail-closed identity: no site or no authenticated
// principal means no execution. A mismatch between the executing site and the
// actor's home site is a cross-tenant attempt and is rejected.
func (k *Kernel) validateContext(op Operation) *contract.Error {
	if op.Context.Site == "" {
		return ErrNoSite
	}
	if !op.Context.Actor.Authenticated() {
		return ErrUnauthenticated
	}
	if op.Context.Actor.Site != "" && op.Context.Actor.Site != op.Context.Site {
		return ErrPermission
	}
	return nil
}

// authorize evaluates the site registry's permission matrix identically for
// every source. AI/MCP actors carry resolved human subjects; the gate never
// special-cases them (authorization parity, KERNEL-004 / SEC-003).
func authorize(reg *doctype.Registry, op Operation, def CommandDefinition) *contract.Error {
	if reg == nil {
		return ErrPermission
	}
	doctypeName := payloadDoctype(op.Payload)
	if doctypeName == "" {
		return contract.NewError(contract.CodeValidationFailed, "payload missing doctype")
	}
	if resolved := reg.Get(doctypeName); resolved != nil {
		doctypeName = resolved.Name
		if resolved.AppendOnly && op.Command != CommandRecordCreate && op.Command != CommandPublicFormSubmit && op.Command != CommandRecordMutateBundle {
			return contract.NewError(contract.CodeValidationFailed, "append-only records cannot be changed")
		}
	}
	permission, err := bundleAuthorizationPermission(op, reg, doctypeName, def.AuthorizesWith)
	if err != nil {
		return err
	}
	target := reg.Get(doctypeName)
	if op.lifecycleEffectAuthorized(target) {
		return nil
	}
	_, err = decideOperationResource(op, reg, target, permission)
	return err
}

// bundleAuthorizationPermission returns the permission a command is authorized by. A
// record.mutate_bundle takes it from its first record, after a submit check when that
// record also transitions; every other command keeps the command's own permission.
func bundleAuthorizationPermission(op Operation, reg *doctype.Registry, doctypeName, permission string) (string, *contract.Error) {
	var bundle RecordMutationBundlePayload
	if op.Command == CommandRecordMutateBundle && json.Unmarshal(op.Payload, &bundle) == nil && len(bundle.Records) > 0 {
		first := bundle.Records[0]
		permission = first.Operation
		if permission == "" {
			permission = "create"
		}
		if permission == "update" && first.TransitionAction != "" {
			if _, err := decideOperationResource(op, reg, reg.Get(doctypeName), "submit"); err != nil {
				return "", err
			}
		}
	}
	return permission, nil
}

// lifecycleEffectAuthorized reports whether a lifecycle script's create or update of target
// is authorized by the source record's permission rather than the target's role matrix
// (see OperationContext.lifecycleEffect). Entity-owned targets never get this elevation.
func (op Operation) lifecycleEffectAuthorized(target *doctype.DocType) bool {
	if !op.Context.lifecycleEffect || target == nil || target.BusinessScopePolicy == contract.ScopeEntityOwned {
		return false
	}
	return op.Command == CommandRecordCreate || op.Command == CommandRecordUpdate
}

func decideOperationResource(op Operation, reg *doctype.Registry, dt *doctype.DocType, operation string) (policy.Decision, *contract.Error) {
	if dt == nil {
		return policy.Decision{}, contract.NewError(contract.CodeNotFound, "resource not found")
	}
	return policy.DecideForBusinessScope(op.Context.Execution, reg, dt.Name, operation, op.Context.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
}
