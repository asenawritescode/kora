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
	permission := def.AuthorizesWith
	var bundle RecordMutationBundlePayload
	if op.Command == CommandRecordMutateBundle && json.Unmarshal(op.Payload, &bundle) == nil && len(bundle.Records) > 0 {
		permission = bundle.Records[0].Operation
		if permission == "" {
			permission = "create"
		}
		if permission == "update" && bundle.Records[0].TransitionAction != "" {
			if _, err := decideOperationResource(op, reg, reg.Get(doctypeName), "submit"); err != nil {
				return err
			}
		}
	}
	_, err := decideOperationResource(op, reg, reg.Get(doctypeName), permission)
	return err
}

func decideOperationResource(op Operation, reg *doctype.Registry, dt *doctype.DocType, operation string) (policy.Decision, *contract.Error) {
	if dt == nil {
		return policy.Decision{}, contract.NewError(contract.CodeNotFound, "resource not found")
	}
	return policy.DecideForBusinessScope(op.Context.Execution, reg, dt.Name, operation, op.Context.BusinessScope, dt.BusinessScopePolicy == contract.ScopeEntityOwned)
}
