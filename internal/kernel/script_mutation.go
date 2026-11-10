package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

// executeScriptMutationInTx executes script-provider CRUD as a nested kernel
// command using the parent operation's transaction. Nested records therefore
// share commit/rollback with the write whose lifecycle hook requested them.
func (k *Kernel) executeScriptMutationInTx(ctx context.Context, dbTx *sql.Tx, txMgr *orm.TxManager, request script.MutationRequest) (json.RawMessage, error) {
	if request.Command != CommandRecordCreate && request.Command != CommandRecordUpdate && request.Command != CommandRecordDelete {
		return nil, contract.NewError(contract.CodeValidationFailed, "unsupported nested script mutation")
	}
	definition, ok := LookupCommand(request.Command)
	if !ok {
		return nil, contract.NewError(contract.CodeValidationFailed, "unknown nested script command")
	}
	actorType := contract.PrincipalType(request.PrincipalType)
	op := Operation{
		Context: OperationContext{
			Site: request.Site, TenantID: txMgr.Execution.Tenant(), Execution: txMgr.Execution, BusinessScope: txMgr.BusinessScope,
			BusinessReferenceValidator: txMgr.BusinessReferenceValidator, HistoricalSnapshotValidator: txMgr.HistoricalSnapshotValidator, Actor: contract.ActorContext{
				PrincipalID: request.PrincipalID, PrincipalType: actorType,
				SubjectUserID: request.SubjectUserID, Site: request.Site, Roles: append([]string(nil), request.Roles...),
			},
			User: request.User, Owner: request.Owner, UserRole: request.UserRole,
			Roles: append([]string(nil), request.Roles...), SkipHookScripts: append([]string(nil), request.SkipHookScripts...),
			AllowReadOnlyFields: request.AllowReadOnly, Source: SourceIntegration,
		},
		Command: request.Command, Payload: request.Payload,
	}
	if !op.Context.Execution.Valid() {
		return nil, contract.NewError(contract.CodeUnauthenticated, "nested script mutation requires verified execution identity")
	}
	if err := normalizeVerifiedExecution(&op); err != nil {
		return nil, err
	}
	if err := k.validateContext(op); err != nil {
		return nil, err
	}
	if authzErr := authorizeOp(txMgr.Registry, op, definition, nil); authzErr != nil {
		return nil, authzErr
	}
	if admissionErr := enforceModuleLifecycleAdmissionTx(ctx, txMgr.Transaction, op, admissionScope{Dialect: k.dialect(), Registry: txMgr.Registry}); admissionErr != nil {
		return nil, admissionErr
	}
	doctypeName := payloadDoctype(op.Payload)
	dt := txMgr.Registry.Get(doctypeName)
	if dt == nil {
		return nil, contract.NewError(contract.CodeNotFound, fmt.Sprintf("doctype %q not found", doctypeName))
	}
	if request.User == "" {
		op.Context.User = "system"
	}
	opID := contract.NewOperationID()
	oldSkipHooks := txMgr.SkipHookScripts
	oldUser, oldRole, oldRoles, oldContext := txMgr.CurrentUser, txMgr.CurrentUserRole, txMgr.CurrentUserRoles, txMgr.Context
	txMgr.SkipHookScripts = append([]string(nil), request.SkipHookScripts...)
	txMgr.CurrentUser, txMgr.CurrentUserRole = op.Context.User, request.UserRole
	txMgr.CurrentUserRoles = append([]string(nil), request.Roles...)
	txMgr.Context = ctx
	defer func() {
		txMgr.SkipHookScripts = oldSkipHooks
		txMgr.CurrentUser, txMgr.CurrentUserRole = oldUser, oldRole
		txMgr.CurrentUserRoles, txMgr.Context = oldRoles, oldContext
	}()

	var result ResultData
	switch op.Command {
	case CommandRecordCreate:
		var payload RecordCreatePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid nested record.create payload")
		}
		doc := doctype.NewDocument(dt.Name)
		if err := applyData(dt, doc, payload.Data, txMgr.Registry, false); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		setDefaultValues(dt, doc)
		if err := txMgr.RunHooksForValidate(dt, doc, nil); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if validation := validateOperationDocument(txMgr, dt, doc, txMgr.Registry, documentCheck{Old: nil, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); validation.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
			return nil, validationContractError(validation)
		}
		if err := txMgr.PrepareInsert(dt, doc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		afterHash := CanonicalDocHash(doc.Fields)
		owner := op.Context.Owner
		if owner == "" {
			owner = op.Context.User
		}
		if err := txMgr.InsertInTx(dbTx, dt, doc, owner, op.Context.User); err != nil {
			slog.Error("authorized nested script record create failed", "doctype", dt.Name, "error", err)
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterInsert, doc, nil}); hookErr != nil {
			return nil, hookErr
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, nil}); hookErr != nil {
			return nil, hookErr
		}
		doc.IsNew = false
		row := k.buildAuditRow(op, definition, opID, auditOutcome{Doctype: dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
		row.AfterHash = afterHash
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, nil, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script history write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: doc.Name, Created: true, Document: doc.ToMap(), Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		completeTx.Transaction = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteInsert(dt, doc) })

	case CommandRecordUpdate:
		var payload RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid nested record.update payload")
		}
		scope := scopeForOperation(txMgr.Registry, op, dt.Name, "write")
		oldDoc, err := txMgr.GetDocWithScope(dt, payload.Name, scope)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load nested script document failed")
		}
		doc := cloneDoc(oldDoc)
		if err := applyData(dt, doc, payload.Data, txMgr.Registry, !op.Context.AllowReadOnlyFields); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := txMgr.RunHooksForValidate(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if validation := validateOperationDocument(txMgr, dt, doc, txMgr.Registry, documentCheck{Old: oldDoc, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}); validation.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
			return nil, validationContractError(validation)
		}
		if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		beforeHash, afterHash := CanonicalDocHash(oldDoc.Fields), CanonicalDocHash(doc.Fields)
		if err := txMgr.SaveInTxWithScope(dbTx, dt, doc, op.Context.User, scope, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, definition, opID, auditOutcome{Doctype: dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
		row.BeforeHash, row.AfterHash = beforeHash, afterHash
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, doc); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script history write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: doc.Name, Document: doc.ToMap(), Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		completeTx.Transaction = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(dt, doc, oldDoc) })

	case CommandRecordDelete:
		var payload RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid nested record.delete payload")
		}
		scope := scopeForOperation(txMgr.Registry, op, dt.Name, "delete")
		oldDoc, err := txMgr.GetDocWithScope(dt, payload.Name, scope)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load nested script document failed")
		}
		if err := txMgr.PrepareDelete(dt, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := txMgr.DeleteInTxWithScope(dbTx, dt, payload.Name, scope, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterDelete, oldDoc, nil}); hookErr != nil {
			return nil, hookErr
		}
		row := k.buildAuditRow(op, definition, opID, auditOutcome{Doctype: dt.Name, DocName: oldDoc.Name, Status: contract.StatusCompleted})
		row.BeforeHash = CanonicalDocHash(oldDoc.Fields)
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		if err := k.writeRecordHistory(dbTx, row, dt, oldDoc, nil); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script history write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: oldDoc.Name, Deleted: true, Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		completeTx.Transaction = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteDelete(dt, oldDoc) })
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, contract.NewError(contract.CodeInternal, "encode nested script result failed")
	}
	return encoded, nil
}

// ExecuteScriptMutationInTx is the narrow worker seam for script-provider
// writes inside an already-owned operation transaction. It cannot open a new
// transaction or bypass the kernel's authorization and domain validation.
func (k *Kernel) ExecuteScriptMutationInTx(ctx context.Context, dbTx *sql.Tx, txMgr *orm.TxManager, request script.MutationRequest) (json.RawMessage, error) {
	if ctx == nil || dbTx == nil || txMgr == nil || txMgr.Transaction != dbTx {
		return nil, contract.NewError(contract.CodeValidationFailed, "script mutation requires the active kernel transaction")
	}
	return k.executeScriptMutationInTx(ctx, dbTx, txMgr, request)
}
