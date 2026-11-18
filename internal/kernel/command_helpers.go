package kernel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

// lockAdjustTarget locks the row an adjusting update changes, so concurrent
// adjustments serialise on the stored value.
func lockAdjustTarget(dbTx *sql.Tx, dialect db.Dialect, dt *doctype.DocType, name string) *contract.Error {
	if err := lockRecordRow(dbTx, dialect, dt, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contract.NewError(contract.CodeNotFound, fmt.Sprintf("record %q not found", name))
		}
		return contract.NewError(contract.CodeDependencyUnavailable, "lock document failed")
	}
	return nil
}

// claimFirstWriteReceipt claims the idempotency receipt when a keyed command
// makes its first write; later writes of the same command already hold it.
func (k *Kernel) claimFirstWriteReceipt(dbTx *sql.Tx, op Operation, def CommandDefinition, opID string, writes int) *contract.Error {
	if op.Context.IdempotencyKey == "" || !def.IdempotentByKey || writes > 0 {
		return nil
	}
	return k.claimReceipt(dbTx, op, def, opID, payloadHash(op.Payload))
}

// postCommitCopy returns a copy of the transaction manager for hooks that run
// after commit, without the state tied to the open transaction.
func postCommitCopy(txMgr *orm.TxManager) *orm.TxManager {
	completeTx := *txMgr
	completeTx.ScriptMutationExecutor = nil
	completeTx.PostCommitHooks = nil
	completeTx.Transaction = nil
	completeTx.SkipHookScripts = nil
	return &completeTx
}

// createDocumentValues builds the document a create step writes from its
// values. Absent references are skipped; table values become child documents.
func createDocumentValues(dt *doctype.DocType, values map[string]string, env refEnv, i int) (*doctype.Document, *contract.Error) {
	doc := doctype.NewDocument(dt.Name)
	for field, ref := range values {
		v, rerr := resolveEnvRef(ref, env)
		if errors.Is(rerr, errRefAbsent) {
			continue
		}
		if rerr != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, fmt.Sprintf("step %d: %v", i, rerr))
		}
		if tf := dt.GetField(field); tf != nil && tf.Fieldtype == "Table" {
			if rows, ok := v.([]any); ok {
				v = childDocuments(tf.Options, rows)
			}
		}
		doc.Set(field, v)
	}
	return doc, nil
}

// commandOwner returns the record owner a command writes: the operation's
// owner when it names one, otherwise the acting user.
func commandOwner(op Operation, user string) string {
	if op.Context.Owner != "" {
		return op.Context.Owner
	}
	return user
}

// captureInsertHooks runs the after-insert and after-save hooks of a new record.
func captureInsertHooks(ctx context.Context, txMgr *orm.TxManager, dbTx *sql.Tx, dt *doctype.DocType, doc *doctype.Document) *contract.Error {
	if hookErr := captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterInsert, doc, nil}); hookErr != nil {
		return hookErr
	}
	return captureLifecycleHooks(ctx, txMgr, dbTx, dt, lifecycleEvent{script.EventAfterSave, doc, nil})
}
