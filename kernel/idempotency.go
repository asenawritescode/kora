package kernel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/policy"
)

// Receipt status values for _kora_idempotency_receipt.
const (
	receiptCompleted = "completed"
)

var errConcurrentIdempotencyClaim = contract.NewError(contract.CodeConflict, "concurrent idempotency claim")

// lookupReceipt returns a prior result scoped to stable tenant, verified
// principal, command and caller key. Legacy receipt lookup is restricted to
// direct human executions whose persisted actor_user still identifies them.
func (k *Kernel) lookupReceipt(ctx context.Context, siteDB *sql.DB, op Operation) (contract.CommandResult, bool, *contract.Error) {
	return k.lookupReceiptFrom(ctx, siteDB, op)
}

// LookupReceiptByIntent recovers a completed receipt without dispatching the
// command. The caller must derive idempotencyKey from its full public intent
// (including scope and operation reference) before calling this method.
func (k *Kernel) LookupReceiptByIntent(ctx context.Context, siteDB *sql.DB, execution policy.Execution, actorUser, command, idempotencyKey string) (contract.CommandResult, bool, *contract.Error) {
	if ctx == nil || siteDB == nil || !execution.Valid() || actorUser == "" || command == "" || idempotencyKey == "" {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeValidationFailed, "receipt lookup identity is invalid")
	}
	actor := execution.Actor()
	op := Operation{Command: command, Context: OperationContext{Site: actor.Site, TenantID: execution.Tenant(), Actor: actor, User: actorUser, Execution: execution, IdempotencyKey: idempotencyKey}}
	key := scopedReceiptKey(op)
	if key == "" {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeUnauthenticated, "receipt lookup requires verified execution identity")
	}
	rows, err := siteDB.QueryContext(ctx, db.Rebind(k.dialect(), `SELECT operation_id, command_name, actor_user, result_hash, status, result_json FROM _kora_idempotency_receipt WHERE idempotency_key = ? LIMIT 2`), key)
	if err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
		}
		return contract.CommandResult{}, false, nil
	}
	var operationID, storedCommand, storedActor, storedHash, status string
	var resultJSON sql.NullString
	if err := rows.Scan(&operationID, &storedCommand, &storedActor, &storedHash, &status, &resultJSON); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	if rows.Next() {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeConflict, "multiple receipts exist for this intent")
	}
	if err := rows.Err(); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	if storedCommand != command || storedActor != actorUser {
		return contract.CommandResult{}, true, contract.NewError(contract.CodePermissionDenied, "receipt intent identity does not match")
	}
	if status != receiptCompleted || !resultJSON.Valid || resultJSON.String == "" {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeOutcomeUnknown, "original operation outcome is not yet recoverable")
	}
	var result contract.CommandResult
	if err := json.Unmarshal([]byte(resultJSON.String), &result); err != nil || storedHash != resultHash(result) {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeInternal, "stored idempotency result failed integrity check")
	}
	result.OperationID, result.Replayed = operationID, true
	return result, true, nil
}

type receiptQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (k *Kernel) lookupReceiptTx(ctx context.Context, tx *sql.Tx, op Operation) (contract.CommandResult, bool, *contract.Error) {
	return k.lookupReceiptFrom(ctx, tx, op)
}

func (k *Kernel) lookupReceiptFrom(ctx context.Context, queryer receiptQueryer, op Operation) (contract.CommandResult, bool, *contract.Error) {
	key := scopedReceiptKey(op)
	if key == "" {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeUnauthenticated, "idempotency receipt requires verified execution identity")
	}
	result, found, err := k.lookupReceiptKey(ctx, queryer, op, key)
	if found || err != nil {
		return result, found, err
	}
	return k.lookupLegacyReceipt(ctx, queryer, op)
}

func (k *Kernel) lookupReceiptKey(ctx context.Context, queryer receiptQueryer, op Operation, key string) (contract.CommandResult, bool, *contract.Error) {
	rows, err := queryer.QueryContext(ctx,
		db.Rebind(k.dialect(), `SELECT operation_id, command_name, actor_user, payload_hash, result_hash, status, result_json FROM _kora_idempotency_receipt
		 WHERE idempotency_key = ? LIMIT 2`), key,
	)
	if err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
		}
		return contract.CommandResult{}, false, nil
	}
	var (
		storedPayloadHash string
		storedResultHash  string
		storedCommand     string
		storedActor       string
		status            string
		operationID       string
		resultJSON        sql.NullString
	)
	if err := rows.Scan(&operationID, &storedCommand, &storedActor, &storedPayloadHash, &storedResultHash, &status, &resultJSON); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	if rows.Next() {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeConflict, "multiple receipts exist for this idempotency key; operator reconciliation is required")
	}
	if err := rows.Err(); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "idempotency lookup failed")
	}
	if storedCommand != op.Command || storedActor != op.Context.User {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeConflict, "idempotency receipt identity does not match the command")
	}
	if storedPayloadHash != "" && storedPayloadHash != payloadHash(op.Payload) && storedPayloadHash != rawPayloadHash(op.Payload) {
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

func (k *Kernel) lookupLegacyReceipt(ctx context.Context, queryer receiptQueryer, op Operation) (contract.CommandResult, bool, *contract.Error) {
	actor := op.Context.Execution.Actor()
	// Old receipts stored only actor_user. Do not infer a delegated, service or
	// shared-principal execution from that lossy identity field.
	if actor.PrincipalType != contract.PrincipalHuman || actor.PrincipalID == "" || (actor.SubjectUserID != "" && actor.PrincipalID != actor.SubjectUserID) || actor.DelegationID != "" || actor.Site != op.Context.Site || actor.PrincipalID != op.Context.User {
		return contract.CommandResult{}, false, nil
	}
	rows, err := queryer.QueryContext(ctx, db.Rebind(k.dialect(), `SELECT operation_id, command_name, actor_user, payload_hash, result_hash, status, result_json
		FROM _kora_idempotency_receipt WHERE idempotency_key = ? AND command_name = ? AND actor_user = ? ORDER BY created_at DESC LIMIT 2`),
		op.Context.IdempotencyKey, op.Command, actor.PrincipalID,
	)
	if err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "legacy idempotency lookup failed")
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "legacy idempotency lookup failed")
		}
		return contract.CommandResult{}, false, nil
	}
	var (
		operationID, command, actorUser             string
		storedPayloadHash, storedResultHash, status string
		resultJSON                                  sql.NullString
	)
	if err := rows.Scan(&operationID, &command, &actorUser, &storedPayloadHash, &storedResultHash, &status, &resultJSON); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "legacy idempotency lookup failed")
	}
	if rows.Next() {
		return contract.CommandResult{}, true, contract.NewError(contract.CodeConflict, "multiple legacy receipts exist for this idempotency key; operator reconciliation is required")
	}
	if err := rows.Err(); err != nil {
		return contract.CommandResult{}, false, contract.NewError(contract.CodeDependencyUnavailable, "legacy idempotency lookup failed")
	}
	if command != op.Command || actorUser != actor.PrincipalID {
		return contract.CommandResult{}, false, nil
	}
	if storedPayloadHash != "" && storedPayloadHash != payloadHash(op.Payload) && storedPayloadHash != rawPayloadHash(op.Payload) {
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

func scopedReceiptKey(op Operation) string {
	if op.Context.IdempotencyKey == "" || !op.Context.Execution.Valid() || op.Command == "" {
		return ""
	}
	actor := op.Context.Execution.Actor()
	identity, err := json.Marshal([]string{
		string(op.Context.Execution.Tenant()), string(actor.PrincipalType), actor.PrincipalID, op.Command, op.Context.IdempotencyKey,
	})
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(identity)
	return "scope-v2:" + hex.EncodeToString(hash[:])
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
		resultHash(result), string(raw), receiptStorageTenant(op), scopedReceiptKey(op),
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
	key := scopedReceiptKey(op)
	if key == "" {
		return contract.NewError(contract.CodeUnauthenticated, "idempotency receipt requires verified execution identity")
	}
	_, err := dbTx.Exec(
		db.Rebind(k.dialect(), `INSERT INTO _kora_idempotency_receipt
			(site, idempotency_key, operation_id, command_name, payload_hash, result_hash, status, actor_user)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?)`),
		receiptStorageTenant(op),
		key,
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
	var existing, command, actor string
	scanErr := dbTx.QueryRow(
		db.Rebind(k.dialect(), `SELECT payload_hash, command_name, actor_user FROM _kora_idempotency_receipt
		 WHERE idempotency_key = ?`), key,
	).Scan(&existing, &command, &actor)
	if scanErr == nil && (existing != pHash || command != def.Name || actor != op.Context.User) {
		return ErrKeyReused
	}
	return errConcurrentIdempotencyClaim
}

// receiptStorageTenant stores new scoped receipts under stable tenant identity.
// The legacy column is named "site", but v2 receipts must survive route renames.
func receiptStorageTenant(op Operation) string {
	if op.Context.Execution.Valid() {
		return string(op.Context.Execution.Tenant())
	}
	return ""
}
