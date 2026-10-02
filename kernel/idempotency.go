package kernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

// Receipt status values for _kora_idempotency_receipt.
const (
	receiptCompleted = "completed"
)

var errConcurrentIdempotencyClaim = contract.NewError(contract.CodeConflict, "concurrent idempotency claim")

// lookupReceipt returns the committed result of a prior operation with the
// same (site, idempotency key). A payload-hash mismatch is a key reuse: the
// caller intended a different operation under a key that is already spent.
func (k *Kernel) lookupReceipt(ctx context.Context, siteDB *sql.DB, op Operation) (contract.CommandResult, bool, *contract.Error) {
	var (
		storedPayloadHash string
		storedResultHash  string
		status            string
		operationID       string
		resultJSON        sql.NullString
	)
	err := siteDB.QueryRowContext(ctx,
		db.Rebind(k.dialect(), `SELECT operation_id, payload_hash, result_hash, status, result_json FROM _kora_idempotency_receipt
		 WHERE site = ? AND idempotency_key = ?`),
		op.Context.Site, op.Context.IdempotencyKey,
	).Scan(&operationID, &storedPayloadHash, &storedResultHash, &status, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.CommandResult{}, false, nil
	}
	if err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	if storedPayloadHash != "" && storedPayloadHash != payloadHash(op.Payload) {
		return contract.CommandResult{}, true, ErrKeyReused
	}
	if status != receiptCompleted || !resultJSON.Valid || resultJSON.String == "" {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeConflict, "the original idempotency result is unavailable; use a new key")
	}
	var result contract.CommandResult
	if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeInternal, "stored idempotency result is invalid")
	}
	if storedResultHash != "" && storedResultHash != resultHash(result) {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeInternal, "stored idempotency result failed integrity check")
	}
	result.OperationID = operationID
	result.Replayed = true
	return result, true, nil
}

// storeReceiptResult stores the exact command envelope before the business
// transaction commits, so a successful mutation can always be replayed.
func (k *Kernel) storeReceiptResult(dbTx *sql.Tx, op Operation, result contract.CommandResult) *contract.Error {
	if op.Context.IdempotencyKey == "" {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding idempotency result failed")
	}
	resultRow, err := dbTx.Exec(
		db.Rebind(k.dialect(), `UPDATE _kora_idempotency_receipt SET result_hash = ?, result_json = ? WHERE site = ? AND idempotency_key = ?`),
		resultHash(result), string(raw), op.Context.Site, op.Context.IdempotencyKey,
	)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "storing idempotency result failed")
	}
	if affected, err := resultRow.RowsAffected(); err != nil || affected != 1 {
		return contract.NewError(contract.CodeInternal, "idempotency receipt was not finalized in the transaction")
	}
	return nil
}

// claimReceipt inserts the receipt inside the active transaction. A primary-
// key collision means a concurrent operation committed the same key first:
// identical payloads are treated as replay; different payloads are reuse.
func (k *Kernel) claimReceipt(dbTx *sql.Tx, op Operation, def CommandDefinition, opID, pHash string) *contract.Error {
	_, err := dbTx.Exec(
		db.Rebind(k.dialect(), `INSERT INTO _kora_idempotency_receipt
			(site, idempotency_key, operation_id, command_name, payload_hash, result_hash, status, actor_user)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?)`),
		op.Context.Site,
		op.Context.IdempotencyKey,
		opID,
		def.Name,
		pHash,
		receiptCompleted,
		op.Context.User,
	)
	if err == nil {
		return nil
	}
	// Unique violation → concurrent duplicate. Distinguish by payload hash.
	var existing string
	scanErr := dbTx.QueryRow(
		db.Rebind(k.dialect(), `SELECT payload_hash FROM _kora_idempotency_receipt
		 WHERE site = ? AND idempotency_key = ?`),
		op.Context.Site, op.Context.IdempotencyKey,
	).Scan(&existing)
	if scanErr == nil && existing != pHash {
		return ErrKeyReused
	}
	return errConcurrentIdempotencyClaim
}
