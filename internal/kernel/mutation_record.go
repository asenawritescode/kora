package kernel

import (
	"context"
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

// remove deletes the record the payload names, with its audit and history rows.
func (m *mutation) remove(ctx context.Context) *contract.Error {
	var p RecordUpdatePayload
	if err := json.Unmarshal(m.op.Payload, &p); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "invalid record.delete payload")
	}
	if p.Name == "" {
		return contract.NewError(contract.CodeValidationFailed, "record.delete missing name")
	}
	scope := scopeForOperation(m.run.Reg, m.op, m.dt.Name, "delete")
	oldDoc, cerr := m.load(p.Name, scope) //nolint:contextcheck // the context travels in txMgr.Context
	if cerr != nil {
		return cerr
	}
	if err := m.run.Mgr.PrepareDelete(m.dt, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if claimErr := m.claim(); claimErr != nil {
		return claimErr
	}
	beforeHash := CanonicalDocHash(oldDoc.Fields)
	if err := m.run.Mgr.DeleteInTxWithScope(m.run.Tx, m.dt, p.Name, scope, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapORMError(err)
	}
	if hookErr := captureLifecycleHooks(ctx, m.run.Mgr, m.run.Tx, m.dt, lifecycleEvent{script.EventAfterDelete, oldDoc, nil}); hookErr != nil {
		return hookErr
	}
	row := m.k.buildAuditRow(m.op, m.def, m.run.OpID, auditOutcome{Doctype: m.dt.Name, DocName: oldDoc.Name, Status: contract.StatusCompleted})
	row.BeforeHash = beforeHash
	auditID, cerr := m.record(row, oldDoc, nil)
	if cerr != nil {
		return cerr
	}
	m.deletedDoc = oldDoc
	m.result = &ResultData{Doctype: m.dt.Name, Name: oldDoc.Name, Deleted: true, AuditID: auditID, Operation: m.run.OpID}
	return nil
}

// update changes the record the payload names and checks the result against
// its schema, workflow status and the caller's expected version.
func (m *mutation) update(ctx context.Context) *contract.Error {
	var p RecordUpdatePayload
	if err := json.Unmarshal(m.op.Payload, &p); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "invalid record.update payload")
	}
	if p.Name == "" {
		return contract.NewError(contract.CodeValidationFailed, "record.update missing name")
	}
	if err := m.k.rejectWorkflowStateMutation(ctx, m.run.Tx, m.op, m.dt, p.Data); err != nil {
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	scope := scopeForOperation(m.run.Reg, m.op, m.dt.Name, "write")
	oldDoc, cerr := m.load(p.Name, scope) //nolint:contextcheck // the context travels in txMgr.Context
	if cerr != nil {
		return cerr
	}
	// Compare the caller's canonical modified-time token while the row is
	// protected by this transaction; Revision is a separate internal counter.
	if versionErr := m.checkVersion(p.Name); versionErr != nil {
		return versionErr
	}
	doc, cerr := m.prepareUpdate(ctx, oldDoc, p.Data)
	if cerr != nil {
		return cerr
	}
	return m.saveUpdate(ctx, doc, oldDoc, scope)
}

// prepareUpdate returns a copy of the record with the payload applied, after
// the hooks, workflow status guard and schema validation have passed.
func (m *mutation) prepareUpdate(ctx context.Context, oldDoc *doctype.Document, data json.RawMessage) (*doctype.Document, *contract.Error) {
	op, dt, txMgr := m.op, m.dt, m.run.Mgr
	doc := cloneDoc(oldDoc)
	if err := applyData(dt, doc, data, m.run.Reg, !op.Context.AllowReadOnlyFields); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if err := dt.BindBusinessScope(doc, op.Context.BusinessScope, false, oldDoc); err != nil {
		return nil, contract.NewError(contract.CodePermissionDenied, err.Error())
	}
	doc.IsNew = false
	if err := txMgr.RunHooksForValidate(dt, doc, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if err := m.k.rejectWorkflowStateChange(ctx, m.run.Tx, op, dt, docChange{oldDoc, doc}); err != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	check := documentCheck{Old: oldDoc, References: op.Context.BusinessReferenceValidator, Snapshots: op.Context.HistoricalSnapshotValidator}
	if verrs := validateOperationDocument(txMgr, dt, doc, m.run.Reg, check); verrs.HasErrors() { //nolint:contextcheck // the context travels in txMgr.Context
		return nil, validationContractError(verrs)
	}
	return doc, nil
}

// saveUpdate persists the updated record with its audit and history rows.
func (m *mutation) saveUpdate(ctx context.Context, doc, oldDoc *doctype.Document, scope policy.RowScope) *contract.Error {
	if err := m.run.Mgr.PrepareSave(m.dt, doc, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return contract.NewError(contract.CodeValidationFailed, err.Error())
	}
	if claimErr := m.claim(); claimErr != nil {
		return claimErr
	}
	beforeHash := CanonicalDocHash(oldDoc.Fields)
	afterHash := CanonicalDocHash(doc.Fields)
	if err := m.run.Mgr.SaveInTxWithScope(m.run.Tx, m.dt, doc, m.user, scope, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapExpectedVersionORMError(err, m.op, m.k.dialect())
	}
	if hookErr := captureLifecycleHooks(ctx, m.run.Mgr, m.run.Tx, m.dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
		return hookErr
	}
	row := m.k.buildAuditRow(m.op, m.def, m.run.OpID, auditOutcome{Doctype: m.dt.Name, DocName: doc.Name, Status: contract.StatusCompleted})
	row.BeforeHash = beforeHash
	row.AfterHash = afterHash
	auditID, cerr := m.record(row, oldDoc, doc)
	if cerr != nil {
		return cerr
	}
	m.result = &ResultData{Doctype: m.dt.Name, Name: doc.Name, Created: false, Document: doc.ToMap(), AuditID: auditID, Operation: m.run.OpID}
	m.savedDoc, m.savedOldDoc = doc, oldDoc
	return nil
}
