package sqlstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/requests"
)

const requestCancelOperation = "request.cancel"

// Cancel atomically closes a pending request, stops outstanding assignments,
// appends history/events and stores the retry receipt. Policy authorization is
// evaluated by the service and fenced by the authorization epochs here.
func (s *Store) Cancel(ctx context.Context, change requests.CancelCommit) (requests.RequestRecord, error) {
	if err := validateCancelCommit(change); err != nil {
		return requests.RequestRecord{}, err
	}
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request cancellation requires a context")
	}
	if prior, found, err := s.lookupCancelReceipt(ctx, change.Identity, change.RequestID); found || err != nil {
		return prior, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("begin request cancellation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	fail := func(cause error) (requests.RequestRecord, error) {
		_ = tx.Rollback()
		if ctx.Err() != nil {
			return requests.RequestRecord{}, cause
		}
		if prior, found, lookupErr := s.lookupCancelReceipt(ctx, change.Identity, change.RequestID); found {
			return prior, lookupErr
		} else if lookupErr != nil {
			return requests.RequestRecord{}, errors.Join(cause, lookupErr)
		}
		return requests.RequestRecord{}, cause
	}
	if err := lockAndCheckAuthorizationRevision(ctx, tx, s.dialect, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision); err != nil {
		return fail(err)
	}

	var revision int64
	var status string
	var isDue bool
	var now db.Timestamp
	nowExpr := s.dialect.NowTimestamp()
	query := `SELECT revision, status, (due_at IS NOT NULL AND due_at <= ` + nowExpr + `), ` + nowExpr + `
		FROM _kora_request WHERE tenant_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(change.Identity.Tenant), string(change.RequestID)).Scan(&revision, &status, &isDue, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(contract.NewError(contract.CodeNotFound, "request not found"))
	}
	if err != nil {
		return fail(fmt.Errorf("lock request for cancellation: %w", err))
	}
	if revision < 0 || strconv.FormatInt(revision, 10) != string(change.ExpectedRevision) {
		return fail(contract.NewError(contract.CodeConflict, "request changed; refresh before cancelling"))
	}
	if status != string(contract.RequestPending) {
		return fail(contract.NewError(contract.CodeConflict, "request is no longer pending"))
	}
	if isDue {
		return fail(contract.NewError(contract.CodeConflict, "request deadline passed; expiry must be persisted before cancellation"))
	}

	newRevision := revision + 1
	now.Time = now.UTC()
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request SET revision = ?, status = ?, updated_at = `+s.dialect.NowTimestamp()+`
		WHERE tenant_id = ? AND id = ? AND revision = ? AND status = ?`), newRevision, string(contract.RequestCancelled), string(change.Identity.Tenant), string(change.RequestID), revision, string(contract.RequestPending))
	if err != nil {
		return fail(fmt.Errorf("cancel request: %w", err))
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fail(contract.NewError(contract.CodeConflict, "request changed while cancelling"))
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = `+s.dialect.NowTimestamp()+`
		WHERE tenant_id = ? AND request_id = ? AND status IN (?, ?)`), string(contract.AssignmentCancelled), string(change.Identity.Tenant), string(change.RequestID), string(contract.AssignmentPending), string(contract.AssignmentBlocked)); err != nil {
		return fail(fmt.Errorf("cancel pending assignments: %w", err))
	}
	actor := change.Identity.Actor
	history := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(newRevision, 10)), Kind: contract.HistoryCancelled, Actor: &actor, Comment: change.Reason, At: now.Time}
	if err := insertHistory(ctx, tx, s.dialect, change.Identity.Tenant, change.RequestID, history); err != nil {
		return fail(fmt.Errorf("append cancellation history: %w", err))
	}
	for _, event := range change.Events {
		if err := s.events.Append(ctx, tx, event); err != nil {
			return fail(fmt.Errorf("append cancellation event %s: %w", event.Type, err))
		}
	}
	if err := insertCancelReceipt(ctx, tx, s.dialect, change.Identity, change.RequestID, now.Time); err != nil {
		return fail(fmt.Errorf("store cancellation idempotency receipt: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("commit request cancellation: %w", err))
	}
	committed = true
	return s.Get(ctx, change.Identity.Tenant, change.RequestID)
}

func validateCancelCommit(change requests.CancelCommit) error {
	identity := change.Identity
	if identity.Operation != requestCancelOperation || identity.Tenant == "" || identity.Actor == "" {
		return contract.NewError(contract.CodeValidationFailed, "request cancellation identity is invalid")
	}
	if _, err := contract.ParseTenantID(string(identity.Tenant)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(identity.Actor)); err != nil {
		return err
	}
	if _, err := contract.ParseRequestID(string(change.RequestID)); err != nil {
		return err
	}
	if _, err := contract.ParseIdempotencyKey(string(identity.IdempotencyKey)); err != nil {
		return err
	}
	if len(identity.CommandDigest) != 64 || strings.ToLower(identity.CommandDigest) != identity.CommandDigest {
		return contract.NewError(contract.CodeValidationFailed, "request cancellation digest is invalid")
	}
	if _, err := hex.DecodeString(identity.CommandDigest); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "request cancellation digest is invalid")
	}
	if _, err := contract.ParseRevision(string(change.ExpectedRevision)); err != nil {
		return err
	}
	if _, err := contract.ParseRevision(string(change.ExpectedPolicyRevision)); err != nil {
		return err
	}
	if _, err := contract.ParseRevision(string(change.ExpectedMembershipRevision)); err != nil {
		return err
	}
	if err := contract.ValidateCancelRequest(contract.CancelRequest{Reason: change.Reason}); err != nil {
		return err
	}
	return nil
}

type cancelReceiptEnvelope struct {
	RequestID contract.RequestID `json:"request_id"`
}

func insertCancelReceipt(ctx context.Context, tx *sql.Tx, dialect db.Dialect, identity requests.WriteIdentity, requestID contract.RequestID, now time.Time) error {
	encoded, err := json.Marshal(cancelReceiptEnvelope{RequestID: requestID})
	if err != nil {
		return err
	}
	query := `INSERT INTO _kora_request_idempotency (tenant_id, actor_id, operation, idempotency_key, command_digest, receipt_json, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + idempotencyExpiryExpression(dialect) + `)`
	_, err = tx.ExecContext(ctx, db.Rebind(dialect, query), string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey), identity.CommandDigest, string(encoded), now.UTC())
	return err
}

func (s *Store) lookupCancelReceipt(ctx context.Context, identity requests.WriteIdentity, expectedRequest contract.RequestID) (requests.RequestRecord, bool, error) {
	operation := strings.TrimPrefix(identity.Operation, "request.")
	var digest, raw string
	var active bool
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest, receipt_json, (expires_at > `+s.dialect.NowTimestamp()+`) FROM _kora_request_idempotency WHERE tenant_id = ? AND actor_id = ? AND operation = ? AND idempotency_key = ?`),
		string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey)).Scan(&digest, &raw, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return requests.RequestRecord{}, false, nil
	}
	if err != nil {
		return requests.RequestRecord{}, false, fmt.Errorf("read %s idempotency receipt: %w", operation, err)
	}
	if !active {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeConflict, operation+" retry window expired; query request outcome")
	}
	if digest != identity.CommandDigest {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key was used for a different "+operation)
	}
	var receipt cancelReceiptEnvelope
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return requests.RequestRecord{}, true, fmt.Errorf("decode %s receipt: %w", operation, err)
	}
	if _, err := contract.ParseRequestID(string(receipt.RequestID)); err != nil {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeInternal, "stored "+operation+" request ID is invalid")
	}
	if receipt.RequestID != expectedRequest {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key was used for a different "+operation)
	}
	record, err := s.Get(ctx, identity.Tenant, receipt.RequestID)
	if err != nil {
		return requests.RequestRecord{}, true, err
	}
	return record, true, nil
}
