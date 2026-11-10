package kernel

import (
	"context"
	"database/sql"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

// txRun is the transactional environment one command executes in.
type txRun struct {
	SiteDB *sql.DB
	// Tx is the caller's transaction; nil makes the executor open, own and
	// commit-or-roll-back its own.
	Tx          *sql.Tx
	Mgr         *orm.TxManager
	Reg         *doctype.Registry
	OpID        string
	AfterCommit *func()
}

// begin returns the run bound to its transaction, and a function that rolls
// the transaction back unless it was committed. A transaction the caller
// supplied is left to the caller.
func (r txRun) begin(ctx context.Context) (txRun, func(), *contract.Error) {
	if r.Tx != nil {
		return r, func() {}, nil
	}
	tx, err := r.SiteDB.BeginTx(ctx, nil)
	if err != nil {
		return r, nil, contract.NewError(contract.CodeDependencyUnavailable, "begin transaction: "+err.Error())
	}
	r.Tx = tx
	return r, func() { _ = tx.Rollback() }, nil
}

// lifecycleEvent is one document event whose async after-hooks are captured.
type lifecycleEvent struct {
	Event  script.Event
	Doc    *doctype.Document
	OldDoc *doctype.Document
}

func captureLifecycleHooks(ctx context.Context, txMgr *orm.TxManager, tx *sql.Tx, dt *doctype.DocType, ev lifecycleEvent) *contract.Error {
	if err := txMgr.CaptureAsyncAfterHooksTx(ctx, tx, dt, ev.Event, ev.Doc, ev.OldDoc); err != nil {
		return contract.NewError(contract.CodeDependencyUnavailable, "persisting async lifecycle hooks failed: "+err.Error())
	}
	return nil
}

// eventOrigin carries the operation's authenticated cause onto the change
// events its writes append.
func eventOrigin(op Operation) *orm.EventOrigin {
	return &orm.EventOrigin{Actor: op.Context.Actor, CorrelationID: op.Context.CorrelationID, CausationID: op.Context.CausationID, CausationDepth: op.Context.CausationDepth}
}

// documentCheck is what validating a document needs beyond the document: its
// prior state on update, and the operation's business-reference and
// historical-snapshot validators.
type documentCheck struct {
	Old        *doctype.Document
	References contract.BusinessReferenceValidator
	Snapshots  contract.HistoricalSnapshotValidator
}

func validateOperationDocument(txMgr *orm.TxManager, dt *doctype.DocType, doc *doctype.Document, reg *doctype.Registry, check documentCheck) doctype.ValidationErrors {
	resolver := func(target *doctype.DocType, name string) (*doctype.Document, error) {
		return txMgr.GetDoc(target, name, "")
	}
	return doctype.ValidateDocumentWithResolvers(dt, doc, reg, check.Old, resolver, check.References, check.Snapshots)
}
