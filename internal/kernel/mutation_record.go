package kernel

import (
	"context"
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
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
