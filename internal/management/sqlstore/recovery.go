package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/management"
)

// RetryStateOperation requeues only an uncertain tenant-state operation after
// an audited operator reconciliation.
func (s *Store) RetryStateOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	return s.retry(ctx, stateRetry, retryRequest{caller: caller, id: id, key: key, reason: reason, payload: reasonPayload{Reason: reason}, validate: func() error {
		return contract.ValidateReconcileTenantStateOperation(contract.ReconcileTenantStateOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason})
	}})
}

// RetryProvisionOperation requeues only a recovery_required base-provision
// command with no package effect. The Engine executor revalidates its configured
// deployment/allocation and compares any existing directory row before replay.
func (s *Store) RetryProvisionOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	return s.retry(ctx, provisionRetry, retryRequest{caller: caller, id: id, key: key, reason: reason, payload: reasonPayload{Reason: reason}, validate: func() error {
		return contract.ValidateReconcileTenantProvisionOperation(contract.ReconcileTenantProvisionOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason})
	}})
}

// RetryPackageOperation requeues an uncertain package command without
// changing its operation ID or command. Package activation is safe to resume
// only through its tenant-local journal, artifact digest check, and claim
// fencing; the executor owns those checks.
func (s *Store) RetryPackageOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	return s.retry(ctx, packageRetry, retryRequest{caller: caller, id: id, key: key, reason: reason, payload: reasonPayload{Reason: reason}, validate: func() error {
		return contract.ValidateReconcileTenantPackageOperation(contract.ReconcileTenantPackageOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason})
	}})
}

// RetryDeleteOperation requeues only an uncertain deletion with the original,
// validated checkpoint reference. The Engine executor must reverify that
// reference against the current allocation before continuing teardown.
func (s *Store) RetryDeleteOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	return s.retry(ctx, deleteRetry, retryRequest{caller: caller, id: id, key: key, reason: reason, payload: reasonPayload{Reason: reason}, validate: func() error {
		return contract.ValidateReconcileTenantDeleteOperation(contract.ReconcileTenantDeleteOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason})
	}})
}

// RetryInvitationOperation records the operator-confirmed delivery outcome and
// requeues only the original uncertain invitation operation.
func (s *Store) RetryInvitationOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, outcome contract.InvitationDeliveryOutcome, reason string) (management.Operation, error) {
	payload := struct {
		Outcome contract.InvitationDeliveryOutcome `json:"outcome"`
		Reason  string                             `json:"reason"`
	}{Outcome: outcome, Reason: reason}
	return s.retry(ctx, invitationRetry, retryRequest{caller: caller, id: id, key: key, reason: reason, payload: payload, validate: func() error {
		return contract.ValidateReconcileTenantInvitationOperation(contract.ReconcileTenantInvitationOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Outcome: outcome, Reason: reason})
	}})
}

type reasonPayload struct {
	Reason string `json:"reason"`
}

// retryRequest is one operator retry submission. payload is the canonical
// reconciliation content whose digest makes the submission idempotent.
type retryRequest struct {
	caller   contract.PrincipalID
	id       contract.OperationID
	key      contract.IdempotencyKey
	reason   string
	payload  any
	validate func() error
}

// retry records an audited reconciliation and requeues the same
// recovery_required operation of the kind's command, or replays an earlier
// submission with the same idempotency key.
func (s *Store) retry(ctx context.Context, kind retryKind, request retryRequest) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if err := kind.check(ctx, request); err != nil {
		return management.Operation{}, err
	}
	payload, err := json.Marshal(request.payload)
	if err != nil {
		return management.Operation{}, fmt.Errorf("%s: %w", kind.text.encode, err)
	}
	digest := management.CommandDigest(kind.digestName, payload)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("%s: %w", kind.text.begin, err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, request.id, request.caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	// Recheck under the operation row lock so concurrent identical requests
	// converge on the first durable audit entry.
	replay, err := s.replayed(ctx, tx, request, digest)
	if err != nil {
		return management.Operation{}, err
	}
	if replay {
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("%s: %w", kind.text.replay, err)
		}
		return operation, nil
	}
	if admitErr := kind.admit(operation); admitErr != nil {
		return management.Operation{}, admitErr
	}
	if requeueErr := s.requeue(ctx, tx, kind, request, digest); requeueErr != nil {
		return management.Operation{}, requeueErr
	}
	operation, err = operationByID(ctx, tx, s.dialect, request.id, request.caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("%s: %w", kind.text.commit, err)
	}
	return operation, nil
}

// replayed reports whether this idempotency key already reconciled the
// operation with the same content; a different content is a reused key.
func (s *Store) replayed(ctx context.Context, tx *sql.Tx, request retryRequest, digest string) (bool, error) {
	prior, found, err := recoveryByKey(ctx, tx, s.dialect, request.id, request.caller, request.key)
	if err != nil || !found {
		return false, err
	}
	if prior != digest {
		return false, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
	}
	return true, nil
}

// requeue writes the audit row and returns the operation to requested. The
// UPDATE is guarded by caller, command kind and recovery_required state.
func (s *Store) requeue(ctx context.Context, tx *sql.Tx, kind retryKind, request retryRequest, digest string) error {
	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(request.id), string(request.caller), string(request.key), digest, request.reason, now); err != nil {
		return fmt.Errorf("%s: %w", kind.text.audit, err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(request.id), string(request.caller), string(kind.command), string(contract.OperationRecoveryRequired))
	if err != nil {
		return fmt.Errorf("%s: %w", kind.text.requeue, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", kind.text.result, err)
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, kind.text.changed)
	}
	return nil
}

func operationByID(ctx context.Context, tx *sql.Tx, dialect db.Dialect, id contract.OperationID, caller contract.PrincipalID, lock bool) (management.Operation, error) {
	query := `SELECT ` + operationColumns + ` FROM _kora_managed_operation WHERE id = ? AND caller_id = ?`
	if lock && dialect.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	operation, err := scanOperation(tx.QueryRowContext(ctx, db.Rebind(dialect, query), string(id), string(caller)))
	if errors.Is(err, sql.ErrNoRows) {
		return management.Operation{}, contract.NewError(contract.CodeNotFound, "management operation not found")
	}
	if err != nil {
		return management.Operation{}, fmt.Errorf("read management operation for reconciliation: %w", err)
	}
	return operation, nil
}

func recoveryByKey(ctx context.Context, tx *sql.Tx, dialect db.Dialect, id contract.OperationID, caller contract.PrincipalID, key contract.IdempotencyKey) (string, bool, error) {
	query := `SELECT command_digest FROM _kora_managed_operation_recovery WHERE operation_id = ? AND caller_id = ? AND idempotency_key = ?`
	var digest string
	err := tx.QueryRowContext(ctx, db.Rebind(dialect, query), string(id), string(caller), string(key)).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read management reconciliation idempotency record: %w", err)
	}
	return digest, true, nil
}

var _ management.StateRecoveryStore = (*Store)(nil)
var _ management.ProvisionRecoveryStore = (*Store)(nil)
var _ management.PackageRecoveryStore = (*Store)(nil)
var _ management.DeleteRecoveryStore = (*Store)(nil)
