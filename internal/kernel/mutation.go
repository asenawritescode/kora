package kernel

import (
	"encoding/json"
	"errors"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
)

// mutation is one record command running inside executeInTx. Its steps leave
// the documents they wrote here, and finish completes them once the
// transaction commits.
type mutation struct {
	k      *Kernel
	run    txRun
	op     Operation
	def    CommandDefinition
	dt     *doctype.DocType
	ownsTx bool

	result                *ResultData
	deletedDoc            *doctype.Document
	insertedDocs          []*doctype.Document
	savedDoc, savedOldDoc *doctype.Document
	bundleSavedDocs       []savedBundleDocument
}

// storeResult records the command result on the idempotency receipt claimed
// earlier in the transaction.
func (m *mutation) storeResult() *contract.Error {
	if m.result == nil || m.op.Context.IdempotencyKey == "" || !m.def.IdempotentByKey {
		return nil
	}
	data, err := json.Marshal(m.result)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding command result failed")
	}
	return m.k.storeReceiptResult(m.run.Tx, m.op, contract.CommandResult{
		OperationID: m.run.OpID, CorrelationID: m.op.Context.CorrelationID,
		Status: contract.StatusCompleted, Data: data,
	})
}

// completion returns the post-commit work for the documents the steps wrote.
func (m *mutation) completion() func() {
	txMgr := m.run.Mgr
	nestedPostCommitHooks := append([]func(){}, txMgr.PostCommitHooks...)
	completionManager := *txMgr
	completionManager.ScriptMutationExecutor = nil
	completionManager.PostCommitHooks = nil
	completionManager.Transaction = nil
	return func() {
		for _, hook := range nestedPostCommitHooks {
			hook()
		}
		if m.deletedDoc != nil {
			completionManager.CompleteDelete(m.dt, m.deletedDoc)
		}
		for _, doc := range m.insertedDocs {
			completionManager.CompleteInsert(m.run.Reg.Get(doc.DocType), doc)
		}
		for _, saved := range m.bundleSavedDocs {
			if saved.oldDoc != nil {
				completionManager.CompleteSave(saved.dt, saved.doc, saved.oldDoc)
			}
		}
		if m.savedDoc != nil {
			completionManager.CompleteSave(m.dt, m.savedDoc, m.savedOldDoc)
		}
	}
}

// finish stores the result, then either commits the transaction this run owns
// and completes at once, or hands the completion to the caller's AfterCommit.
func (m *mutation) finish() (*ResultData, *contract.Error) {
	if cerr := m.storeResult(); cerr != nil {
		return nil, cerr
	}
	complete := m.completion()
	if m.ownsTx {
		if err := m.run.Tx.Commit(); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "commit failed")
		}
		m.run.Mgr.ScriptMutationExecutor = nil
		m.run.Mgr.PostCommitHooks = nil
		complete()
	} else if m.run.AfterCommit != nil {
		*m.run.AfterCommit = complete
	}
	return m.result, nil
}

// claim claims the idempotency receipt when the command is idempotent by key.
func (m *mutation) claim() *contract.Error {
	if m.op.Context.IdempotencyKey == "" || !m.def.IdempotentByKey {
		return nil
	}
	return m.k.claimReceipt(m.run.Tx, m.op, m.def, m.run.OpID, payloadHash(m.op.Payload))
}

// load reads the record a command names, mapping a missing row to NOT_FOUND.
func (m *mutation) load(name string, scope policy.RowScope) (*doctype.Document, *contract.Error) {
	doc, err := m.run.Mgr.GetDocWithScope(m.dt, name, scope)
	if err != nil {
		if errors.Is(err, orm.ErrNotFound) {
			return nil, contract.NewError(contract.CodeNotFound, "document not found")
		}
		return nil, contract.NewError(contract.CodeDependencyUnavailable, "load document failed")
	}
	return doc, nil
}

// record writes the audit row and the record history of a completed step and
// returns the audit id.
func (m *mutation) record(row AuditRow, before, after *doctype.Document) (string, *contract.Error) {
	auditID, err := m.k.writeAudit(m.run.Tx, row)
	if err != nil {
		return "", contract.NewError(contract.CodeInternal, "audit write failed")
	}
	if err := m.k.writeRecordHistory(m.run.Tx, row, m.dt, before, after); err != nil {
		return "", contract.NewError(contract.CodeInternal, "record history write failed")
	}
	return auditID, nil
}
