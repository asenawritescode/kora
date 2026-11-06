package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/management"
)

type Store struct {
	database *sql.DB
	dialect  db.Dialect
}

const operationColumns = "id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,COALESCE(observed_revision,''),COALESCE(error_type,''),COALESCE(error_message,''),created_at,updated_at"

var _ management.Store = (*Store)(nil)

func New(database *sql.DB, dialect db.Dialect) (*Store, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("management operation store requires database and dialect")
	}
	if _, err := SchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &Store{database: database, dialect: dialect}, nil
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	statements, err := SchemaStatements(s.dialect)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create management operation schema: %w", err)
		}
	}
	return nil
}

func (s *Store) Accept(ctx context.Context, input management.AcceptCommand) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management accept requires context")
	}
	if _, err := contract.ParseOperationID(string(input.ID)); err != nil {
		return management.Operation{}, err
	}
	if _, err := contract.ParseTenantID(string(input.Tenant)); err != nil {
		return management.Operation{}, err
	}
	if _, err := contract.ParsePrincipalID(string(input.Caller)); err != nil {
		return management.Operation{}, err
	}
	if _, err := contract.ParseIdempotencyKey(string(input.IdempotencyKey)); err != nil {
		return management.Operation{}, err
	}
	if input.Now.IsZero() || len(input.Command) == 0 || len(input.Command) > 1<<20 || !json.Valid(input.Command) {
		return management.Operation{}, fmt.Errorf("management command payload or timestamp is invalid")
	}
	if input.Kind != management.CommandProvisionTenant && input.Kind != management.CommandApplyPackage && input.Kind != management.CommandSetTenantState && input.Kind != management.CommandDeleteTenant && input.Kind != management.CommandTenantInvitation && input.Kind != management.CommandApplyModuleChange && input.Kind != management.CommandTenantMemberRevoke && input.Kind != management.CommandTenantMemberGrant {
		return management.Operation{}, fmt.Errorf("management command kind is not registered")
	}
	digest := management.CommandDigest(input.Kind, input.Command)
	if existing, found, err := s.lookup(ctx, input.Tenant, input.Caller, input.IdempotencyKey); found || err != nil {
		return matchReplay(existing, digest, err)
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return management.Operation{}, fmt.Errorf("begin management operation acceptance: %w", err)
	}
	defer tx.Rollback()
	if existing, found, err := s.lookupTx(ctx, tx, input.Tenant, input.Caller, input.IdempotencyKey); err != nil {
		return management.Operation{}, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return management.Operation{}, fmt.Errorf("finish management operation replay: %w", err)
		}
		return matchReplay(existing, digest, nil)
	}
	stamp := input.Now.UTC()
	query := `INSERT INTO _kora_managed_operation (id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(input.ID), string(input.Tenant), string(input.Caller), string(input.IdempotencyKey), string(input.Kind), digest, string(input.Command), string(contract.OperationRequested), stamp, stamp); err != nil {
		_ = tx.Rollback()
		if existing, found, lookupErr := s.lookup(ctx, input.Tenant, input.Caller, input.IdempotencyKey); found || lookupErr != nil {
			return matchReplay(existing, digest, lookupErr)
		}
		return management.Operation{}, fmt.Errorf("persist accepted management operation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if existing, found, lookupErr := s.lookup(ctx, input.Tenant, input.Caller, input.IdempotencyKey); found || lookupErr != nil {
			return matchReplay(existing, digest, lookupErr)
		}
		return management.Operation{}, fmt.Errorf("commit accepted management operation: %w", err)
	}
	return management.Operation{ID: input.ID, Tenant: input.Tenant, Caller: input.Caller, IdempotencyKey: input.IdempotencyKey, Kind: input.Kind, CommandDigest: digest, Command: append(json.RawMessage(nil), input.Command...), State: contract.OperationRequested, CreatedAt: stamp, UpdatedAt: stamp}, nil
}

func (s *Store) Get(ctx context.Context, caller contract.PrincipalID, id contract.OperationID) (management.Operation, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return management.Operation{}, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return management.Operation{}, fmt.Errorf("management operation lookup requires context")
	}
	if _, err := contract.ParsePrincipalID(string(caller)); err != nil {
		return management.Operation{}, contract.NewError(contract.CodeUnauthenticated, "authenticated management caller is required")
	}
	if _, err := contract.ParseOperationID(string(id)); err != nil {
		return management.Operation{}, err
	}
	value, err := scanOperation(s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,COALESCE(observed_revision,''),COALESCE(error_type,''),COALESCE(error_message,''),created_at,updated_at FROM _kora_managed_operation WHERE id = ? AND caller_id = ?`), string(id), string(caller)))
	if errors.Is(err, sql.ErrNoRows) {
		return management.Operation{}, contract.NewError(contract.CodeNotFound, "management operation not found")
	}
	return value, err
}

func (s *Store) ClaimDue(ctx context.Context, owner string, limit uint16, lease time.Duration) ([]management.Claim, error) {
	return s.claimDue(ctx, owner, limit, lease, "")
}

func (s *Store) ClaimDueForDeployment(ctx context.Context, owner string, limit uint16, lease time.Duration, deployment contract.DeploymentID) ([]management.Claim, error) {
	if _, err := contract.ParseDeploymentID(string(deployment)); err != nil {
		return nil, err
	}
	return s.claimDue(ctx, owner, limit, lease, string(deployment))
}

func (s *Store) claimDue(ctx context.Context, owner string, limit uint16, lease time.Duration, deployment string) ([]management.Claim, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return nil, fmt.Errorf("management operation store is not configured")
	}
	if ctx == nil {
		return nil, fmt.Errorf("management operation claim requires context")
	}
	if owner == "" || len(owner) > 160 || limit == 0 || limit > 100 || lease < time.Second || lease > 5*time.Minute {
		return nil, fmt.Errorf("management claim owner, limit, or lease is invalid")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin management operation claim: %w", err)
	}
	defer tx.Rollback()
	now := dbNow(s.dialect)
	query := `SELECT ` + operationColumns + `,lease_fence FROM _kora_managed_operation WHERE state IN ('requested','running') AND (lease_until IS NULL OR lease_until <= ` + now + `)`
	args := make([]any, 0, 2)
	if deployment != "" {
		var deploymentJSON string
		switch s.dialect.DriverName() {
		case "postgres":
			deploymentJSON = "command_json->>'deployment_id'"
		case "mysql":
			deploymentJSON = "JSON_UNQUOTE(JSON_EXTRACT(command_json, '$.deployment_id'))"
		default:
			deploymentJSON = "json_extract(command_json, '$.deployment_id')"
		}
		query += ` AND (command_kind <> 'provision_tenant' OR ` + deploymentJSON + ` = ?)`
		args = append(args, deployment)
	}
	query += ` ORDER BY created_at,id LIMIT ?`
	args = append(args, limit)
	if s.dialect.DriverName() == "mysql" {
		query += " FOR UPDATE SKIP LOCKED"
	}
	if s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("select due management operations: %w", err)
	}
	type dueOperation struct {
		operation management.Operation
		fence     uint64
	}
	due := make([]dueOperation, 0, limit)
	for rows.Next() {
		operation, fence, scanErr := scanOperationWithFence(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due management operation: %w", scanErr)
		}
		due = append(due, dueOperation{operation: operation, fence: fence})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	claims := make([]management.Claim, 0, len(due))
	stamp := now
	for _, item := range due {
		leaseUntil := leaseExpression(s.dialect)
		query := `UPDATE _kora_managed_operation SET state = 'running',lease_owner = ?,lease_fence = lease_fence + 1,lease_until = ` + leaseUntil + `,updated_at = ` + stamp + ` WHERE id = ? AND lease_fence = ? AND state IN ('requested','running') AND (lease_until IS NULL OR lease_until <= ` + stamp + `)`
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), owner, leaseArgument(s.dialect, lease), string(item.operation.ID), item.fence)
		if err != nil {
			return nil, fmt.Errorf("fence management operation claim: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			continue
		}
		item.operation.State = contract.OperationRunning
		claims = append(claims, management.Claim{Operation: item.operation, Owner: owner, Fence: item.fence + 1})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit management operation claims: %w", err)
	}
	return claims, nil
}

func (s *Store) Renew(ctx context.Context, claim management.Claim, lease time.Duration) (management.Claim, error) {
	if err := validateClaim(claim); err != nil {
		return management.Claim{}, err
	}
	if lease < time.Second || lease > 5*time.Minute {
		return management.Claim{}, fmt.Errorf("management operation lease must be between 1 second and 5 minutes")
	}
	now := dbNow(s.dialect)
	query := `UPDATE _kora_managed_operation SET lease_until = ` + leaseExpression(s.dialect) + `,updated_at = ` + now + ` WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + now
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), leaseArgument(s.dialect, lease), string(claim.Operation.ID), claim.Owner, claim.Fence)
	if err != nil {
		return management.Claim{}, fmt.Errorf("renew management operation claim: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return management.Claim{}, err
	}
	if changed != 1 {
		return management.Claim{}, contract.NewError(contract.CodeConflict, "management operation claim is stale or expired")
	}
	// The exact expiry is informational; correctness uses the database clock in
	// every renewal/finish predicate, avoiding clock-skew-dependent fencing.
	claim.LeaseUntil = time.Time{}
	return claim, nil
}

func (s *Store) Finish(ctx context.Context, claim management.Claim, outcome management.FinishResult) error {
	if err := validateClaim(claim); err != nil {
		return err
	}
	if outcome.State != contract.OperationSucceeded && outcome.State != contract.OperationFailed && outcome.State != contract.OperationRecoveryRequired {
		return fmt.Errorf("management operation result state must be terminal")
	}
	if outcome.State == contract.OperationSucceeded && outcome.Error != nil {
		return fmt.Errorf("successful management operation cannot have an error")
	}
	if outcome.State != contract.OperationSucceeded && (outcome.Error == nil || len(outcome.Error.Message) > 8192) {
		return fmt.Errorf("failed or uncertain management operation requires a bounded error")
	}
	var observed, errorType, errorMessage any
	if outcome.ObservedRevision != "" {
		if _, err := contract.ParseRevision(string(outcome.ObservedRevision)); err != nil {
			return fmt.Errorf("observed management revision: %w", err)
		}
		observed = string(outcome.ObservedRevision)
	}
	if outcome.Error != nil {
		errorType, errorMessage = string(outcome.Error.Type), outcome.Error.Message
	}
	now := dbNow(s.dialect)
	query := `UPDATE _kora_managed_operation SET state = ?,observed_revision = ?,error_type = ?,error_message = ?,lease_owner = NULL,lease_until = NULL,updated_at = ` + now + ` WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + now
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), string(outcome.State), observed, errorType, errorMessage, string(claim.Operation.ID), claim.Owner, claim.Fence)
	if err != nil {
		return fmt.Errorf("finish management operation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "management operation claim is stale or expired")
	}
	return nil
}

func (s *Store) Release(ctx context.Context, claim management.Claim) error {
	if err := validateClaim(claim); err != nil {
		return err
	}
	now := dbNow(s.dialect)
	query := `UPDATE _kora_managed_operation SET state = 'requested',lease_owner = NULL,lease_until = NULL,updated_at = ` + now + ` WHERE id = ? AND state = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + now
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), string(claim.Operation.ID), claim.Owner, claim.Fence)
	if err != nil {
		return fmt.Errorf("release management operation claim: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "management operation claim is stale or expired")
	}
	return nil
}

func validateClaim(claim management.Claim) error {
	if _, err := contract.ParseOperationID(string(claim.Operation.ID)); err != nil {
		return err
	}
	if claim.Owner == "" || len(claim.Owner) > 160 || claim.Fence == 0 {
		return fmt.Errorf("management operation owner and fence are required")
	}
	return nil
}

func dbNow(dialect db.Dialect) string {
	if dialect.DriverName() == "mysql" {
		return "CURRENT_TIMESTAMP(6)"
	}
	return "CURRENT_TIMESTAMP"
}

func leaseExpression(dialect db.Dialect) string {
	switch dialect.DriverName() {
	case "mysql":
		return "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND)"
	case "postgres":
		return "CURRENT_TIMESTAMP + (? * INTERVAL '1 microsecond')"
	default:
		return "datetime('now', '+' || ? || ' seconds')"
	}
}

func leaseArgument(dialect db.Dialect, lease time.Duration) any {
	if dialect.DriverName() == "libsql" {
		return strconv.FormatFloat(lease.Seconds(), 'f', 6, 64)
	}
	return lease.Microseconds()
}

func (s *Store) lookup(ctx context.Context, tenant contract.TenantID, caller contract.PrincipalID, key contract.IdempotencyKey) (management.Operation, bool, error) {
	value, err := scanOperation(s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,COALESCE(observed_revision,''),COALESCE(error_type,''),COALESCE(error_message,''),created_at,updated_at FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ?`), string(tenant), string(caller), string(key)))
	if errors.Is(err, sql.ErrNoRows) {
		return management.Operation{}, false, nil
	}
	return value, err == nil, err
}

func (s *Store) lookupTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, caller contract.PrincipalID, key contract.IdempotencyKey) (management.Operation, bool, error) {
	query := `SELECT id,tenant_id,caller_id,idempotency_key,command_kind,command_digest,command_json,state,COALESCE(observed_revision,''),COALESCE(error_type,''),COALESCE(error_message,''),created_at,updated_at FROM _kora_managed_operation WHERE tenant_id = ? AND caller_id = ? AND idempotency_key = ?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	value, err := scanOperation(tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(caller), string(key)))
	if errors.Is(err, sql.ErrNoRows) {
		return management.Operation{}, false, nil
	}
	return value, err == nil, err
}

func matchReplay(value management.Operation, digest string, err error) (management.Operation, error) {
	if err != nil {
		return management.Operation{}, fmt.Errorf("resolve management operation idempotency: %w", err)
	}
	if value.CommandDigest != digest {
		return management.Operation{}, contract.NewError(contract.CodeIdempotencyKeyReused, "management idempotency key was reused with different command content")
	}
	return value, nil
}

type rowScanner interface{ Scan(...any) error }

func scanOperation(row rowScanner) (management.Operation, error) {
	value, _, err := scanOperationInternal(row, nil)
	return value, err
}

func scanOperationWithFence(row rowScanner) (management.Operation, uint64, error) {
	var fence uint64
	value, _, err := scanOperationInternal(row, &fence)
	return value, fence, err
}

func scanOperationInternal(row rowScanner, fence *uint64) (management.Operation, uint64, error) {
	var value management.Operation
	var payload []byte
	var observed, errorType, errorMessage sql.NullString
	var createdRaw, updatedRaw any
	args := []any{&value.ID, &value.Tenant, &value.Caller, &value.IdempotencyKey, &value.Kind, &value.CommandDigest, &payload, &value.State, &observed, &errorType, &errorMessage, &createdRaw, &updatedRaw}
	if fence != nil {
		args = append(args, fence)
	}
	err := row.Scan(args...)
	if err != nil {
		return management.Operation{}, 0, err
	}
	if !json.Valid(payload) || management.CommandDigest(value.Kind, payload) != value.CommandDigest {
		return management.Operation{}, 0, fmt.Errorf("stored management operation payload digest is invalid")
	}
	value.Command = append(json.RawMessage(nil), payload...)
	if observed.Valid {
		value.ObservedRevision = contract.Revision(observed.String)
	}
	if errorType.String != "" || errorMessage.String != "" {
		if !errorType.Valid || !errorMessage.Valid || errorType.String == "" || errorMessage.String == "" {
			return management.Operation{}, 0, fmt.Errorf("stored management operation error is incomplete")
		}
		value.Error = &contract.Error{Type: contract.Code(errorType.String), Message: errorMessage.String}
	}
	value.CreatedAt, err = parseTime(createdRaw)
	if err != nil {
		return management.Operation{}, 0, err
	}
	value.UpdatedAt, err = parseTime(updatedRaw)
	if err != nil {
		return management.Operation{}, 0, err
	}
	var fenceValue uint64
	if fence != nil {
		fenceValue = *fence
	}
	return value, fenceValue, nil
}

func parseTime(raw any) (time.Time, error) {
	switch value := raw.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseTimeString(value)
	case []byte:
		return parseTimeString(string(value))
	default:
		return time.Time{}, fmt.Errorf("unsupported operation timestamp value %T", raw)
	}
}

func parseTimeString(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05.999", "2006-01-02 15:04:05"} {
		if value, err := time.Parse(layout, raw); err == nil {
			return value.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid stored operation timestamp %q", raw)
}
