package configstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

// PublishedOperationAttempt is the durable public identity and provider pin
// for one operation intent. Raw idempotency keys are never persisted.
type PublishedOperationAttempt struct {
	Receipt    contract.OperationReceipt
	IntentHash string
	InputHash  string
	Dispatch   *contract.PinnedDispatch
	Lease      *contract.CapabilityLease
	Claimed    bool
}

// EnsurePublishedOperationReceiptSchema creates the actor and exact-scope
// receipt projection used by the public execution lookup routes.
func (s *ModuleStore) EnsurePublishedOperationReceiptSchema(ctx context.Context) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil {
		return fmt.Errorf("published operation receipt schema dependencies are required")
	}
	query := `CREATE TABLE IF NOT EXISTS _kora_published_operation_receipt (tenant_id VARCHAR(140) NOT NULL, execution_id VARCHAR(160) NOT NULL, actor_type VARCHAR(40) NOT NULL, actor_id VARCHAR(160) NOT NULL, intent_hash VARCHAR(64) NOT NULL, input_hash VARCHAR(64) NOT NULL, state VARCHAR(32) NOT NULL, dispatch_state VARCHAR(24) NOT NULL, operation_ref VARCHAR(256) NOT NULL, scope_json TEXT NOT NULL, dispatch_json TEXT NOT NULL, lease_json TEXT NOT NULL, receipt_json TEXT NOT NULL, PRIMARY KEY (tenant_id, execution_id), UNIQUE (tenant_id, actor_type, actor_id, intent_hash))`
	if driver, ok := s.Dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "mysql" {
		query += ` ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	_, err := s.DB.ExecContext(ctx, query)
	return err
}

func PublishedOperationInputHash(input []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err == nil {
		if canonical, marshalErr := json.Marshal(value); marshalErr == nil {
			input = canonical
		}
	}
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

// BeginPublishedOperationAttempt atomically claims a public intent before any
// provider call. Repeated identical intents return the original execution and
// pin; changed payloads under the same scoped key conflict.
func (s *ModuleStore) BeginPublishedOperationAttempt(ctx context.Context, actor contract.ActorContext, attempt PublishedOperationAttempt) (PublishedOperationAttempt, bool, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || !actor.Authenticated() || attempt.Receipt.Validate() != nil || attempt.IntentHash == "" || len(attempt.IntentHash) != 64 || attempt.InputHash == "" || len(attempt.InputHash) != 64 {
		return PublishedOperationAttempt{}, false, fmt.Errorf("published operation attempt identity is invalid")
	}
	if !isCanonicalHex(attempt.IntentHash, 32) || !isCanonicalHex(attempt.InputHash, 32) {
		return PublishedOperationAttempt{}, false, fmt.Errorf("published operation intent and input hashes must be canonical SHA-256 values")
	}
	if attempt.Receipt.Status != contract.ReceiptRunning || (attempt.Dispatch == nil) != (attempt.Lease == nil) || attempt.Dispatch == nil || attempt.Dispatch.Validate() != nil || attempt.Lease.Validate() != nil || attempt.Dispatch.ExecutionID != string(attempt.Receipt.ExecutionID) || attempt.Dispatch.Scope != attempt.Receipt.BusinessScope || attempt.Dispatch.InputHash != attempt.InputHash || attempt.Dispatch.ProviderModuleID == "" || attempt.Dispatch.ProviderModuleID != attempt.Lease.ProviderModuleID || attempt.Dispatch.ProviderID != attempt.Lease.ProviderID || attempt.Dispatch.CallerModuleID != attempt.Lease.CallerModuleID || attempt.Dispatch.BindingRevision != attempt.Lease.BindingRevision || attempt.Dispatch.Operation.OperationName != attempt.Lease.Operation || attempt.Dispatch.IdempotencyKey != contract.IdempotencyKey("published:"+attempt.IntentHash) || contract.PublishedOperationRef(attempt.Dispatch.ProviderModuleID, attempt.Dispatch.Operation.CapabilityName, attempt.Dispatch.Operation.OperationName) != attempt.Receipt.OperationRef || attempt.Lease.Scope != attempt.Dispatch.Scope || attempt.Lease.TenantID != attempt.Dispatch.Scope.TenantID || attempt.Lease.State != "active" {
		return PublishedOperationAttempt{}, false, fmt.Errorf("published operation provider pin is invalid")
	}
	scopeJSON, _ := json.Marshal(attempt.Receipt.BusinessScope)
	dispatchJSON, _ := json.Marshal(attempt.Dispatch)
	leaseJSON, _ := json.Marshal(attempt.Lease)
	receiptJSON, _ := json.Marshal(attempt.Receipt)
	_, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `INSERT INTO _kora_published_operation_receipt (tenant_id,execution_id,actor_type,actor_id,intent_hash,input_hash,state,dispatch_state,operation_ref,scope_json,dispatch_json,lease_json,receipt_json) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`), string(attempt.Receipt.BusinessScope.TenantID), string(attempt.Receipt.ExecutionID), string(actor.PrincipalType), actor.PrincipalID, attempt.IntentHash, attempt.InputHash, string(attempt.Receipt.Status), "claimed", attempt.Receipt.OperationRef, string(scopeJSON), string(dispatchJSON), string(leaseJSON), string(receiptJSON))
	if err == nil {
		return attempt, true, nil
	}
	loaded, found, loadErr := s.LoadPublishedOperationAttempt(ctx, attempt.Receipt.BusinessScope.TenantID, attempt.IntentHash, actor, attempt.Receipt.BusinessScope)
	if loadErr != nil {
		return PublishedOperationAttempt{}, false, err
	}
	if !found {
		return PublishedOperationAttempt{}, false, err
	}
	if loaded.Receipt.OperationRef != attempt.Receipt.OperationRef || loaded.InputHash != attempt.InputHash {
		return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeIdempotencyKeyReused, "operation idempotency key was reused for a different intent")
	}
	return loaded, false, nil
}

func (s *ModuleStore) LoadPublishedOperationAttempt(ctx context.Context, tenant contract.TenantID, intentHash string, actor contract.ActorContext, scope contract.BusinessScope) (PublishedOperationAttempt, bool, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || tenant == "" || intentHash == "" || !actor.Authenticated() || scope.Validate() != nil || scope.TenantID != tenant {
		return PublishedOperationAttempt{}, false, fmt.Errorf("published operation intent lookup identity is invalid")
	}
	var operationRef, scopeJSON, inputHash, state, dispatchState, dispatchJSON, leaseJSON, receiptJSON string
	var executionID string
	err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT execution_id,operation_ref,scope_json,input_hash,state,dispatch_state,dispatch_json,lease_json,receipt_json FROM _kora_published_operation_receipt WHERE tenant_id=? AND intent_hash=? AND actor_type=? AND actor_id=?`), string(tenant), intentHash, string(actor.PrincipalType), actor.PrincipalID).Scan(&executionID, &operationRef, &scopeJSON, &inputHash, &state, &dispatchState, &dispatchJSON, &leaseJSON, &receiptJSON)
	if err == sql.ErrNoRows {
		return PublishedOperationAttempt{}, false, nil
	}
	if err != nil {
		return PublishedOperationAttempt{}, false, err
	}
	var storedScope contract.BusinessScope
	var attempt PublishedOperationAttempt
	if json.Unmarshal([]byte(scopeJSON), &storedScope) != nil || storedScope != scope || json.Unmarshal([]byte(receiptJSON), &attempt.Receipt) != nil || attempt.Receipt.Validate() != nil || attempt.Receipt.BusinessScope != scope || attempt.Receipt.OperationRef != operationRef || string(attempt.Receipt.ExecutionID) != executionID {
		return PublishedOperationAttempt{}, false, contract.NewError(contract.CodePermissionDenied, "published operation intent scope does not match")
	}
	attempt.IntentHash, attempt.InputHash = intentHash, inputHash
	attempt.Claimed = dispatchState == "claimed"
	if dispatchState != "claimed" && dispatchState != "dispatched" {
		return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeInternal, "stored operation dispatch state is invalid")
	}
	if dispatchJSON != "null" && dispatchJSON != "" {
		var dispatch contract.PinnedDispatch
		if json.Unmarshal([]byte(dispatchJSON), &dispatch) != nil || dispatch.Validate() != nil {
			return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeInternal, "stored provider pin is invalid")
		}
		attempt.Dispatch = &dispatch
	}
	if leaseJSON != "null" && leaseJSON != "" {
		var lease contract.CapabilityLease
		if json.Unmarshal([]byte(leaseJSON), &lease) != nil || lease.Validate() != nil {
			return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeInternal, "stored capability lease is invalid")
		}
		attempt.Lease = &lease
	}
	if !isCanonicalHex(attempt.IntentHash, 32) || !isCanonicalHex(attempt.InputHash, 32) || (attempt.Dispatch == nil) != (attempt.Lease == nil) || attempt.Dispatch == nil || attempt.Dispatch.InputHash != attempt.InputHash || attempt.Dispatch.Scope != scope || attempt.Dispatch.ExecutionID != executionID || attempt.Dispatch.ProviderModuleID == "" || attempt.Dispatch.ProviderModuleID != attempt.Lease.ProviderModuleID || attempt.Dispatch.ProviderID != attempt.Lease.ProviderID || attempt.Dispatch.CallerModuleID != attempt.Lease.CallerModuleID || attempt.Dispatch.BindingRevision != attempt.Lease.BindingRevision || attempt.Dispatch.Operation.OperationName != attempt.Lease.Operation || attempt.Dispatch.IdempotencyKey != contract.IdempotencyKey("published:"+attempt.IntentHash) || contract.PublishedOperationRef(attempt.Dispatch.ProviderModuleID, attempt.Dispatch.Operation.CapabilityName, attempt.Dispatch.Operation.OperationName) != attempt.Receipt.OperationRef || attempt.Lease.Scope != scope || attempt.Lease.TenantID != scope.TenantID {
		return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeInternal, "stored provider pin and lease do not match the operation intent")
	}
	if state != string(attempt.Receipt.Status) {
		return PublishedOperationAttempt{}, false, contract.NewError(contract.CodeInternal, "published operation attempt status failed integrity check")
	}
	return attempt, true, nil
}

// ClaimPublishedOperationDispatch grants exactly one caller the right to make
// the first external provider request. A duplicate may only reconcile it.
func (s *ModuleStore) ClaimPublishedOperationDispatch(ctx context.Context, actor contract.ActorContext, attempt PublishedOperationAttempt) (bool, error) {
	if ctx == nil || s == nil || s.DB == nil || !actor.Authenticated() || attempt.Receipt.Status != contract.ReceiptRunning || attempt.Dispatch == nil || attempt.Lease == nil {
		return false, fmt.Errorf("published operation dispatch claim is invalid")
	}
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_published_operation_receipt SET dispatch_state='dispatched' WHERE tenant_id=? AND execution_id=? AND actor_type=? AND actor_id=? AND intent_hash=? AND state='running' AND dispatch_state='claimed'`), string(attempt.Receipt.BusinessScope.TenantID), string(attempt.Receipt.ExecutionID), string(actor.PrincipalType), actor.PrincipalID, attempt.IntentHash)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func isCanonicalHex(value string, byteLength int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == byteLength && hex.EncodeToString(decoded) == value
}

// CompletePublishedOperationAttempt replaces a non-terminal public receipt
// only for its owning actor and exact scope. Provider pin columns are immutable.
func (s *ModuleStore) CompletePublishedOperationAttempt(ctx context.Context, actor contract.ActorContext, attempt PublishedOperationAttempt) error {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || !actor.Authenticated() || attempt.Receipt.Validate() != nil || attempt.IntentHash == "" {
		return fmt.Errorf("published operation completion identity is invalid")
	}
	receiptJSON, _ := json.Marshal(attempt.Receipt)
	scopeJSON, _ := json.Marshal(attempt.Receipt.BusinessScope)
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, `UPDATE _kora_published_operation_receipt SET state=?,receipt_json=? WHERE tenant_id=? AND execution_id=? AND actor_type=? AND actor_id=? AND intent_hash=? AND operation_ref=? AND scope_json=? AND input_hash=? AND state IN ('running','outcome_unknown')`), string(attempt.Receipt.Status), string(receiptJSON), string(attempt.Receipt.BusinessScope.TenantID), string(attempt.Receipt.ExecutionID), string(actor.PrincipalType), actor.PrincipalID, attempt.IntentHash, attempt.Receipt.OperationRef, string(scopeJSON), attempt.InputHash)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return contract.NewError(contract.CodeConflict, "published operation attempt could not be completed")
	}
	return nil
}

func (s *ModuleStore) LoadPublishedOperationReceipt(ctx context.Context, tenant contract.TenantID, id contract.OperationID, actor contract.ActorContext, scope contract.BusinessScope) (contract.OperationReceipt, bool, error) {
	if ctx == nil || s == nil || s.DB == nil || s.Dialect == nil || tenant == "" || id == "" || !actor.Authenticated() || scope.Validate() != nil || scope.TenantID != tenant {
		return contract.OperationReceipt{}, false, fmt.Errorf("published operation receipt lookup identity is invalid")
	}
	var storedOperationRef, scopeJSON, raw string
	err := s.DB.QueryRowContext(ctx, db.Rebind(s.Dialect, `SELECT operation_ref,scope_json,receipt_json FROM _kora_published_operation_receipt WHERE tenant_id=? AND execution_id=? AND actor_type=? AND actor_id=?`), string(tenant), string(id), string(actor.PrincipalType), actor.PrincipalID).Scan(&storedOperationRef, &scopeJSON, &raw)
	if err == sql.ErrNoRows {
		return contract.OperationReceipt{}, false, nil
	}
	if err != nil {
		return contract.OperationReceipt{}, false, err
	}
	var storedScope contract.BusinessScope
	var receipt contract.OperationReceipt
	if json.Unmarshal([]byte(scopeJSON), &storedScope) != nil || storedScope != scope || json.Unmarshal([]byte(raw), &receipt) != nil || receipt.Validate() != nil || receipt.ExecutionID != id || receipt.BusinessScope != scope || receipt.OperationRef != storedOperationRef {
		return contract.OperationReceipt{}, false, contract.NewError(contract.CodePermissionDenied, "published operation receipt scope does not match")
	}
	return receipt, true, nil
}
