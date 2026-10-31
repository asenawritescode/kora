package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/management"
)

func (s *Store) RetryStateOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management operation reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid reconciliation idempotency key")
	}
	if err := contract.ValidateReconcileTenantStateOperation(contract.ReconcileTenantStateOperation{
		Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason,
	}); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "tenant-state reconciliation command is invalid")
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return management.Operation{}, fmt.Errorf("encode operation reconciliation: %w", err)
	}
	digest := management.CommandDigest("reconcile_tenant_state", payload)

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin management operation reconciliation: %w", err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, id, caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	// Recheck under the operation row lock so concurrent identical requests
	// converge on the first durable audit entry.
	if prior, found, lookupErr := recoveryByKey(ctx, tx, s.dialect, id, caller, key); lookupErr != nil {
		return management.Operation{}, lookupErr
	} else if found {
		if prior != digest {
			return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
		}
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("commit concurrent reconciliation replay: %w", err)
		}
		return operation, nil
	}
	if operation.Kind != management.CommandSetTenantState || operation.State != contract.OperationRecoveryRequired {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only uncertain tenant-state operations can be reconciled")
	}

	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(id), string(caller), string(key), digest, reason, now); err != nil {
		return management.Operation{}, fmt.Errorf("persist management operation reconciliation audit: %w", err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(caller), string(management.CommandSetTenantState), string(contract.OperationRecoveryRequired))
	if err != nil {
		return management.Operation{}, fmt.Errorf("requeue reconciled tenant-state operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Operation{}, fmt.Errorf("read reconciled operation update result: %w", err)
	}
	if changed != 1 {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant-state operation changed during reconciliation")
	}
	operation, err = operationByID(ctx, tx, s.dialect, id, caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("commit tenant-state operation reconciliation: %w", err)
	}
	return operation, nil
}

// RetryProvisionOperation requeues only a recovery_required base-provision
// command with no package effect. The Engine executor revalidates its configured
// deployment/allocation and compares any existing directory row before replay.
func (s *Store) RetryProvisionOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management operation reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid reconciliation idempotency key")
	}
	if err := contract.ValidateReconcileTenantProvisionOperation(contract.ReconcileTenantProvisionOperation{
		Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason,
	}); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "tenant provisioning reconciliation command is invalid")
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return management.Operation{}, fmt.Errorf("encode provisioning operation reconciliation: %w", err)
	}
	digest := management.CommandDigest("reconcile_tenant_provision", payload)

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin provisioning operation reconciliation: %w", err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, id, caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	if prior, found, lookupErr := recoveryByKey(ctx, tx, s.dialect, id, caller, key); lookupErr != nil {
		return management.Operation{}, lookupErr
	} else if found {
		if prior != digest {
			return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
		}
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("commit concurrent provisioning reconciliation replay: %w", err)
		}
		return operation, nil
	}
	if operation.Kind != management.CommandProvisionTenant || operation.State != contract.OperationRecoveryRequired {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only uncertain tenant provisioning operations can be reconciled")
	}
	var command contract.ProvisionTenant
	if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateProvisionTenant(command) != nil || command.Tenant != operation.Tenant || command.PackageDigest != "" {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only valid base provisioning operations without package effects can be reconciled")
	}

	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(id), string(caller), string(key), digest, reason, now); err != nil {
		return management.Operation{}, fmt.Errorf("persist provisioning reconciliation audit: %w", err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(caller), string(management.CommandProvisionTenant), string(contract.OperationRecoveryRequired))
	if err != nil {
		return management.Operation{}, fmt.Errorf("requeue reconciled tenant provisioning operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Operation{}, fmt.Errorf("read reconciled provisioning update result: %w", err)
	}
	if changed != 1 {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant provisioning operation changed during reconciliation")
	}
	operation, err = operationByID(ctx, tx, s.dialect, id, caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("commit provisioning operation reconciliation: %w", err)
	}
	return operation, nil
}

// RetryPackageOperation requeues an uncertain package command without
// changing its operation ID or command. Package activation is safe to resume
// only through its tenant-local journal, artifact digest check, and claim
// fencing; the executor owns those checks.
func (s *Store) RetryPackageOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management operation reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid reconciliation idempotency key")
	}
	if err := contract.ValidateReconcileTenantPackageOperation(contract.ReconcileTenantPackageOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason}); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "package application reconciliation command is invalid")
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return management.Operation{}, fmt.Errorf("encode package operation reconciliation: %w", err)
	}
	digest := management.CommandDigest("reconcile_tenant_package", payload)

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin package operation reconciliation: %w", err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, id, caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	if prior, found, lookupErr := recoveryByKey(ctx, tx, s.dialect, id, caller, key); lookupErr != nil {
		return management.Operation{}, lookupErr
	} else if found {
		if prior != digest {
			return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
		}
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("commit concurrent package reconciliation replay: %w", err)
		}
		return operation, nil
	}
	if operation.Kind != management.CommandApplyPackage || operation.State != contract.OperationRecoveryRequired {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only uncertain package application operations can be reconciled")
	}
	var command contract.ApplyPackage
	if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateApplyPackage(command) != nil || command.Tenant != operation.Tenant {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "stored package application command is invalid or does not match its operation")
	}

	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(id), string(caller), string(key), digest, reason, now); err != nil {
		return management.Operation{}, fmt.Errorf("persist package reconciliation audit: %w", err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(caller), string(management.CommandApplyPackage), string(contract.OperationRecoveryRequired))
	if err != nil {
		return management.Operation{}, fmt.Errorf("requeue reconciled package operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Operation{}, fmt.Errorf("read reconciled package update result: %w", err)
	}
	if changed != 1 {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "package operation changed during reconciliation")
	}
	operation, err = operationByID(ctx, tx, s.dialect, id, caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("commit package operation reconciliation: %w", err)
	}
	return operation, nil
}

// RetryDeleteOperation requeues only an uncertain deletion with the original,
// validated checkpoint reference. The Engine executor must reverify that
// reference against the current allocation before continuing teardown.
func (s *Store) RetryDeleteOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, reason string) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management operation reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid reconciliation idempotency key")
	}
	if err := contract.ValidateReconcileTenantDeleteOperation(contract.ReconcileTenantDeleteOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Reason: reason}); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "tenant deletion reconciliation command is invalid")
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return management.Operation{}, fmt.Errorf("encode deletion operation reconciliation: %w", err)
	}
	digest := management.CommandDigest("reconcile_tenant_delete", payload)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin deletion operation reconciliation: %w", err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, id, caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	if prior, found, lookupErr := recoveryByKey(ctx, tx, s.dialect, id, caller, key); lookupErr != nil {
		return management.Operation{}, lookupErr
	} else if found {
		if prior != digest {
			return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
		}
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("commit concurrent deletion reconciliation replay: %w", err)
		}
		return operation, nil
	}
	if operation.Kind != management.CommandDeleteTenant || operation.State != contract.OperationRecoveryRequired {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only uncertain tenant deletion operations can be reconciled")
	}
	var command contract.DeleteTenant
	if err := json.Unmarshal(operation.Command, &command); err != nil || contract.ValidateDeleteTenant(command) != nil || command.Meta.IdempotencyKey != operation.IdempotencyKey {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only valid tenant deletion commands can be reconciled")
	}
	if _, err := contract.ParseTenantID(string(operation.Tenant)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant deletion operation has an invalid tenant identity")
	}
	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(id), string(caller), string(key), digest, reason, now); err != nil {
		return management.Operation{}, fmt.Errorf("persist deletion reconciliation audit: %w", err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(caller), string(management.CommandDeleteTenant), string(contract.OperationRecoveryRequired))
	if err != nil {
		return management.Operation{}, fmt.Errorf("requeue reconciled tenant deletion operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Operation{}, fmt.Errorf("read reconciled deletion update result: %w", err)
	}
	if changed != 1 {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant deletion operation changed during reconciliation")
	}
	operation, err = operationByID(ctx, tx, s.dialect, id, caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("commit tenant deletion operation reconciliation: %w", err)
	}
	return operation, nil
}

func (s *Store) RetryInvitationOperation(ctx context.Context, caller contract.PrincipalID, id contract.OperationID, key contract.IdempotencyKey, outcome contract.InvitationDeliveryOutcome, reason string) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "management reconciliation requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "invalid management operation ID")
	}
	command := contract.ReconcileTenantInvitationOperation{Meta: contract.CommandMeta{IdempotencyKey: key}, Outcome: outcome, Reason: reason}
	if err := contract.ValidateReconcileTenantInvitationOperation(command); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeValidationFailed, "tenant invitation reconciliation command is invalid")
	}
	payload, err := json.Marshal(struct {
		Outcome contract.InvitationDeliveryOutcome `json:"outcome"`
		Reason  string                             `json:"reason"`
	}{Outcome: outcome, Reason: reason})
	if err != nil {
		return management.Operation{}, fmt.Errorf("encode invitation reconciliation: %w", err)
	}
	digest := management.CommandDigest("reconcile_tenant_invitation", payload)
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin invitation reconciliation: %w", err)
	}
	defer tx.Rollback()
	operation, err := operationByID(ctx, tx, s.dialect, id, caller, true)
	if err != nil {
		return management.Operation{}, err
	}
	if prior, found, lookupErr := recoveryByKey(ctx, tx, s.dialect, id, caller, key); lookupErr != nil {
		return management.Operation{}, lookupErr
	} else if found {
		if prior != digest {
			return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "reconciliation idempotency key was already used with different content")
		}
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("commit invitation reconciliation replay: %w", err)
		}
		return operation, nil
	}
	if operation.Kind != management.CommandTenantInvitation || operation.State != contract.OperationRecoveryRequired {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only uncertain tenant invitation operations can be reconciled")
	}
	var original contract.RequestTenantInvitation
	if err := json.Unmarshal(operation.Command, &original); err != nil || contract.ValidateRequestTenantInvitation(original) != nil || original.Meta.IdempotencyKey != operation.IdempotencyKey {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "only valid tenant invitation commands can be reconciled")
	}
	if _, err := contract.ParseTenantID(string(operation.Tenant)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant invitation operation has an invalid tenant identity")
	}
	now := time.Now().UTC()
	query := `INSERT INTO _kora_managed_operation_recovery (recovery_id,operation_id,caller_id,idempotency_key,command_digest,reason,created_at) VALUES (?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), contract.NewID(), string(id), string(caller), string(key), digest, reason, now); err != nil {
		return management.Operation{}, fmt.Errorf("persist invitation reconciliation audit: %w", err)
	}
	query = `UPDATE _kora_managed_operation SET state = 'requested',observed_revision = NULL,error_type = NULL,error_message = NULL,lease_owner = NULL,lease_until = NULL,lease_fence = lease_fence + 1,updated_at = ` + dbNow(s.dialect) + ` WHERE id = ? AND caller_id = ? AND command_kind = ? AND state = ?`
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(caller), string(management.CommandTenantInvitation), string(contract.OperationRecoveryRequired))
	if err != nil {
		return management.Operation{}, fmt.Errorf("requeue reconciled invitation operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Operation{}, fmt.Errorf("read reconciled invitation update result: %w", err)
	}
	if changed != 1 {
		return management.Operation{}, contract.NewError(contract.CodeConflict, "tenant invitation operation changed during reconciliation")
	}
	operation, err = operationByID(ctx, tx, s.dialect, id, caller, false)
	if err != nil {
		return management.Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return management.Operation{}, fmt.Errorf("commit tenant invitation reconciliation: %w", err)
	}
	return operation, nil
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
