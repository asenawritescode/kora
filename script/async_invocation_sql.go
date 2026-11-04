package script

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

const maxAsyncHookRetryDelay = time.Minute

func AsyncHookInvocationSchemaStatements(dialect db.Dialect) ([]string, error) {
	if dialect == nil {
		return nil, errors.New("async hook schema requires a database dialect")
	}
	switch dialect.DriverName() {
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_script_invocation (id CHAR(64) NOT NULL PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, source_operation_id VARCHAR(64) NOT NULL, sequence_no INT NOT NULL, status VARCHAR(16) NOT NULL, payload_json JSON NOT NULL, attempts INT NOT NULL DEFAULT 0, lease_owner VARCHAR(160) NULL, lease_fence BIGINT NOT NULL DEFAULT 0, lease_until DATETIME(6) NULL, external_effect_started BOOLEAN NOT NULL DEFAULT FALSE, next_attempt_at DATETIME(6) NOT NULL, last_error TEXT NOT NULL, created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, UNIQUE KEY uq_kora_script_invocation_source (tenant_id, source_operation_id, sequence_no), INDEX idx_kora_script_invocation_due (tenant_id, status, next_attempt_at, lease_until, id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		}, nil
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_script_invocation (id CHAR(64) PRIMARY KEY, tenant_id TEXT NOT NULL, source_operation_id TEXT NOT NULL, sequence_no INTEGER NOT NULL, status TEXT NOT NULL, payload_json JSONB NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, lease_owner TEXT, lease_fence BIGINT NOT NULL DEFAULT 0, lease_until TIMESTAMPTZ, external_effect_started BOOLEAN NOT NULL DEFAULT FALSE, next_attempt_at TIMESTAMPTZ NOT NULL, last_error TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE (tenant_id, source_operation_id, sequence_no))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_script_invocation_due ON _kora_script_invocation (tenant_id, status, next_attempt_at, lease_until, id)`,
		}, nil
	case "libsql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_script_invocation (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, source_operation_id TEXT NOT NULL, sequence_no INTEGER NOT NULL, status TEXT NOT NULL, payload_json TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, lease_owner TEXT, lease_fence INTEGER NOT NULL DEFAULT 0, lease_until TEXT, external_effect_started INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE (tenant_id, source_operation_id, sequence_no))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_script_invocation_due ON _kora_script_invocation (tenant_id, status, next_attempt_at, lease_until, id)`,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported async hook invocation dialect %q", dialect.DriverName())
	}
}

func (s *Store) EnsureAsyncHookInvocationSchema(ctx context.Context) error {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil {
		return errors.New("async hook schema ensure requires store, database, dialect and context")
	}
	statements, err := AsyncHookInvocationSchemaStatements(s.Dialect)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := s.DB.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure async hook invocation schema: %w", err)
		}
	}
	return nil
}

func (s *Store) AppendAsyncHookTx(ctx context.Context, tx *sql.Tx, invocation AsyncHookInvocation) error {
	if s == nil || s.Dialect == nil || tx == nil || ctx == nil {
		return errors.New("async hook append requires store, transaction, dialect and context")
	}
	if _, err := contract.ParseTenantID(string(invocation.TenantID)); err != nil {
		return fmt.Errorf("async hook tenant: %w", err)
	}
	if _, err := contract.ParseOperationID(string(invocation.SourceOperation)); err != nil {
		return fmt.Errorf("async hook source operation: %w", err)
	}
	if invocation.Site == "" || invocation.Actor.Site != invocation.Site || !invocation.Actor.Authenticated() || invocation.ConfigRevision == 0 || invocation.Sequence == 0 || invocation.Doctype == "" || !IsAfterEvent(invocation.Event) {
		return errors.New("async hook invocation identity and source are incomplete")
	}
	if invocation.Record.ScriptType != TypeDocEvent || invocation.Record.Name == "" || invocation.Record.Site != invocation.Site || invocation.Record.DocType != invocation.Doctype || invocation.Record.Event != invocation.Event || !invocation.Record.IsActive || invocation.Record.Script == "" || invocation.Record.TimeoutMs <= 0 || invocation.Record.TimeoutMs > int(MaxAsyncHookTimeout/time.Millisecond) {
		return errors.New("async lifecycle hook script snapshot is invalid")
	}
	if len(invocation.Document) == 0 || len(invocation.Document) > MaxAsyncHookPayloadBytes || !json.Valid(invocation.Document) {
		return errors.New("async hook document snapshot is invalid or exceeds its size limit")
	}
	if len(invocation.OldDocument) > MaxAsyncHookPayloadBytes || (len(invocation.OldDocument) > 0 && !json.Valid(invocation.OldDocument)) {
		return errors.New("async hook old-document snapshot is invalid or exceeds its size limit")
	}
	if len(invocation.Grants) > 256 {
		return errors.New("async hook verified grant list exceeds its limit")
	}
	for _, grant := range invocation.Grants {
		if !grant.Valid() {
			return errors.New("async hook contains an invalid verified grant")
		}
	}
	payload, err := json.Marshal(invocation)
	if err != nil {
		return fmt.Errorf("encode async hook invocation: %w", err)
	}
	if len(payload) > MaxAsyncHookPayloadBytes {
		return fmt.Errorf("async hook invocation exceeds %d bytes", MaxAsyncHookPayloadBytes)
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", invocation.TenantID, invocation.SourceOperation, invocation.Sequence)))
	id := hex.EncodeToString(digest[:])
	stamp := s.currentTimestamp()
	query := `INSERT INTO _kora_script_invocation (id, tenant_id, source_operation_id, sequence_no, status, payload_json, attempts, lease_owner, lease_fence, lease_until, external_effect_started, next_attempt_at, last_error, created_at, updated_at) VALUES (?, ?, ?, ?, 'pending', ?, 0, NULL, 0, NULL, FALSE, ` + stamp + `, '', ` + stamp + `, ` + stamp + `)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, query), id, string(invocation.TenantID), string(invocation.SourceOperation), invocation.Sequence, string(payload)); err != nil {
		return fmt.Errorf("persist async hook invocation: %w", err)
	}
	return nil
}

func (s *Store) ClaimAsyncHooks(ctx context.Context, tenant contract.TenantID, owner string, limit uint16, lease time.Duration) ([]AsyncHookClaim, error) {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil || owner == "" || len(owner) > 160 || limit == 0 || limit > MaxAsyncHookBatch || lease <= 0 {
		return nil, errors.New("async hook claim requires a bounded tenant, owner, limit and lease")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("async hook tenant: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin async hook claim: %w", err)
	}
	defer tx.Rollback()
	stamp := s.currentTimestamp()
	uncertain := `UPDATE _kora_script_invocation SET status = 'uncertain', lease_owner = NULL, lease_until = NULL, last_error = 'external effect may have been dispatched before worker lease expired', updated_at = ` + stamp + ` WHERE tenant_id = ? AND status = 'running' AND external_effect_started = TRUE AND lease_until <= ` + stamp
	if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, uncertain), string(tenant)); err != nil {
		return nil, fmt.Errorf("mark expired external async hooks uncertain: %w", err)
	}
	query := `SELECT id, payload_json, attempts, lease_fence, external_effect_started FROM _kora_script_invocation WHERE tenant_id = ? AND ((status = 'pending' AND next_attempt_at <= ` + stamp + `) OR (status = 'running' AND external_effect_started = FALSE AND lease_until <= ` + stamp + `)) ORDER BY next_attempt_at, id LIMIT ?`
	if s.Dialect.DriverName() == "mysql" || s.Dialect.DriverName() == "postgres" {
		query += " FOR UPDATE SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.Dialect, query), string(tenant), limit)
	if err != nil {
		return nil, fmt.Errorf("select due async hooks: %w", err)
	}
	type selected struct {
		id              string
		payload         []byte
		attempts, fence uint64
		external        bool
	}
	items := make([]selected, 0, limit)
	for rows.Next() {
		var item selected
		if err := rows.Scan(&item.id, &item.payload, &item.attempts, &item.fence, &item.external); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due async hook: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate due async hooks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close async hook cursor: %w", err)
	}
	claims := make([]AsyncHookClaim, 0, len(items))
	for _, item := range items {
		if item.attempts >= uint64(MaxAsyncHookAttempts) {
			terminal := `UPDATE _kora_script_invocation SET status = 'failed', lease_owner = NULL, lease_until = NULL, last_error = 'worker lease expired after maximum attempts', updated_at = ` + stamp + ` WHERE id = ? AND tenant_id = ? AND lease_fence = ? AND attempts = ? AND external_effect_started = FALSE AND ((status = 'pending' AND next_attempt_at <= ` + stamp + `) OR (status = 'running' AND lease_until <= ` + stamp + `))`
			if _, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, terminal), item.id, string(tenant), item.fence, item.attempts); err != nil {
				return nil, fmt.Errorf("terminalize exhausted async hook: %w", err)
			}
			continue
		}
		var invocation AsyncHookInvocation
		if err := json.Unmarshal(item.payload, &invocation); err != nil {
			if terminalErr := s.failMalformedAsyncHook(ctx, tx, tenant, item.id, item.fence, "persisted lifecycle invocation JSON is invalid", stamp); terminalErr != nil {
				return nil, terminalErr
			}
			continue
		}
		if invocation.TenantID != tenant {
			if terminalErr := s.failMalformedAsyncHook(ctx, tx, tenant, item.id, item.fence, "persisted lifecycle invocation tenant does not match its tenant row", stamp); terminalErr != nil {
				return nil, terminalErr
			}
			continue
		}
		leaseUpdate := `UPDATE _kora_script_invocation SET status = 'running', attempts = attempts + 1, lease_owner = ?, lease_fence = lease_fence + 1, lease_until = ` + s.leaseUpdateExpression() + `, external_effect_started = FALSE, updated_at = ` + stamp + ` WHERE id = ? AND tenant_id = ? AND lease_fence = ? AND ((status = 'pending' AND next_attempt_at <= ` + stamp + `) OR (status = 'running' AND external_effect_started = FALSE AND lease_until <= ` + stamp + `))`
		result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, leaseUpdate), owner, s.leaseArgument(lease), item.id, string(tenant), item.fence)
		if err != nil {
			return nil, fmt.Errorf("fence async hook claim: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("verify async hook claim: %w", err)
		}
		if changed != 1 {
			continue
		}
		claims = append(claims, AsyncHookClaim{Invocation: invocation, Owner: owner, Fence: item.fence + 1, Attempt: uint8(item.attempts + 1), LeaseUntil: time.Now().UTC().Add(lease)})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit async hook claims: %w", err)
	}
	return claims, nil
}

func (s *Store) failMalformedAsyncHook(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, id string, fence uint64, reason, stamp string) error {
	query := `UPDATE _kora_script_invocation SET status = 'failed', lease_owner = NULL, lease_until = NULL, last_error = ?, updated_at = ` + stamp + ` WHERE id = ? AND tenant_id = ? AND lease_fence = ? AND ((status = 'pending' AND next_attempt_at <= ` + stamp + `) OR (status = 'running' AND external_effect_started = FALSE AND lease_until <= ` + stamp + `))`
	result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, query), reason, id, string(tenant), fence)
	if err != nil {
		return fmt.Errorf("terminalize malformed async lifecycle invocation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify malformed async lifecycle invocation terminal state: %w", err)
	}
	if changed != 1 {
		return errors.New("malformed async lifecycle invocation changed during claim")
	}
	return nil
}

func (s *Store) BeginAsyncHook(ctx context.Context, claim AsyncHookClaim) (*sql.Tx, error) {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 || claim.Attempt == 0 {
		return nil, errors.New("async hook execution requires a valid fenced claim")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin async hook execution: %w", err)
	}
	stamp := s.currentTimestamp()
	query := `SELECT id FROM _kora_script_invocation WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + stamp
	var id string
	if err := tx.QueryRowContext(ctx, db.Rebind(s.Dialect, query), string(claim.Invocation.TenantID), string(claim.Invocation.SourceOperation), claim.Invocation.Sequence, claim.Owner, claim.Fence).Scan(&id); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("validate async hook claim: %w", err)
	}
	return tx, nil
}

func (s *Store) MarkAsyncHookExternalEffect(ctx context.Context, claim AsyncHookClaim) error {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 {
		return errors.New("mark external async hook effect requires a valid claim")
	}
	stamp := s.currentTimestamp()
	query := `UPDATE _kora_script_invocation SET external_effect_started = TRUE, updated_at = ` + stamp + ` WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + stamp
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, query), string(claim.Invocation.TenantID), string(claim.Invocation.SourceOperation), claim.Invocation.Sequence, claim.Owner, claim.Fence)
	if err != nil {
		return fmt.Errorf("mark async hook external effect: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify async hook external effect marker: %w", err)
	}
	if changed != 1 {
		return errors.New("async hook claim was lost before external effect")
	}
	return nil
}

func (s *Store) SucceedAsyncHookTx(ctx context.Context, tx *sql.Tx, claim AsyncHookClaim) error {
	if s == nil || s.Dialect == nil || ctx == nil || tx == nil || claim.Owner == "" || claim.Fence == 0 {
		return errors.New("async hook success requires transaction and fenced claim")
	}
	stamp := s.currentTimestamp()
	query := `UPDATE _kora_script_invocation SET status = 'succeeded', lease_owner = NULL, lease_until = NULL, last_error = '', updated_at = ` + stamp + ` WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND lease_until > ` + stamp
	result, err := tx.ExecContext(ctx, db.Rebind(s.Dialect, query), string(claim.Invocation.TenantID), string(claim.Invocation.SourceOperation), claim.Invocation.Sequence, claim.Owner, claim.Fence)
	if err != nil {
		return fmt.Errorf("complete async hook in source-effects transaction: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify async hook success: %w", err)
	}
	if changed != 1 {
		return errors.New("async hook claim was lost before transaction commit")
	}
	return nil
}

func (s *Store) FinishAsyncHook(ctx context.Context, claim AsyncHookClaim, outcome AsyncHookOutcome) error {
	if s == nil || s.DB == nil || s.Dialect == nil || ctx == nil || claim.Owner == "" || claim.Fence == 0 {
		return errors.New("async hook finish requires a valid fenced claim")
	}
	if outcome.Success && (outcome.Retry || outcome.Uncertain || outcome.Error != "") {
		return errors.New("successful async hook cannot include failure metadata")
	}
	if outcome.RetryAfter < 0 || outcome.RetryAfter > maxAsyncHookRetryDelay {
		return errors.New("async hook retry delay exceeds its bound")
	}
	outcome.Error = strings.ToValidUTF8(outcome.Error, "�")
	if len(outcome.Error) > 2048 {
		outcome.Error = outcome.Error[:2048]
		for !utf8.ValidString(outcome.Error) {
			outcome.Error = outcome.Error[:len(outcome.Error)-1]
		}
	}
	status := "failed"
	if outcome.Uncertain {
		status = "uncertain"
	} else if outcome.Retry && claim.Attempt < MaxAsyncHookAttempts {
		status = "pending"
	} else if outcome.Success {
		status = "succeeded"
	}
	stamp := s.currentTimestamp()
	nextAttempt := stamp
	args := []any{status}
	if status == "pending" {
		if outcome.RetryAfter == 0 {
			outcome.RetryAfter = time.Second
		}
		nextAttempt = s.leaseUpdateExpression()
		args = append(args, s.leaseArgument(outcome.RetryAfter))
	}
	args = append(args, outcome.Error, string(claim.Invocation.TenantID), string(claim.Invocation.SourceOperation), claim.Invocation.Sequence, claim.Owner, claim.Fence)
	query := `UPDATE _kora_script_invocation SET status = ?, next_attempt_at = ` + nextAttempt + `, lease_owner = NULL, lease_until = NULL, last_error = ?, updated_at = ` + stamp + ` WHERE tenant_id = ? AND source_operation_id = ? AND sequence_no = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ?`
	if status == "pending" || status == "failed" {
		query += ` AND lease_until > ` + stamp
	}
	result, err := s.DB.ExecContext(ctx, db.Rebind(s.Dialect, query), args...)
	if err != nil {
		return fmt.Errorf("finish async hook: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify async hook finish: %w", err)
	}
	if changed != 1 {
		return errors.New("async hook claim was lost or already completed")
	}
	return nil
}

func (s *Store) currentTimestamp() string {
	if s.Dialect.DriverName() == "mysql" {
		return "CURRENT_TIMESTAMP(6)"
	}
	return "CURRENT_TIMESTAMP"
}

func (s *Store) leaseUpdateExpression() string {
	switch s.Dialect.DriverName() {
	case "mysql":
		return "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND)"
	case "postgres":
		return "CURRENT_TIMESTAMP + (? * INTERVAL '1 microsecond')"
	default:
		return "datetime('now', '+' || ? || ' seconds')"
	}
}

func (s *Store) leaseArgument(value time.Duration) any {
	if s.Dialect.DriverName() == "libsql" {
		return fmt.Sprintf("%.6f", value.Seconds())
	}
	return value.Microseconds()
}
