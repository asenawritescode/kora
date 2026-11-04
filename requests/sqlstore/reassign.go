package sqlstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

const requestReassignOperation = "request.reassign"

// Reassign atomically transfers a pending assignment, records the immutable
// history entry, publishes invalidations and stores the retry receipt.
func (s *Store) Reassign(ctx context.Context, change requests.ReassignCommit) (requests.RequestRecord, error) {
	if err := validateReassignCommit(change); err != nil {
		return requests.RequestRecord{}, err
	}
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request reassignment requires a context")
	}
	if prior, found, err := s.lookupCancelReceipt(ctx, change.Identity, change.RequestID); found || err != nil {
		return prior, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("begin request reassignment: %w", err)
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
	var due bool
	var now db.Timestamp
	query := `SELECT revision, status, (due_at IS NOT NULL AND due_at <= ` + s.dialect.NowTimestamp() + `), ` + s.dialect.NowTimestamp() + ` FROM _kora_request WHERE tenant_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(change.Identity.Tenant), string(change.RequestID)).Scan(&revision, &status, &due, &now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(contract.NewError(contract.CodeNotFound, "request not found"))
		}
		return fail(fmt.Errorf("lock request for reassignment: %w", err))
	}
	if strconv.FormatInt(revision, 10) != string(change.ExpectedRevision) {
		return fail(contract.NewError(contract.CodeConflict, "request changed; refresh before reassigning"))
	}
	if status != string(contract.RequestPending) || due {
		return fail(contract.NewError(contract.CodeConflict, "request is no longer assignable"))
	}
	var currentStatus, currentAssigneeKind, currentAssigneeID string
	assignmentQuery := `SELECT status, assignee_kind, assignee_id FROM _kora_request_assignment WHERE tenant_id = ? AND request_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		assignmentQuery += " FOR UPDATE"
	}
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, assignmentQuery), string(change.Identity.Tenant), string(change.RequestID), string(change.AssignmentID)).Scan(&currentStatus, &currentAssigneeKind, &currentAssigneeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fail(contract.NewError(contract.CodeNotFound, "assignment not found"))
		}
		return fail(fmt.Errorf("lock assignment for reassignment: %w", err))
	}
	if currentAssigneeKind != string(contract.AssigneeUser) {
		return fail(contract.NewError(contract.CodeInternal, "stored expanded assignment is not a user"))
	}
	if currentAssigneeID == string(change.Replacement.Principal) {
		return fail(contract.NewError(contract.CodeConflict, "assignment already has this assignee"))
	}
	if currentStatus != string(contract.AssignmentPending) && currentStatus != string(contract.AssignmentBlocked) {
		return fail(contract.NewError(contract.CodeConflict, "assignment is no longer active"))
	}
	now.Time = now.UTC()
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET assignee_kind = ?, assignee_id = ?, membership_revision = ?, status = ?, updated_at = `+s.dialect.NowTimestamp()+`
		WHERE tenant_id = ? AND request_id = ? AND id = ? AND status IN (?, ?)`), string(contract.AssigneeUser), string(change.Replacement.Principal), string(change.ExpectedMembershipRevision), currentStatus, string(change.Identity.Tenant), string(change.RequestID), string(change.AssignmentID), string(contract.AssignmentPending), string(contract.AssignmentBlocked))
	if err != nil {
		return fail(fmt.Errorf("update reassigned participant: %w", err))
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fail(contract.NewError(contract.CodeConflict, "assignment changed while reassigning"))
	}
	newRevision := revision + 1
	result, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request SET revision = ?, updated_at = `+s.dialect.NowTimestamp()+` WHERE tenant_id = ? AND id = ? AND revision = ? AND status = ?`), newRevision, string(change.Identity.Tenant), string(change.RequestID), revision, string(contract.RequestPending))
	if err != nil {
		return fail(fmt.Errorf("advance request revision: %w", err))
	}
	changed, err = result.RowsAffected()
	if err != nil || changed != 1 {
		return fail(contract.NewError(contract.CodeConflict, "request changed while reassigning"))
	}
	actor := change.Identity.Actor
	previous := contract.Assignee{Kind: contract.AssigneeUser, ID: currentAssigneeID}
	next := contract.Assignee{Kind: contract.AssigneeUser, ID: string(change.Replacement.Principal)}
	history := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(newRevision, 10)), Kind: contract.HistoryReassigned, Actor: &actor, AssignmentID: change.AssignmentID, PreviousAssignee: &previous, NewAssignee: &next, Comment: change.Reason, At: now.Time}
	if err := insertHistory(ctx, tx, s.dialect, change.Identity.Tenant, change.RequestID, history); err != nil {
		return fail(fmt.Errorf("append reassignment history: %w", err))
	}
	for _, event := range change.Events {
		if err := s.events.Append(ctx, tx, event); err != nil {
			return fail(fmt.Errorf("append reassignment event %s: %w", event.Type, err))
		}
	}
	if err := insertCancelReceipt(ctx, tx, s.dialect, change.Identity, change.RequestID, now.Time); err != nil {
		return fail(fmt.Errorf("store reassignment idempotency receipt: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("commit reassignment: %w", err))
	}
	committed = true
	return s.Get(ctx, change.Identity.Tenant, change.RequestID)
}

func validateReassignCommit(change requests.ReassignCommit) error {
	identity := change.Identity
	if identity.Operation != requestReassignOperation || identity.Tenant == "" || identity.Actor == "" {
		return contract.NewError(contract.CodeValidationFailed, "request reassignment identity is invalid")
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
	if _, err := contract.ParseAssignmentID(string(change.AssignmentID)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(change.Replacement.Principal)); err != nil {
		return err
	}
	if change.Replacement.ID != change.AssignmentID || change.Replacement.RequestID != change.RequestID {
		return contract.NewError(contract.CodeValidationFailed, "replacement must refer to the reassigned assignment")
	}
	if _, err := contract.ParseIdempotencyKey(string(identity.IdempotencyKey)); err != nil {
		return err
	}
	if len(identity.CommandDigest) != 64 || strings.ToLower(identity.CommandDigest) != identity.CommandDigest {
		return contract.NewError(contract.CodeValidationFailed, "request reassignment digest is invalid")
	}
	if _, err := hex.DecodeString(identity.CommandDigest); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "request reassignment digest is invalid")
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
	if err := contract.ValidateReassignRequest(contract.ReassignRequest{AssignmentID: change.AssignmentID, To: contract.Assignee{Kind: contract.AssigneeUser, ID: string(change.Replacement.Principal)}, Reason: change.Reason}); err != nil {
		return err
	}
	return nil
}
