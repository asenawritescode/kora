package kernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/script"
)

// payloadDoctype extracts the target doctype from either typed payload.
func payloadDoctype(raw json.RawMessage) string {
	var probe struct {
		Doctype string `json:"doctype"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Doctype
}

// executeInTx runs mutation + receipt claim + audit write inside ONE SQL
// transaction (the mutation itself writes its outbox rows via orm InTx
// variants). Any failure rolls back everything; a commit implies all of it.
func (k *Kernel) executeInTx(ctx context.Context, run txRun, op Operation, def CommandDefinition) (*ResultData, *contract.Error) {
	if op.Context.HistoricalSnapshotValidator == nil {
		op.Context.HistoricalSnapshotValidator = newHistoricalSnapshotValidator(op.Context.BusinessScope, k.CommandSchemas)
	}
	ownsTransaction := run.Tx == nil
	auditFailure := func(code contract.Code) {
		if ownsTransaction {
			k.writeFailureAudit(run.SiteDB, op, def, run.OpID, code)
		}
	}
	run, rollback, beginErr := run.begin(ctx)
	if beginErr != nil {
		return nil, beginErr
	}
	defer rollback()
	dbTx, txMgr, reg := run.Tx, run.Mgr, run.Reg
	if admissionErr := enforceModuleLifecycleAdmissionTx(ctx, dbTx, op, admissionScope{Dialect: k.dialect(), Registry: reg}); admissionErr != nil {
		return nil, admissionErr
	}
	priorMutationExecutor := txMgr.ScriptMutationExecutor
	priorPostCommitHooks := txMgr.PostCommitHooks
	priorTransaction := txMgr.Transaction
	priorReferenceValidator, priorSnapshotValidator := txMgr.BusinessReferenceValidator, txMgr.HistoricalSnapshotValidator
	txMgr.BusinessReferenceValidator = op.Context.BusinessReferenceValidator
	txMgr.HistoricalSnapshotValidator = op.Context.HistoricalSnapshotValidator
	priorEventOrigin := txMgr.EventOrigin
	txMgr.Transaction = dbTx
	txMgr.EventOrigin = eventOrigin(op)
	txMgr.PostCommitHooks = nil
	txMgr.ScriptMutationExecutor = script.MutationExecutorFunc(func(nestedCtx context.Context, request script.MutationRequest) (json.RawMessage, error) {
		return k.executeScriptMutationInTx(nestedCtx, dbTx, txMgr, request)
	})
	defer func() {
		txMgr.ScriptMutationExecutor = priorMutationExecutor
		txMgr.PostCommitHooks = priorPostCommitHooks
		txMgr.Transaction = priorTransaction
		txMgr.EventOrigin = priorEventOrigin
		txMgr.BusinessReferenceValidator, txMgr.HistoricalSnapshotValidator = priorReferenceValidator, priorSnapshotValidator
	}()

	doctypeName := payloadDoctype(op.Payload)
	dt := reg.Get(doctypeName)
	if dt == nil {
		return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", doctypeName))
	}
	if err := txMgr.LoadLifecycleHooksTx(ctx, dbTx, dt); err != nil {
		return nil, contract.NewError(contract.CodeDependencyUnavailable, err.Error())
	}

	user := op.Context.User
	if user == "" {
		user = "system"
	}
	owner := op.Context.Owner
	if owner == "" {
		owner = user
	}

	m := &mutation{k: k, run: run, op: op, def: def, dt: dt, user: user, owner: owner, ownsTx: ownsTransaction, auditFailure: auditFailure}

	var stepErr *contract.Error
	switch op.Command {
	case CommandRecordMutateBundle:
		stepErr = m.bundle(ctx)

	case CommandRecordCreate, CommandPublicFormSubmit:
		stepErr = m.create(ctx)

	case CommandRecordUpdate:
		stepErr = m.update(ctx)

	case CommandRecordDelete:
		stepErr = m.remove(ctx)

	case CommandWorkflowStatusTransition:
		stepErr = m.transition(ctx)

	default:
		return nil, contract.NewError(contract.CodeValidationFailed, "unknown command "+op.Command)
	}
	if stepErr != nil {
		return nil, stepErr
	}
	return m.finish() //nolint:contextcheck // the context travels in txMgr.Context
}
