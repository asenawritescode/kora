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
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/requests"
)

const requestRespondOperation = "request.respond"

// CommitDecision durably records one authorized assignment response. It locks
// the tenant authorization epochs and request row, then updates the decision,
// quorum state, history, outbox and idempotency receipt in one transaction.
// The caller must re-evaluate actor eligibility and subject visibility before
// constructing this commit; the epochs make that check stable through commit.
func (s *Store) CommitDecision(ctx context.Context, change requests.DecisionCommit) (contract.ResponseReceipt, error) {
	return s.commitDecision(ctx, nil, change)
}

// CommitDecisionInTx performs the response using the transaction owned by a
// composition-layer unit of work. The caller is responsible for commit or
// rollback; no second connection is acquired by this method.
func (s *Store) CommitDecisionInTx(ctx context.Context, tx *sql.Tx, change requests.DecisionCommit) (contract.ResponseReceipt, error) {
	if tx == nil {
		return contract.ResponseReceipt{}, contract.NewError(contract.CodeValidationFailed, "request decision requires an owning transaction")
	}
	return s.commitDecision(ctx, tx, change)
}

func (s *Store) commitDecision(ctx context.Context, tx *sql.Tx, change requests.DecisionCommit) (contract.ResponseReceipt, error) {
	ownsTransaction := tx == nil
	if err := validateDecisionCommit(change); err != nil {
		return contract.ResponseReceipt{}, err
	}
	if ctx == nil {
		return contract.ResponseReceipt{}, fmt.Errorf("request decision requires a context")
	}
	var prior contract.ResponseReceipt
	var found bool
	var err error
	if ownsTransaction {
		prior, found, err = s.lookupDecisionReceipt(ctx, change.Identity)
	} else {
		prior, found, err = s.lookupDecisionReceiptTx(ctx, tx, change.Identity)
	}
	if found || err != nil {
		return prior, err
	}
	if ownsTransaction {
		var err error
		tx, err = s.database.BeginTx(ctx, nil)
		if err != nil {
			return contract.ResponseReceipt{}, fmt.Errorf("begin request decision: %w", err)
		}
	}
	committed := false
	defer func() {
		if ownsTransaction && !committed {
			_ = tx.Rollback()
		}
	}()
	fail := func(cause error) (contract.ResponseReceipt, error) {
		if !ownsTransaction {
			return contract.ResponseReceipt{}, cause
		}
		_ = tx.Rollback()
		committed = true
		if ctx.Err() != nil {
			return contract.ResponseReceipt{}, cause
		}
		if prior, found, lookupErr := s.lookupDecisionReceipt(ctx, change.Identity); found {
			return prior, lookupErr
		} else if lookupErr != nil {
			return contract.ResponseReceipt{}, errors.Join(cause, lookupErr)
		}
		return contract.ResponseReceipt{}, cause
	}
	if err := lockAndCheckAuthorizationRevision(ctx, tx, s.dialect, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision); err != nil {
		return fail(err)
	}

	var currentRevision int64
	var status, mode string
	var subjectKind, subjectID, subjectRevision sql.NullString
	var isDue bool
	requestQuery := `SELECT revision, status, assignment_mode, subject_kind, subject_id, subject_revision,
		(due_at IS NOT NULL AND due_at <= ` + s.dialect.NowTimestamp() + `)
		FROM _kora_request WHERE tenant_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		requestQuery += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, requestQuery), string(change.Identity.Tenant), string(change.Decision.RequestID)).Scan(
		&currentRevision, &status, &mode, &subjectKind, &subjectID, &subjectRevision, &isDue)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(contract.NewError(contract.CodeNotFound, "request not found"))
	}
	if err != nil {
		return fail(fmt.Errorf("lock request for response: %w", err))
	}
	if currentRevision < 0 || strconv.FormatInt(currentRevision, 10) != string(change.ExpectedRevision) {
		return fail(contract.NewError(contract.CodeConflict, "request changed; refresh before responding"))
	}
	if status != string(contract.RequestPending) {
		return fail(contract.NewError(contract.CodeConflict, "request is no longer pending"))
	}
	if isDue {
		return fail(contract.NewError(contract.CodeConflict, "request deadline passed; expiry must be persisted before responding"))
	}
	if err := checkExpectedSubject(subjectKind, subjectID, subjectRevision, change.ExpectedSubject); err != nil {
		return fail(err)
	}
	assignmentMode := contract.AssignmentMode(mode)
	if assignmentMode != contract.AssignmentAny && assignmentMode != contract.AssignmentAll && assignmentMode != contract.AssignmentSequential {
		return fail(contract.NewError(contract.CodeInternal, "stored request assignment mode is invalid"))
	}

	var assignedActor, assignmentStatus string
	var assignedStage int64
	var allowedRaw []byte
	assignmentQuery := `SELECT assignee_id, stage, status, allowed_responses FROM _kora_request_assignment
		WHERE tenant_id = ? AND request_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		assignmentQuery += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, assignmentQuery), string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID)).Scan(
		&assignedActor, &assignedStage, &assignmentStatus, &allowedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return fail(contract.NewError(contract.CodeNotFound, "request assignment not found"))
	}
	if err != nil {
		return fail(fmt.Errorf("lock request assignment: %w", err))
	}
	if assignedActor != string(change.Decision.Actor) || assignmentStatus != string(contract.AssignmentPending) {
		return fail(contract.NewError(contract.CodePermissionDenied, "actor is not the current pending assignee"))
	}
	var allowed []contract.ResponseKind
	if err := json.Unmarshal(allowedRaw, &allowed); err != nil {
		return fail(contract.NewError(contract.CodeInternal, "stored allowed responses are invalid"))
	}
	if !containsResponse(allowed, change.Decision.Response) {
		return fail(contract.NewError(contract.CodePermissionDenied, "response is not allowed for this assignment"))
	}
	if assignmentMode == contract.AssignmentSequential && assignmentStatus != string(contract.AssignmentPending) {
		return fail(contract.NewError(contract.CodeConflict, "sequential assignment is not active"))
	}
	if assignmentMode == contract.AssignmentSequential {
		var earlierPending int64
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_request_assignment
			WHERE tenant_id = ? AND request_id = ? AND status IN (?, ?) AND (stage < ? OR (stage = ? AND id < ?))`),
			string(change.Identity.Tenant), string(change.Decision.RequestID), string(contract.AssignmentPending), string(contract.AssignmentBlocked), assignedStage, assignedStage, string(change.Decision.AssignmentID)).Scan(&earlierPending); err != nil {
			return fail(fmt.Errorf("verify current sequential assignment: %w", err))
		}
		if earlierPending > 0 {
			return fail(contract.NewError(contract.CodeConflict, "an earlier sequential assignment is still active"))
		}
	}

	informationJSON, err := marshalInformation(change.Decision.Information)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_request_decision
		(id, tenant_id, request_id, assignment_id, actor_id, on_behalf_of_id, response, comment, information_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		string(change.Decision.ID), string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID),
		string(change.Decision.Actor), string(change.Decision.OnBehalfOf), string(change.Decision.Response), change.Decision.Comment,
		informationJSON, change.Decision.At.UTC()); err != nil {
		return fail(fmt.Errorf("insert immutable request decision: %w", err))
	}
	updated, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = `+s.dialect.NowTimestamp()+`
		WHERE tenant_id = ? AND request_id = ? AND id = ? AND status = ?`),
		string(contract.AssignmentResponded), string(change.Identity.Tenant), string(change.Decision.RequestID), string(change.Decision.AssignmentID), string(contract.AssignmentPending))
	if err != nil {
		return fail(fmt.Errorf("mark assignment responded: %w", err))
	}
	rowsChanged, err := updated.RowsAffected()
	if err != nil || rowsChanged != 1 {
		return fail(contract.NewError(contract.CodeConflict, "assignment changed while responding"))
	}

	outcome := responseOutcome(change.Decision.Response)
	resolved := change.Decision.Response == contract.ResponseReject || change.Decision.Response == contract.ResponseReturn
	switch assignmentMode {
	case contract.AssignmentAny:
		resolved = true
	case contract.AssignmentAll:
		if !resolved {
			var pending int64
			if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_request_assignment
				WHERE tenant_id = ? AND request_id = ? AND status = ?`), string(change.Identity.Tenant), string(change.Decision.RequestID), string(contract.AssignmentPending)).Scan(&pending); err != nil {
				return fail(fmt.Errorf("count remaining request assignments: %w", err))
			}
			resolved = pending == 0
		}
	case contract.AssignmentSequential:
		if !resolved {
			var nextID string
			err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT id FROM _kora_request_assignment
				WHERE tenant_id = ? AND request_id = ? AND status = ? AND stage > ? ORDER BY stage, id LIMIT 1`),
				string(change.Identity.Tenant), string(change.Decision.RequestID), string(contract.AssignmentBlocked), assignedStage).Scan(&nextID)
			if err == nil {
				result, updateErr := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = `+s.dialect.NowTimestamp()+`
					WHERE tenant_id = ? AND request_id = ? AND id = ? AND status = ?`), string(contract.AssignmentPending), string(change.Identity.Tenant), string(change.Decision.RequestID), nextID, string(contract.AssignmentBlocked))
				if updateErr != nil {
					return fail(fmt.Errorf("activate next sequential assignment: %w", updateErr))
				}
				changed, rowsErr := result.RowsAffected()
				if rowsErr != nil || changed != 1 {
					return fail(contract.NewError(contract.CodeConflict, "next sequential assignment changed"))
				}
			} else if errors.Is(err, sql.ErrNoRows) {
				resolved = true
			} else {
				return fail(fmt.Errorf("find next sequential assignment: %w", err))
			}
		}
	}

	newRevision := currentRevision + 1
	now := change.Decision.At.UTC()
	var terminalStatus any
	var resolvedAt any
	var outcomeValue any
	if resolved {
		terminalStatus = string(contract.RequestResolved)
		resolvedAt = now
		outcomeValue = string(outcome)
		if err := supersedeRemainingAssignments(ctx, tx, s.dialect, change.Identity.Tenant, change.Decision.RequestID); err != nil {
			return fail(err)
		}
	}
	continuationID, continuationState := "", ""
	if change.Receipt.Continuation != nil {
		continuationID = string(change.Receipt.Continuation.OperationID)
		continuationState = string(change.Receipt.Continuation.State)
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request SET revision = ?, status = COALESCE(?, status), outcome = COALESCE(?, outcome),
		resolved_at = COALESCE(?, resolved_at), continuation_operation_id = CASE WHEN ? = '' THEN continuation_operation_id ELSE ? END,
		continuation_state = CASE WHEN ? = '' THEN continuation_state ELSE ? END, updated_at = ?
		WHERE tenant_id = ? AND id = ? AND revision = ? AND status = ?`),
		newRevision, terminalStatus, outcomeValue, resolvedAt, continuationID, continuationID, continuationState, continuationState, now,
		string(change.Identity.Tenant), string(change.Decision.RequestID), currentRevision, string(contract.RequestPending))
	if err != nil {
		return fail(fmt.Errorf("advance request revision: %w", err))
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fail(contract.NewError(contract.CodeConflict, "request changed while responding"))
	}

	response := change.Decision.Response
	actor := change.Decision.Actor
	history := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(newRevision, 10)), Kind: contract.HistoryResponded, Actor: &actor,
		AssignmentID: change.Decision.AssignmentID, Response: &response, Comment: change.Decision.Comment, At: now}
	if err := insertHistory(ctx, tx, s.dialect, change.Identity.Tenant, change.Decision.RequestID, history); err != nil {
		return fail(fmt.Errorf("append response history: %w", err))
	}
	if resolved {
		sequence := contract.Revision(strconv.FormatInt(newRevision+1, 10))
		resolvedHistory := contract.RequestHistoryEntry{Sequence: sequence, Kind: contract.HistoryResolved, Actor: &actor, At: now}
		if err := insertHistory(ctx, tx, s.dialect, change.Identity.Tenant, change.Decision.RequestID, resolvedHistory); err != nil {
			return fail(fmt.Errorf("append resolution history: %w", err))
		}
	}
	for _, event := range change.Events {
		if err := s.events.Append(ctx, tx, event); err != nil {
			return fail(fmt.Errorf("append request response event %s: %w", event.Type, err))
		}
	}
	receipt := contract.ResponseReceipt{RequestID: change.Decision.RequestID, DecisionID: change.Decision.ID,
		Revision: contract.Revision(strconv.FormatInt(newRevision, 10)), Status: contract.RequestPending}
	if resolved {
		receipt.Status, receipt.Outcome = contract.RequestResolved, &outcome
	}
	if change.Receipt.Continuation != nil {
		receipt.Continuation = change.Receipt.Continuation
	}
	if err := insertDecisionIdempotencyReceipt(ctx, tx, s.dialect, change.Identity, receipt, now); err != nil {
		return fail(fmt.Errorf("store response idempotency receipt: %w", err))
	}
	if ownsTransaction {
		if err := tx.Commit(); err != nil {
			return fail(fmt.Errorf("commit request decision: %w", err))
		}
		committed = true
	}
	return receipt, nil
}

func validateDecisionCommit(change requests.DecisionCommit) error {
	identity, decision := change.Identity, change.Decision
	if identity.Operation != requestRespondOperation || identity.Tenant == "" || identity.Actor == "" || identity.Actor != decision.Actor || decision.Tenant != identity.Tenant || change.Request.Tenant != identity.Tenant || change.Request.ID != decision.RequestID || change.Request.Revision != change.ExpectedRevision {
		return contract.NewError(contract.CodeValidationFailed, "request response identity is invalid")
	}
	if _, err := contract.ParseTenantID(string(identity.Tenant)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(identity.Actor)); err != nil {
		return err
	}
	if decision.OnBehalfOf != "" {
		if _, err := contract.ParsePrincipalID(string(decision.OnBehalfOf)); err != nil {
			return err
		}
	}
	if _, err := contract.ParseRequestID(string(decision.RequestID)); err != nil {
		return err
	}
	if _, err := contract.ParseAssignmentID(string(decision.AssignmentID)); err != nil {
		return err
	}
	if _, err := contract.ParseDecisionID(string(decision.ID)); err != nil {
		return err
	}
	if _, err := contract.ParseIdempotencyKey(string(identity.IdempotencyKey)); err != nil {
		return err
	}
	if len(identity.CommandDigest) != 64 || strings.ToLower(identity.CommandDigest) != identity.CommandDigest {
		return contract.NewError(contract.CodeValidationFailed, "request response command digest is invalid")
	}
	if _, err := hex.DecodeString(identity.CommandDigest); err != nil {
		return contract.NewError(contract.CodeValidationFailed, "request response command digest is invalid")
	}
	if len(change.ExpectedRevision) == 0 {
		return contract.NewError(contract.CodeValidationFailed, "request revision is required")
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
	if change.ExpectedSubject != nil {
		if _, err := contract.ParseRevision(string(change.ExpectedSubject.Revision)); err != nil {
			return err
		}
	}
	if decision.Response != contract.ResponseApprove && decision.Response != contract.ResponseReject && decision.Response != contract.ResponseReturn && decision.Response != contract.ResponseProvide && decision.Response != contract.ResponseAcknowledge && decision.Response != contract.ResponseComplete {
		return contract.NewError(contract.CodeValidationFailed, "request response value is invalid")
	}
	if decision.At.IsZero() || change.Receipt.RequestID != decision.RequestID || change.Receipt.DecisionID != decision.ID {
		return contract.NewError(contract.CodeValidationFailed, "request response receipt binding is invalid")
	}
	if !sameContinuation(change.Request.Continuation, change.Receipt.Continuation) {
		return contract.NewError(contract.CodeValidationFailed, "request response continuation binding is invalid")
	}
	if err := contract.ValidateRespondRequest(contract.RespondRequest{AssignmentID: decision.AssignmentID, Response: decision.Response, Comment: decision.Comment, Information: decision.Information}); err != nil {
		return err
	}
	if decision.Response == contract.ResponseProvide && decision.Information == nil {
		return contract.NewError(contract.CodeValidationFailed, "information response payload is required")
	}
	if decision.Response != contract.ResponseProvide && decision.Information != nil {
		return contract.NewError(contract.CodeValidationFailed, "information payload is only valid for information responses")
	}
	return nil
}

func sameContinuation(a, b *contract.ContinuationView) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OperationID == b.OperationID && a.State == b.State
}

func checkExpectedSubject(kind, id, revision sql.NullString, expected *contract.SubjectRef) error {
	if expected == nil {
		if kind.Valid || id.Valid || revision.Valid {
			return contract.NewError(contract.CodeConflict, "request subject binding changed")
		}
		return nil
	}
	if !kind.Valid || !id.Valid || !revision.Valid || kind.String != expected.Kind || id.String != expected.ID || revision.String != string(expected.Revision) {
		return contract.NewError(contract.CodeConflict, "request subject changed; approval is stale")
	}
	return nil
}

func containsResponse(values []contract.ResponseKind, response contract.ResponseKind) bool {
	for _, value := range values {
		if value == response {
			return true
		}
	}
	return false
}

func responseOutcome(response contract.ResponseKind) contract.RequestOutcome {
	switch response {
	case contract.ResponseApprove:
		return contract.OutcomeApproved
	case contract.ResponseReject:
		return contract.OutcomeRejected
	case contract.ResponseReturn:
		return contract.OutcomeReturned
	case contract.ResponseProvide:
		return contract.OutcomeProvided
	case contract.ResponseAcknowledge:
		return contract.OutcomeAcknowledged
	default:
		return contract.OutcomeCompleted
	}
}

func marshalInformation(value *contract.InformationResponse) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode information response: %w", err)
	}
	return string(encoded), nil
}

func supersedeRemainingAssignments(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant contract.TenantID, requestID contract.RequestID) error {
	_, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = `+dialect.NowTimestamp()+`
		WHERE tenant_id = ? AND request_id = ? AND status IN (?, ?)`), string(contract.AssignmentSuperseded), string(tenant), string(requestID), string(contract.AssignmentPending), string(contract.AssignmentBlocked))
	if err != nil {
		return fmt.Errorf("supersede remaining assignments: %w", err)
	}
	return nil
}

type decisionReceiptEnvelope struct {
	Receipt contract.ResponseReceipt `json:"receipt"`
}

func insertDecisionIdempotencyReceipt(ctx context.Context, tx *sql.Tx, dialect db.Dialect, identity requests.WriteIdentity, receipt contract.ResponseReceipt, now time.Time) error {
	encoded, err := json.Marshal(decisionReceiptEnvelope{Receipt: receipt})
	if err != nil {
		return fmt.Errorf("encode response receipt: %w", err)
	}
	query := `INSERT INTO _kora_request_idempotency (tenant_id, actor_id, operation, idempotency_key, command_digest, receipt_json, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + idempotencyExpiryExpression(dialect) + `)`
	_, err = tx.ExecContext(ctx, db.Rebind(dialect, query), string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey), identity.CommandDigest, string(encoded), now.UTC())
	return err
}

func (s *Store) lookupDecisionReceipt(ctx context.Context, identity requests.WriteIdentity) (contract.ResponseReceipt, bool, error) {
	return s.lookupDecisionReceiptWith(ctx, s.database, identity)
}

// LookupResponseReceipt is the public request-service seam for resolving a
// same-command retry before checking the now-completed assignment again.
func (s *Store) LookupResponseReceipt(ctx context.Context, identity requests.WriteIdentity) (contract.ResponseReceipt, bool, error) {
	if identity.Operation != requestRespondOperation || identity.Tenant == "" || identity.Actor == "" || identity.IdempotencyKey == "" || identity.CommandDigest == "" {
		return contract.ResponseReceipt{}, false, contract.NewError(contract.CodeValidationFailed, "response receipt identity is incomplete")
	}
	return s.lookupDecisionReceipt(ctx, identity)
}

func (s *Store) lookupDecisionReceiptTx(ctx context.Context, tx *sql.Tx, identity requests.WriteIdentity) (contract.ResponseReceipt, bool, error) {
	return s.lookupDecisionReceiptWith(ctx, tx, identity)
}

type decisionReceiptQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) lookupDecisionReceiptWith(ctx context.Context, queryer decisionReceiptQueryer, identity requests.WriteIdentity) (contract.ResponseReceipt, bool, error) {
	var digest, raw string
	var active bool
	err := queryer.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest, receipt_json, (expires_at > `+s.dialect.NowTimestamp()+`) FROM _kora_request_idempotency WHERE tenant_id = ? AND actor_id = ? AND operation = ? AND idempotency_key = ?`),
		string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey)).Scan(&digest, &raw, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.ResponseReceipt{}, false, nil
	}
	if err != nil {
		return contract.ResponseReceipt{}, false, fmt.Errorf("read response idempotency receipt: %w", err)
	}
	if !active {
		return contract.ResponseReceipt{}, true, contract.NewError(contract.CodeConflict, "response retry window expired; query request outcome")
	}
	if digest != identity.CommandDigest {
		return contract.ResponseReceipt{}, true, contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key was used for a different response")
	}
	var receipt decisionReceiptEnvelope
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return contract.ResponseReceipt{}, true, fmt.Errorf("decode response receipt: %w", err)
	}
	if _, err := contract.ParseRequestID(string(receipt.Receipt.RequestID)); err != nil {
		return contract.ResponseReceipt{}, true, contract.NewError(contract.CodeInternal, "stored response receipt request ID is invalid")
	}
	if _, err := contract.ParseDecisionID(string(receipt.Receipt.DecisionID)); err != nil {
		return contract.ResponseReceipt{}, true, contract.NewError(contract.CodeInternal, "stored response receipt decision ID is invalid")
	}
	return receipt.Receipt, true, nil
}
