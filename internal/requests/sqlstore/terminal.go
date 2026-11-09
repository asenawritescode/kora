package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
)

const maxRequestExpiryBatch = 500

// ExpireDue closes a bounded batch of requests whose deadline has passed. The
// database clock is sampled in the selection query and reused for every write
// so workers on differently skewed hosts agree with response/cancellation
// checks. Each terminal state, assignment cancellation, history record and
// outbox event is committed in one transaction.
func (s *Store) ExpireDue(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	if ctx == nil {
		return 0, fmt.Errorf("request expiry requires a context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return 0, err
	}
	if limit == 0 || limit > maxRequestExpiryBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "request expiry requires a batch limit from 1 to 500")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin request expiry: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	databaseNow := s.dialect.NowTimestamp()
	query := `SELECT id, revision, ` + databaseNow + ` FROM _kora_request WHERE tenant_id = ? AND status = ? AND due_at IS NOT NULL AND due_at <= ` + databaseNow + ` ORDER BY due_at, id LIMIT ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(contract.RequestPending), int64(limit))
	if err != nil {
		return 0, fmt.Errorf("select due requests: %w", err)
	}
	type dueRequest struct {
		id       contract.RequestID
		revision int64
		dueAt    db.Timestamp
	}
	var due []dueRequest
	for rows.Next() {
		var item dueRequest
		if err := rows.Scan(&item.id, &item.revision, &item.dueAt); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan due request: %w", err)
		}
		if _, err := contract.ParseRequestID(string(item.id)); err != nil {
			_ = rows.Close()
			return 0, contract.NewError(contract.CodeInternal, "stored due request ID is invalid")
		}
		if item.revision < 1 {
			_ = rows.Close()
			return 0, contract.NewError(contract.CodeInternal, "stored due request revision is invalid")
		}
		due = append(due, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read due requests: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close due request rows: %w", err)
	}
	for _, item := range due {
		newRevision := item.revision + 1
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request SET status = ?, revision = ?, resolved_at = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND revision = ? AND status = ? AND due_at <= ?`),
			string(contract.RequestExpired), newRevision, item.dueAt.UTC(), item.dueAt.UTC(), string(tenant), string(item.id), item.revision, string(contract.RequestPending), item.dueAt.UTC())
		if err != nil {
			return 0, fmt.Errorf("expire request %s: %w", item.id, err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return 0, contract.NewError(contract.CodeConflict, "due request changed while expiring")
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = ? WHERE tenant_id = ? AND request_id = ? AND status IN (?, ?)`),
			string(contract.AssignmentCancelled), item.dueAt.UTC(), string(tenant), string(item.id), string(contract.AssignmentPending), string(contract.AssignmentBlocked)); err != nil {
			return 0, fmt.Errorf("close expired assignments for %s: %w", item.id, err)
		}
		history := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(newRevision, 10)), Kind: contract.HistoryExpired, Comment: "Request deadline passed", At: item.dueAt.UTC()}
		if err := insertHistory(ctx, tx, s.dialect, tenant, item.id, history); err != nil {
			return 0, fmt.Errorf("append expiry history for %s: %w", item.id, err)
		}
		if err := s.events.Append(ctx, tx, terminalEvent(tenant, item.id, "request.expired", newRevision, item.dueAt.Time)); err != nil {
			return 0, fmt.Errorf("append expiry event for %s: %w", item.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit request expiry: %w", err)
	}
	committed = true
	return uint16(len(due)), nil
}

// MarkStale persists the terminal transition after the caller has observed a
// different live subject revision. It refuses to stale an unrelated or already
// terminal request and records no subject content in the event payload.
func (s *Store) MarkStale(ctx context.Context, tenant contract.TenantID, id contract.RequestID, expected contract.Revision, observed contract.SubjectRef) error {
	if ctx == nil {
		return fmt.Errorf("request stale reconciliation requires a context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if _, err := contract.ParseRequestID(string(id)); err != nil {
		return err
	}
	if _, err := contract.ParseRevision(string(expected)); err != nil {
		return err
	}
	if observed.Kind == "" || observed.ID == "" {
		return contract.NewError(contract.CodeValidationFailed, "observed subject identity is invalid")
	}
	if _, err := contract.ParseRevision(string(observed.Revision)); err != nil {
		return err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin stale request reconciliation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	query := `SELECT revision, status, subject_kind, subject_id, subject_revision, ` + s.dialect.NowTimestamp() + ` FROM _kora_request WHERE tenant_id = ? AND id = ?`
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	var revision int64
	var status string
	var kind, subjectID, subjectRevision sql.NullString
	var now db.Timestamp
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(id)).Scan(&revision, &status, &kind, &subjectID, &subjectRevision, &now); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contract.NewError(contract.CodeNotFound, "request not found")
		}
		return fmt.Errorf("lock request for stale reconciliation: %w", err)
	}
	expectedNumber, _ := strconv.ParseInt(string(expected), 10, 64)
	if status == string(contract.RequestStale) && expectedNumber < int64(^uint64(0)>>1) && revision == expectedNumber+1 &&
		kind.Valid && subjectID.Valid && subjectRevision.Valid && kind.String == observed.Kind && subjectID.String == observed.ID && subjectRevision.String != string(observed.Revision) {
		// A previous reconciliation committed the only transition possible from
		// this expected request revision. Treat its exact retry as success so
		// duplicate subject-change signals cannot strand workflow consumers.
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit duplicate stale request reconciliation: %w", err)
		}
		committed = true
		return nil
	}
	if strconv.FormatInt(revision, 10) != string(expected) || status != string(contract.RequestPending) {
		return contract.NewError(contract.CodeConflict, "request changed before stale reconciliation")
	}
	if !kind.Valid || !subjectID.Valid || !subjectRevision.Valid || kind.String != observed.Kind || subjectID.String != observed.ID {
		return contract.NewError(contract.CodeConflict, "observed subject does not match the request subject")
	}
	if subjectRevision.String == string(observed.Revision) {
		return contract.NewError(contract.CodeConflict, "request subject has not changed")
	}
	newRevision := revision + 1
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request SET status = ?, revision = ?, updated_at = ? WHERE tenant_id = ? AND id = ? AND revision = ? AND status = ?`),
		string(contract.RequestStale), newRevision, now.UTC(), string(tenant), string(id), revision, string(contract.RequestPending))
	if err != nil {
		return fmt.Errorf("mark request stale: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return contract.NewError(contract.CodeConflict, "request changed while marking stale")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_request_assignment SET status = ?, updated_at = ? WHERE tenant_id = ? AND request_id = ? AND status IN (?, ?)`),
		string(contract.AssignmentCancelled), now.UTC(), string(tenant), string(id), string(contract.AssignmentPending), string(contract.AssignmentBlocked)); err != nil {
		return fmt.Errorf("close stale request assignments: %w", err)
	}
	history := contract.RequestHistoryEntry{Sequence: contract.Revision(strconv.FormatInt(newRevision, 10)), Kind: contract.HistoryStale, Comment: "Subject revision changed", At: now.UTC()}
	if err := insertHistory(ctx, tx, s.dialect, tenant, id, history); err != nil {
		return fmt.Errorf("append stale request history: %w", err)
	}
	if err := s.events.Append(ctx, tx, terminalEvent(tenant, id, "request.stale", newRevision, now.Time)); err != nil {
		return fmt.Errorf("append stale request event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit stale request reconciliation: %w", err)
	}
	committed = true
	return nil
}

func terminalEvent(tenant contract.TenantID, id contract.RequestID, eventType string, revision int64, at time.Time) contract.EventEnvelope {
	return contract.EventEnvelope{ID: contract.NewEventID(), Type: eventType, Version: contract.CurrentVersion, Source: "kora.engine", Site: string(tenant), AggregateType: "request", AggregateID: string(id), OccurredAt: at.UTC(), Data: contract.MustEncodeData(struct {
		RequestID contract.RequestID     `json:"request_id"`
		Revision  contract.Revision      `json:"revision"`
		Status    contract.RequestStatus `json:"status"`
	}{RequestID: id, Revision: contract.Revision(strconv.FormatInt(revision, 10)), Status: contract.RequestStatus(eventType[len("request."):])})}
}
