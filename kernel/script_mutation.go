package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
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
			Site: request.Site, Actor: contract.ActorContext{
				PrincipalID: request.PrincipalID, PrincipalType: actorType,
				SubjectUserID: request.SubjectUserID, Site: request.Site, Roles: append([]string(nil), request.Roles...),
			},
			User: request.User, Owner: request.Owner, UserRole: request.UserRole,
			Roles: append([]string(nil), request.Roles...), SkipHookScripts: append([]string(nil), request.SkipHookScripts...),
			AllowReadOnlyFields: request.AllowReadOnly, Source: SourceIntegration,
		},
		Command: request.Command, Payload: request.Payload,
	}
	if err := k.validateContext(op); err != nil {
		return nil, err
	}
	if authzErr := authorizeOp(txMgr.Registry, op, definition, nil); authzErr != nil {
		return nil, authzErr
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
		if validation := validateOperationDocument(txMgr, dt, doc, txMgr.Registry, nil); validation.HasErrors() {
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
			return nil, wrapORMError(err)
		}
		doc.IsNew = false
		row := k.buildAuditRow(op, definition, opID, dt.Name, doc.Name, contract.StatusCompleted, "")
		row.AfterHash = afterHash
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: doc.Name, Created: true, Document: doc.ToMap(), Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteInsert(dt, doc) })

	case CommandRecordUpdate:
		var payload RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid nested record.update payload")
		}
		owner := ownerForOperation(txMgr.Registry, op, dt.Name, "write")
		oldDoc, err := txMgr.GetDoc(dt, payload.Name, owner)
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
		if validation := validateOperationDocument(txMgr, dt, doc, txMgr.Registry, oldDoc); validation.HasErrors() {
			return nil, validationContractError(validation)
		}
		if err := txMgr.PrepareSave(dt, doc, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		beforeHash, afterHash := CanonicalDocHash(oldDoc.Fields), CanonicalDocHash(doc.Fields)
		if err := txMgr.SaveInTx(dbTx, dt, doc, op.Context.User, owner, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		row := k.buildAuditRow(op, definition, opID, dt.Name, doc.Name, contract.StatusCompleted, "")
		row.BeforeHash, row.AfterHash = beforeHash, afterHash
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: doc.Name, Document: doc.ToMap(), Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(dt, doc, oldDoc) })

	case CommandRecordDelete:
		var payload RecordUpdatePayload
		if err := json.Unmarshal(op.Payload, &payload); err != nil || payload.Name == "" {
			return nil, contract.NewError(contract.CodeValidationFailed, "invalid nested record.delete payload")
		}
		owner := ownerForOperation(txMgr.Registry, op, dt.Name, "delete")
		oldDoc, err := txMgr.GetDoc(dt, payload.Name, owner)
		if err != nil {
			if errors.Is(err, orm.ErrNotFound) {
				return nil, contract.NewError(contract.CodeNotFound, "document not found")
			}
			return nil, contract.NewError(contract.CodeDependencyUnavailable, "load nested script document failed")
		}
		if err := txMgr.PrepareDelete(dt, oldDoc); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
		}
		if err := txMgr.DeleteInTx(dbTx, dt, payload.Name, owner, oldDoc); err != nil {
			return nil, wrapORMError(err)
		}
		row := k.buildAuditRow(op, definition, opID, dt.Name, oldDoc.Name, contract.StatusCompleted, "")
		row.BeforeHash = CanonicalDocHash(oldDoc.Fields)
		if _, err := k.writeAudit(dbTx, row); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "nested script audit write failed")
		}
		result = ResultData{Doctype: dt.Name, Name: oldDoc.Name, Deleted: true, Operation: opID}
		completeTx := *txMgr
		completeTx.ScriptMutationExecutor = nil
		completeTx.SkipHookScripts = nil
		txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteDelete(dt, oldDoc) })
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, contract.NewError(contract.CodeInternal, "encode nested script result failed")
	}
	return encoded, nil
}
