package kernel

import (
	"context"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/script"
)

// commandWriter writes the records a pack command creates or updates, inside the
// command's transaction. It runs the same status-gated quantity effect as the
// record-command path (mutation), so a record written by a command moves target quantities
// exactly as a record written through record.create or record.update does.
type commandWriter struct {
	k    *Kernel
	run  txRun
	op   Operation
	def  CommandDefinition
	user string
}

// insert creates a record the command names, runs its insert hooks, and applies
// the status effect of the new record.
func (w commandWriter) insert(ctx context.Context, dt *doctype.DocType, doc *doctype.Document) *contract.Error {
	// A gated record with no status (none sent and no default) is refused, as a
	// record.create is, rather than left silently out of its target quantity.
	if _, cerr := createStatusApplies(dt, doc.Fields); cerr != nil {
		return cerr
	}
	txMgr := w.run.Mgr
	if err := txMgr.InsertInTx(w.run.Tx, dt, doc, commandOwner(w.op, w.user), w.user); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapORMError(err, w.k.dialect())
	}
	if hookErr := captureInsertHooks(ctx, txMgr, w.run.Tx, dt, doc); hookErr != nil {
		return hookErr
	}
	return applyGatedEffect(ctx, w.effects(), dt, nil, doc)
}

// save saves a changed record the command names, runs its after-save hooks, and
// applies the change of its status effect: target quantities move when the status enters or
// leaves the applying set, and the lines may not change while it stays inside.
func (w commandWriter) save(ctx context.Context, dt *doctype.DocType, doc, oldDoc *doctype.Document, scope policy.RowScope) *contract.Error {
	txMgr := w.run.Mgr
	if err := txMgr.SaveInTxWithScope(w.run.Tx, dt, doc, w.user, scope, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapExpectedVersionORMError(err, w.op, w.k.dialect())
	}
	if cerr := applyGatedEffect(ctx, w.effects(), dt, oldDoc, doc); cerr != nil {
		return cerr
	}
	if hookErr := captureLifecycleHooks(ctx, txMgr, w.run.Tx, dt, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
		return hookErr
	}
	return nil
}

// effects returns the effect writer of this command's writes.
func (w commandWriter) effects() effectWriter {
	return effectWriter{k: w.k, run: w.run, op: w.op, owner: commandOwner(w.op, w.user), user: w.user, writeTarget: w.saveTarget}
}

// saveTarget saves a target record a status effect moved, with its audit and
// history rows, and completes it after commit as a command save does.
func (w commandWriter) saveTarget(ctx context.Context, target *doctype.DocType, doc, oldDoc *doctype.Document) *contract.Error {
	txMgr := w.run.Mgr
	beforeHash := CanonicalDocHash(oldDoc.Fields)
	scope := scopeForOperation(w.run.Reg, w.op, target.Name, "write")
	if err := txMgr.SaveInTxWithScope(w.run.Tx, target, doc, w.user, scope, oldDoc); err != nil { //nolint:contextcheck // the context travels in txMgr.Context
		return wrapORMError(err, w.k.dialect())
	}
	if hookErr := captureLifecycleHooks(ctx, txMgr, w.run.Tx, target, lifecycleEvent{script.EventAfterSave, doc, oldDoc}); hookErr != nil {
		return hookErr
	}
	row := w.k.buildAuditRow(w.op, w.def, w.run.OpID, auditOutcome{Doctype: target.Name, DocName: doc.Name, Status: contract.StatusCompleted})
	row.BeforeHash = beforeHash
	if _, cerr := w.k.writeRecordAudit(w.run.Tx, row, target, oldDoc, doc); cerr != nil {
		return cerr
	}
	completeTx := postCommitCopy(txMgr)
	txMgr.PostCommitHooks = append(txMgr.PostCommitHooks, func() { completeTx.CompleteSave(target, doc, oldDoc) }) //nolint:contextcheck // runs after commit, outside the request context
	return nil
}
