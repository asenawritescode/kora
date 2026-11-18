package kernel

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
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
