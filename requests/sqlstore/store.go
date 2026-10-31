package sqlstore

import (
	"context"
	"crypto/sha256"
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
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/requests"
)

const requestCreateOperation = "request.submit"

// Store persists request data in the tenant database. It is deliberately not
// an HTTP service: callers must authorize and project records before exposing
// them. Create commits the request, assignment snapshot, history, outbox events
// and retry receipt in one SQL transaction.
type Store struct {
	database *sql.DB
	dialect  db.Dialect
	events   outbox.Writer
}

var _ requests.Store = (*Store)(nil)
var _ requests.DecisionStore = (*Store)(nil)
var _ requests.DecisionReceiptLookup = (*Store)(nil)
var _ requests.TransactionCreateStore = (*Store)(nil)

func New(database *sql.DB, dialect db.Dialect) (*Store, error) {
	if database == nil {
		return nil, fmt.Errorf("request SQL store requires a database")
	}
	if dialect == nil {
		return nil, fmt.Errorf("request SQL store requires a dialect")
	}
	if _, err := SchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &Store{database: database, dialect: dialect, events: outbox.NewSQLWriter(dialect)}, nil
}

// Create atomically records a newly authorized submission. Authorization
// snapshots are supplied by the request service; this method validates their
// binding and makes every local durable effect commit or roll back together.
func (s *Store) Create(ctx context.Context, change requests.CreateCommit) (requests.RequestRecord, error) {
	if err := validateCreateCommit(change); err != nil {
		return requests.RequestRecord{}, err
	}
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request create requires a context")
	}
	if prior, found, err := s.lookupCreateReceipt(ctx, change.Identity); found || err != nil {
		return prior, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("begin request submission: %w", err)
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
		if prior, found, lookupErr := s.lookupCreateReceipt(ctx, change.Identity); found {
			return prior, lookupErr
		} else if lookupErr != nil {
			return requests.RequestRecord{}, errors.Join(cause, lookupErr)
		}
		return requests.RequestRecord{}, cause
	}
	stored, err := s.CreateInTx(ctx, tx, change)
	if err != nil {
		return fail(err)
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Errorf("commit request submission: %w", err))
	}
	committed = true
	return stored, nil
}

// CreateInTx appends the request, assignment snapshot, history, event outbox,
// and idempotency receipt to the caller's transaction. It deliberately leaves
// transaction ownership with the caller.
func (s *Store) CreateInTx(ctx context.Context, tx *sql.Tx, change requests.CreateCommit) (requests.RequestRecord, error) {
	if s == nil || s.database == nil || s.dialect == nil || s.events == nil || tx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request create requires a configured store and caller transaction")
	}
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request create requires a context")
	}
	if err := validateCreateCommit(change); err != nil {
		return requests.RequestRecord{}, err
	}
	if prior, found, err := s.lookupCreateReceiptTx(ctx, tx, change.Identity); found || err != nil {
		return prior, err
	}
	if err := lockAndCheckAuthorizationRevision(ctx, tx, s.dialect, change.ExpectedPolicyRevision, change.ExpectedMembershipRevision); err != nil {
		return requests.RequestRecord{}, err
	}
	if err := insertRequest(ctx, tx, s.dialect, change.Request); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("insert request: %w", err)
	}
	for _, assignment := range change.Assignments {
		if err := insertAssignment(ctx, tx, s.dialect, change.Identity.Tenant, assignment, change.Request.CreatedAt); err != nil {
			return requests.RequestRecord{}, fmt.Errorf("insert request assignment: %w", err)
		}
	}
	if err := insertHistory(ctx, tx, s.dialect, change.Identity.Tenant, change.Request.ID, change.History); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("insert request history: %w", err)
	}
	for _, event := range change.Events {
		if err := s.events.Append(ctx, tx, event); err != nil {
			return requests.RequestRecord{}, fmt.Errorf("append request event %s: %w", event.Type, err)
		}
	}
	if err := insertIdempotencyReceipt(ctx, tx, s.dialect, change.Identity, change.Request); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("store request idempotency receipt: %w", err)
	}
	record := change.Request
	record.Assignments = append([]requests.AssignmentRecord(nil), change.Assignments...)
	return record, nil
}

func lockAndCheckAuthorizationRevision(ctx context.Context, tx *sql.Tx, dialect db.Dialect, policy, membership contract.Revision) error {
	if policy == "" || membership == "" {
		return contract.NewError(contract.CodeConflict, "request authorization revision is required")
	}
	if _, err := contract.ParseRevision(string(policy)); err != nil {
		return contract.NewError(contract.CodeConflict, "request policy authorization snapshot is invalid")
	}
	if _, err := contract.ParseRevision(string(membership)); err != nil {
		return contract.NewError(contract.CodeConflict, "request membership authorization snapshot is invalid")
	}
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "libsql" {
		if _, err := tx.ExecContext(ctx, `UPDATE _kora_auth_revision SET id = id WHERE id = 1`); err != nil {
			return fmt.Errorf("lock authorization revision: %w", err)
		}
	}
	query := `SELECT policy_revision, membership_revision FROM _kora_auth_revision WHERE id = 1`
	if driver, ok := dialect.(interface{ DriverName() string }); ok {
		switch driver.DriverName() {
		case "postgres":
			query += " FOR SHARE"
		case "mysql":
			query += " LOCK IN SHARE MODE"
		}
	}
	var currentPolicy, currentMembership int64
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, query)).Scan(&currentPolicy, &currentMembership); err != nil {
		return fmt.Errorf("read authorization revision: %w", err)
	}
	if strconv.FormatInt(currentPolicy, 10) != string(policy) || strconv.FormatInt(currentMembership, 10) != string(membership) {
		return contract.NewError(contract.CodeConflict, "authorization changed; refresh the request and retry")
	}
	return nil
}

// Get loads the current durable record. Visibility and field authorization are
// intentionally enforced by the request service before projection.
func (s *Store) Get(ctx context.Context, tenant contract.TenantID, id contract.RequestID) (requests.RequestRecord, error) {
	return s.get(ctx, s.database, tenant, id)
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) get(ctx context.Context, query queryer, tenant contract.TenantID, id contract.RequestID) (requests.RequestRecord, error) {
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("request lookup requires a context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParseRequestID(string(id)); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("request ID: %w", err)
	}
	var (
		record                                                 requests.RequestRecord
		revision                                               int64
		supersedes, subjectKind, subjectID, subjectRevision    sql.NullString
		dueAt, outcome, resolvedAt                             sql.NullString
		continuationID, continuationState, informationSchemaID sql.NullString
		informationSchemaVersion                               int64
		sourceInstanceID, sourceStepID                         sql.NullString
		createdAt, updatedAt                                   db.Timestamp
	)
	err := query.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT
		id, tenant_id, revision, kind, assignment_mode, status, requester_id, supersedes_request_id,
		subject_kind, subject_id, subject_revision, subject_digest, title, message,
		due_at, outcome, resolved_at, continuation_operation_id, continuation_state,
		policy_id, policy_revision, information_schema_id, information_schema_version,
		source_instance_id, source_step_id, created_at, updated_at
		FROM _kora_request WHERE tenant_id = ? AND id = ?`), string(tenant), string(id)).Scan(
		&record.ID, &record.Tenant, &revision, &record.Kind, &record.AssignmentMode, &record.Status, &record.Requester, &supersedes,
		&subjectKind, &subjectID, &subjectRevision, &record.SubjectDigest, &record.Title, &record.Message,
		&dueAt, &outcome, &resolvedAt, &continuationID, &continuationState,
		&record.PolicyID, &record.PolicyRevision, &informationSchemaID, &informationSchemaVersion,
		&sourceInstanceID, &sourceStepID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return requests.RequestRecord{}, contract.NewError(contract.CodeNotFound, "request not found")
	}
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("load request: %w", err)
	}
	if revision < 0 || informationSchemaVersion < 0 || informationSchemaVersion > int64(^uint32(0)) {
		return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request revision or schema version is invalid")
	}
	record.Revision = contract.Revision(strconv.FormatInt(revision, 10))
	if supersedes.Valid {
		id := contract.RequestID(supersedes.String)
		if _, err := contract.ParseRequestID(string(id)); err != nil {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request supersession ID is invalid")
		}
		record.SupersedesRequestID = &id
	}
	if subjectKind.Valid || subjectID.Valid || subjectRevision.Valid {
		if !subjectKind.Valid || !subjectID.Valid || !subjectRevision.Valid {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request subject is incomplete")
		}
		record.Subject = &contract.SubjectRef{Kind: subjectKind.String, ID: subjectID.String, Revision: contract.Revision(subjectRevision.String)}
	}
	if dueAt.Valid {
		parsed, err := parseDBTime(dueAt.String)
		if err != nil {
			return requests.RequestRecord{}, fmt.Errorf("parse request due time: %w", err)
		}
		record.DueAt = &parsed
	}
	if outcome.Valid {
		value := contract.RequestOutcome(outcome.String)
		record.Outcome = &value
	}
	if resolvedAt.Valid {
		parsed, err := parseDBTime(resolvedAt.String)
		if err != nil {
			return requests.RequestRecord{}, fmt.Errorf("parse request resolution time: %w", err)
		}
		record.ResolvedAt = &parsed
	}
	if continuationID.Valid && continuationID.String != "" {
		record.Continuation = &contract.ContinuationView{OperationID: contract.OperationID(continuationID.String), State: contract.OperationState(continuationState.String)}
	} else if continuationState.Valid && continuationState.String != "" {
		return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request continuation state lacks an operation ID")
	}
	if informationSchemaID.Valid && informationSchemaID.String != "" {
		if informationSchemaVersion == 0 {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored information schema lacks a version")
		}
		record.InformationSchema = &contract.SchemaRef{ID: informationSchemaID.String, Version: uint32(informationSchemaVersion)}
	} else if informationSchemaVersion != 0 {
		return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored information schema version lacks an ID")
	}
	if sourceInstanceID.Valid && sourceInstanceID.String != "" || sourceStepID.Valid && sourceStepID.String != "" {
		if !sourceInstanceID.Valid || sourceInstanceID.String == "" || !sourceStepID.Valid || sourceStepID.String == "" {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request workflow source is incomplete")
		}
		if _, err := contract.ParseInstanceID(sourceInstanceID.String); err != nil {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request workflow instance ID is invalid")
		}
		if _, err := contract.ParseStepID(sourceStepID.String); err != nil {
			return requests.RequestRecord{}, contract.NewError(contract.CodeInternal, "stored request workflow step ID is invalid")
		}
		record.Source = &contract.RequestSource{InstanceID: contract.InstanceID(sourceInstanceID.String), StepID: contract.StepID(sourceStepID.String)}
	}
	record.CreatedAt, record.UpdatedAt = createdAt.UTC(), updatedAt.UTC()
	if err := validateLoadedRecord(record); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("validate stored request: %w", err)
	}
	assignments, err := s.loadAssignmentsFrom(ctx, query, tenant, id)
	if err != nil {
		return requests.RequestRecord{}, err
	}
	record.Assignments = assignments
	return record, nil
}

func (s *Store) loadAssignments(ctx context.Context, tenant contract.TenantID, id contract.RequestID) ([]requests.AssignmentRecord, error) {
	return s.loadAssignmentsFrom(ctx, s.database, tenant, id)
}

func (s *Store) loadAssignmentsFrom(ctx context.Context, query queryer, tenant contract.TenantID, id contract.RequestID) ([]requests.AssignmentRecord, error) {
	rows, err := query.QueryContext(ctx, db.Rebind(s.dialect, `SELECT id, request_id, stage, assignee_kind, assignee_id, original_assignee_kind, original_assignee_id, status, allowed_responses, membership_revision FROM _kora_request_assignment WHERE tenant_id = ? AND request_id = ? ORDER BY stage, id`), string(tenant), string(id))
	if err != nil {
		return nil, fmt.Errorf("load request assignments: %w", err)
	}
	defer rows.Close()
	var assignments []requests.AssignmentRecord
	for rows.Next() {
		var item requests.AssignmentRecord
		var stage int64
		var kind, originalKind string
		var principal, originalID, status, rawResponses, membership string
		if err := rows.Scan(&item.ID, &item.RequestID, &stage, &kind, &principal, &originalKind, &originalID, &status, &rawResponses, &membership); err != nil {
			return nil, fmt.Errorf("scan request assignment: %w", err)
		}
		if stage < 0 || stage > int64(^uint16(0)) {
			return nil, contract.NewError(contract.CodeInternal, "stored request assignment stage is invalid")
		}
		item.Stage = uint16(stage)
		item.Principal = contract.PrincipalID(principal)
		item.OriginalTarget = contract.Assignee{Kind: contract.AssigneeKind(originalKind), ID: originalID}
		item.Status = contract.AssignmentStatus(status)
		item.MembershipRevision = contract.Revision(membership)
		if contract.AssigneeKind(kind) != contract.AssigneeUser {
			return nil, contract.NewError(contract.CodeInternal, "stored expanded assignment must target a user")
		}
		if _, err := contract.ParseAssignmentID(string(item.ID)); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored request assignment ID is invalid")
		}
		if item.RequestID != id {
			return nil, contract.NewError(contract.CodeInternal, "stored request assignment belongs to another request")
		}
		if _, err := contract.ParsePrincipalID(principal); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored request assignee is invalid")
		}
		if err := contract.ValidateAssignee(item.OriginalTarget); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored original request target is invalid")
		}
		if item.Status != contract.AssignmentPending && item.Status != contract.AssignmentBlocked && item.Status != contract.AssignmentResponded && item.Status != contract.AssignmentSuperseded && item.Status != contract.AssignmentCancelled {
			return nil, contract.NewError(contract.CodeInternal, "stored request assignment status is invalid")
		}
		if _, err := contract.ParseRevision(string(item.MembershipRevision)); err != nil {
			return nil, contract.NewError(contract.CodeInternal, "stored assignment membership revision is invalid")
		}
		if err := json.Unmarshal([]byte(rawResponses), &item.AllowedResponses); err != nil || len(item.AllowedResponses) == 0 {
			return nil, contract.NewError(contract.CodeInternal, "stored assignment response set is invalid")
		}
		seen := make(map[contract.ResponseKind]struct{}, len(item.AllowedResponses))
		for _, response := range item.AllowedResponses {
			if response != contract.ResponseApprove && response != contract.ResponseReject && response != contract.ResponseReturn && response != contract.ResponseProvide && response != contract.ResponseAcknowledge && response != contract.ResponseComplete {
				return nil, contract.NewError(contract.CodeInternal, "stored assignment response is invalid")
			}
			if _, ok := seen[response]; ok {
				return nil, contract.NewError(contract.CodeInternal, "stored assignment response is duplicated")
			}
			seen[response] = struct{}{}
		}
		assignments = append(assignments, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read request assignments: %w", err)
	}
	return assignments, nil
}

// MarkRead stores a per-principal watermark. It deliberately does not change
// request state or emit a completion event. Visibility authorization is done
// by the service before this storage operation.
func (s *Store) MarkRead(ctx context.Context, scope requests.VisibilityScope, id contract.RequestID) error {
	if ctx == nil {
		return fmt.Errorf("mark request read requires a context")
	}
	if _, err := contract.ParseTenantID(string(scope.Tenant)); err != nil {
		return fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(scope.Actor)); err != nil {
		return fmt.Errorf("request reader: %w", err)
	}
	if _, err := contract.ParseRequestID(string(id)); err != nil {
		return fmt.Errorf("request ID: %w", err)
	}
	for attempt := 0; ; attempt++ {
		err := s.markReadOnce(ctx, scope, id)
		if err == nil || !isLibSQLBusy(s.dialect, err) || attempt >= 7 {
			return err
		}
		delay := 5 * time.Millisecond * time.Duration(1<<attempt)
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) markReadOnce(ctx context.Context, scope requests.VisibilityScope, id contract.RequestID) error {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin request read receipt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sequence int64
	query := `SELECT r.revision FROM _kora_request r WHERE r.tenant_id = ? AND r.id = ?`
	args := []any{string(scope.Tenant), string(id)}
	if !scope.CanManageAll {
		query += ` AND (r.requester_id = ? OR EXISTS (SELECT 1 FROM _kora_request_assignment a WHERE a.tenant_id = r.tenant_id AND a.request_id = r.id AND a.assignee_kind = ? AND a.assignee_id = ?))`
		args = append(args, string(scope.Actor), string(contract.AssigneeUser), string(scope.Actor))
	}
	if driver, ok := s.dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "libsql" {
		query += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), args...).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.NewError(contract.CodeNotFound, "request not found")
	}
	if err != nil {
		return fmt.Errorf("read request watermark: %w", err)
	}
	if sequence < 0 {
		return contract.NewError(contract.CodeInternal, "stored request revision is invalid")
	}
	query = `INSERT INTO _kora_request_read (request_id, principal_id, history_sequence, read_at)
		VALUES (?, ?, ?, ` + s.dialect.NowTimestamp() + `) ` + readReceiptUpsert(s.dialect)
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(id), string(scope.Actor), sequence); err != nil {
		return fmt.Errorf("store request read receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit request read receipt: %w", err)
	}
	return nil
}

func isLibSQLBusy(dialect db.Dialect, err error) bool {
	driver, ok := dialect.(interface{ DriverName() string })
	if !ok || driver.DriverName() != "libsql" || err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database table is locked") || strings.Contains(message, "sqlite_busy")
}

func readReceiptUpsert(dialect db.Dialect) string {
	switch dialect.(type) {
	case *db.LibSQLDialect:
		return `ON CONFLICT(request_id, principal_id) DO UPDATE SET
			history_sequence = MAX(_kora_request_read.history_sequence, excluded.history_sequence),
			read_at = MAX(_kora_request_read.read_at, excluded.read_at)`
	case *db.PostgresDialect:
		return `ON CONFLICT(request_id, principal_id) DO UPDATE SET
			history_sequence = GREATEST(_kora_request_read.history_sequence, EXCLUDED.history_sequence),
			read_at = GREATEST(_kora_request_read.read_at, EXCLUDED.read_at)`
	default:
		return `ON DUPLICATE KEY UPDATE
			history_sequence = GREATEST(history_sequence, VALUES(history_sequence)),
			read_at = GREATEST(read_at, VALUES(read_at))`
	}
}

func validateLoadedRecord(record requests.RequestRecord) error {
	if _, err := contract.ParseTenantID(string(record.Tenant)); err != nil {
		return err
	}
	if _, err := contract.ParseRequestID(string(record.ID)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(record.Requester)); err != nil {
		return err
	}
	if _, err := contract.ParseRevision(string(record.Revision)); err != nil {
		return err
	}
	if record.Kind != contract.RequestApproval && record.Kind != contract.RequestReview && record.Kind != contract.RequestInformation && record.Kind != contract.RequestAcknowledgment && record.Kind != contract.RequestTask {
		return fmt.Errorf("unknown request kind %q", record.Kind)
	}
	if record.AssignmentMode != contract.AssignmentAny && record.AssignmentMode != contract.AssignmentAll && record.AssignmentMode != contract.AssignmentSequential {
		return fmt.Errorf("unknown request assignment mode %q", record.AssignmentMode)
	}
	if record.Status != contract.RequestPending && record.Status != contract.RequestResolved && record.Status != contract.RequestCancelled && record.Status != contract.RequestExpired && record.Status != contract.RequestStale {
		return fmt.Errorf("unknown request status %q", record.Status)
	}
	if record.Outcome != nil && *record.Outcome != contract.OutcomeApproved && *record.Outcome != contract.OutcomeRejected && *record.Outcome != contract.OutcomeReturned && *record.Outcome != contract.OutcomeProvided && *record.Outcome != contract.OutcomeAcknowledged && *record.Outcome != contract.OutcomeCompleted {
		return fmt.Errorf("unknown request outcome %q", *record.Outcome)
	}
	if record.Subject != nil {
		if strings.TrimSpace(record.Subject.Kind) == "" || strings.TrimSpace(record.Subject.ID) == "" {
			return fmt.Errorf("stored subject kind or ID is empty")
		}
		if _, err := contract.ParseRevision(string(record.Subject.Revision)); err != nil {
			return err
		}
	}
	if record.PolicyRevision != "" {
		if _, err := contract.ParseRevision(string(record.PolicyRevision)); err != nil {
			return err
		}
	}
	if record.InformationSchema != nil && (record.InformationSchema.ID == "" || record.InformationSchema.Version == 0 || record.Kind != contract.RequestInformation) {
		return fmt.Errorf("stored information schema binding is invalid")
	}
	if record.Continuation != nil {
		if _, err := contract.ParseOperationID(string(record.Continuation.OperationID)); err != nil {
			return err
		}
		switch record.Continuation.State {
		case contract.OperationRequested, contract.OperationRunning, contract.OperationSucceeded, contract.OperationFailed, contract.OperationRecoveryRequired:
		default:
			return fmt.Errorf("unknown continuation state %q", record.Continuation.State)
		}
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() {
		return fmt.Errorf("stored request timestamps are empty")
	}
	return nil
}

func parseDBTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05.999", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported SQL timestamp %q", value)
}

func validateCreateCommit(change requests.CreateCommit) error {
	identity := change.Identity
	if _, err := contract.ParseTenantID(string(identity.Tenant)); err != nil {
		return fmt.Errorf("request tenant: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(identity.Actor)); err != nil {
		return fmt.Errorf("request actor: %w", err)
	}
	if _, err := contract.ParseIdempotencyKey(string(identity.IdempotencyKey)); err != nil {
		return fmt.Errorf("request idempotency key: %w", err)
	}
	if identity.Operation != requestCreateOperation {
		return fmt.Errorf("request create operation is not registered")
	}
	if len(identity.CommandDigest) != sha256.Size*2 {
		return fmt.Errorf("request command digest must be a SHA-256 hex string")
	}
	if _, err := hex.DecodeString(identity.CommandDigest); err != nil || strings.ToLower(identity.CommandDigest) != identity.CommandDigest {
		return fmt.Errorf("request command digest must be lowercase SHA-256 hex")
	}
	record := change.Request
	if record.Tenant != identity.Tenant || record.Requester != identity.Actor {
		return fmt.Errorf("request identity does not match the authorized write identity")
	}
	if _, err := contract.ParseRequestID(string(record.ID)); err != nil {
		return fmt.Errorf("request ID: %w", err)
	}
	if record.Revision != "1" || record.Status != contract.RequestPending || record.Outcome != nil || record.ResolvedAt != nil {
		return fmt.Errorf("new request must start pending at revision 1 without an outcome")
	}
	if record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || !record.UpdatedAt.Equal(record.CreatedAt) {
		return fmt.Errorf("new request creation and update timestamps must be the same nonzero instant")
	}
	if record.Kind != contract.RequestApproval && record.Kind != contract.RequestReview && record.Kind != contract.RequestInformation && record.Kind != contract.RequestAcknowledgment && record.Kind != contract.RequestTask {
		return fmt.Errorf("request kind is invalid")
	}
	if record.AssignmentMode != contract.AssignmentAny && record.AssignmentMode != contract.AssignmentAll && record.AssignmentMode != contract.AssignmentSequential {
		return fmt.Errorf("request assignment mode is invalid")
	}
	if record.Subject != nil {
		if strings.TrimSpace(record.Subject.Kind) == "" || strings.TrimSpace(record.Subject.ID) == "" {
			return fmt.Errorf("request subject kind and ID are required")
		}
		if _, err := contract.ParseRevision(string(record.Subject.Revision)); err != nil {
			return fmt.Errorf("request subject revision: %w", err)
		}
	}
	if record.SupersedesRequestID != nil {
		if _, err := contract.ParseRequestID(string(*record.SupersedesRequestID)); err != nil {
			return fmt.Errorf("superseded request ID: %w", err)
		}
	}
	if err := contract.ValidateSubmitRequest(contract.SubmitRequest{Kind: record.Kind, Title: record.Title, Message: record.Message}); err != nil {
		return err
	}
	if record.Subject != nil && change.ExpectedSubject == nil {
		return fmt.Errorf("subject-bound request is missing its expected subject guard")
	}
	if !sameSubject(record.Subject, change.ExpectedSubject) {
		return fmt.Errorf("request subject differs from the authorized subject snapshot")
	}
	if record.Kind == contract.RequestApproval && (record.Subject == nil || record.PolicyID == "" || record.PolicyRevision == "") {
		return fmt.Errorf("governed approval requires a subject and policy revision")
	}
	if record.Kind == contract.RequestApproval && record.PolicyRevision == "" {
		return fmt.Errorf("governed approval requires a business policy revision")
	}
	if record.PolicyRevision != "" {
		if _, err := contract.ParseRevision(string(record.PolicyRevision)); err != nil {
			return fmt.Errorf("request policy revision: %w", err)
		}
	}
	if record.Kind == contract.RequestInformation && record.InformationSchema == nil {
		return fmt.Errorf("information request requires a registered response schema")
	}
	if record.InformationSchema != nil && (record.InformationSchema.ID == "" || record.InformationSchema.Version == 0) {
		return fmt.Errorf("information response schema ID and version are required")
	}
	if record.Kind != contract.RequestInformation && record.InformationSchema != nil {
		return fmt.Errorf("only information requests may declare an information response schema")
	}
	if len(change.Assignments) == 0 || len(change.Assignments) > 50 {
		return fmt.Errorf("request must have between 1 and 50 expanded assignments")
	}
	if _, err := contract.ParseRevision(string(change.ExpectedMembershipRevision)); err != nil {
		return fmt.Errorf("request membership revision: %w", err)
	}
	seen := make(map[string]struct{}, len(change.Assignments))
	for _, assignment := range change.Assignments {
		if assignment.RequestID != record.ID {
			return fmt.Errorf("assignment is bound to a different request")
		}
		if _, err := contract.ParseAssignmentID(string(assignment.ID)); err != nil {
			return fmt.Errorf("assignment ID: %w", err)
		}
		if _, err := contract.ParsePrincipalID(string(assignment.Principal)); err != nil {
			return fmt.Errorf("assignment principal: %w", err)
		}
		if err := contract.ValidateAssignee(assignment.OriginalTarget); err != nil {
			return fmt.Errorf("assignment target is invalid: %w", err)
		}
		if assignment.Status != contract.AssignmentPending && assignment.Status != contract.AssignmentBlocked {
			return fmt.Errorf("new assignment status must be pending or blocked")
		}
		if assignment.MembershipRevision != change.ExpectedMembershipRevision {
			return fmt.Errorf("assignment membership revision differs from the authorized snapshot")
		}
		if len(assignment.AllowedResponses) == 0 {
			return fmt.Errorf("assignment must have at least one allowed response")
		}
		seenResponses := make(map[contract.ResponseKind]struct{}, len(assignment.AllowedResponses))
		for _, response := range assignment.AllowedResponses {
			if response != contract.ResponseApprove && response != contract.ResponseReject && response != contract.ResponseReturn && response != contract.ResponseProvide && response != contract.ResponseAcknowledge && response != contract.ResponseComplete {
				return fmt.Errorf("assignment contains an invalid allowed response")
			}
			if _, exists := seenResponses[response]; exists {
				return fmt.Errorf("assignment contains a duplicate allowed response")
			}
			seenResponses[response] = struct{}{}
		}
		key := fmt.Sprintf("%d\x00%s", assignment.Stage, assignment.Principal)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("request has duplicate expanded assignment participants")
		}
		seen[key] = struct{}{}
	}
	if change.History.Kind != contract.HistoryCreated || change.History.Sequence != record.Revision || change.History.At.IsZero() || change.History.Actor == nil || *change.History.Actor != identity.Actor || change.History.AssignmentID != "" || change.History.Response != nil {
		return fmt.Errorf("new request history must record its creation at revision 1")
	}
	if _, err := contract.ParseRevision(string(change.History.Sequence)); err != nil {
		return fmt.Errorf("request history sequence: %w", err)
	}
	if len(change.Events) == 0 {
		return fmt.Errorf("request creation requires a durable event")
	}
	for _, event := range change.Events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("request event: %w", err)
		}
	}
	return nil
}

func sameSubject(a, b *contract.SubjectRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Kind == b.Kind && a.ID == b.ID && a.Revision == b.Revision
}

func insertRequest(ctx context.Context, tx *sql.Tx, dialect db.Dialect, record requests.RequestRecord) error {
	var subjectKind, subjectID, subjectRevision, supersedes any
	if record.Subject != nil {
		subjectKind, subjectID, subjectRevision = record.Subject.Kind, record.Subject.ID, string(record.Subject.Revision)
	}
	if record.SupersedesRequestID != nil {
		supersedes = string(*record.SupersedesRequestID)
	}
	var dueAt, outcome, resolvedAt any
	if record.DueAt != nil {
		dueAt = record.DueAt.UTC()
	}
	if record.Outcome != nil {
		outcome = string(*record.Outcome)
	}
	if record.ResolvedAt != nil {
		resolvedAt = record.ResolvedAt.UTC()
	}
	var informationSchemaID string
	var informationSchemaVersion uint32
	if record.InformationSchema != nil {
		informationSchemaID = record.InformationSchema.ID
		informationSchemaVersion = record.InformationSchema.Version
	}
	var sourceInstanceID, sourceStepID string
	if record.Source != nil {
		sourceInstanceID, sourceStepID = string(record.Source.InstanceID), string(record.Source.StepID)
	}
	var continuationOperationID, continuationState string
	if record.Continuation != nil {
		continuationOperationID, continuationState = string(record.Continuation.OperationID), string(record.Continuation.State)
	}
	_, err := tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_request
		(id, tenant_id, revision, kind, assignment_mode, status, requester_id, supersedes_request_id, subject_kind, subject_id, subject_revision, subject_digest,
		 title, message, due_at, outcome, resolved_at, continuation_operation_id, continuation_state, policy_id, policy_revision,
		 information_schema_id, information_schema_version, source_instance_id, source_step_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		string(record.ID), string(record.Tenant), int64(1), string(record.Kind), string(record.AssignmentMode), string(record.Status), string(record.Requester), supersedes,
		subjectKind, subjectID, subjectRevision, record.SubjectDigest, record.Title, record.Message, dueAt, outcome, resolvedAt,
		continuationOperationID, continuationState, record.PolicyID, string(record.PolicyRevision),
		informationSchemaID, int64(informationSchemaVersion), sourceInstanceID, sourceStepID, record.CreatedAt.UTC(), record.UpdatedAt.UTC())
	return err
}

func insertAssignment(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant contract.TenantID, record requests.AssignmentRecord, at time.Time) error {
	allowed, err := json.Marshal(record.AllowedResponses)
	if err != nil {
		return fmt.Errorf("encode assignment responses: %w", err)
	}
	_, err = tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_request_assignment
		(id, tenant_id, request_id, stage, assignee_kind, assignee_id, original_assignee_kind, original_assignee_id,
		 status, allowed_responses, membership_revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		string(record.ID), string(tenant), string(record.RequestID), int64(record.Stage), string(contract.AssigneeUser), string(record.Principal),
		string(record.OriginalTarget.Kind), record.OriginalTarget.ID, string(record.Status), string(allowed), string(record.MembershipRevision), at.UTC(), at.UTC())
	return err
}

func insertHistory(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant contract.TenantID, requestID contract.RequestID, history contract.RequestHistoryEntry) error {
	sequence, err := strconv.ParseInt(string(history.Sequence), 10, 64)
	if err != nil || sequence < 1 {
		return fmt.Errorf("history sequence is invalid")
	}
	var actor, assignmentID, response, previousAssignee, newAssignee any
	if history.Actor != nil {
		if _, err := contract.ParsePrincipalID(string(*history.Actor)); err != nil {
			return fmt.Errorf("history actor is invalid: %w", err)
		}
		actor = string(*history.Actor)
	}
	if history.AssignmentID != "" {
		if _, err := contract.ParseAssignmentID(string(history.AssignmentID)); err != nil {
			return fmt.Errorf("history assignment ID is invalid: %w", err)
		}
		assignmentID = string(history.AssignmentID)
	}
	if history.Response != nil {
		response = string(*history.Response)
	}
	if (history.PreviousAssignee == nil) != (history.NewAssignee == nil) {
		return fmt.Errorf("reassignment history must include both assignees")
	}
	if history.Kind == contract.HistoryReassigned && (history.PreviousAssignee == nil || history.NewAssignee == nil || history.AssignmentID == "") {
		return fmt.Errorf("reassignment history lacks transfer details")
	}
	if history.Kind != contract.HistoryReassigned && history.PreviousAssignee != nil {
		return fmt.Errorf("only reassignment history can include transfer details")
	}
	if history.PreviousAssignee != nil {
		if history.PreviousAssignee.Kind != contract.AssigneeUser || history.NewAssignee.Kind != contract.AssigneeUser {
			return fmt.Errorf("reassignment history must record expanded users")
		}
		if _, err := contract.ParsePrincipalID(history.PreviousAssignee.ID); err != nil {
			return fmt.Errorf("previous assignee is invalid: %w", err)
		}
		if _, err := contract.ParsePrincipalID(history.NewAssignee.ID); err != nil {
			return fmt.Errorf("new assignee is invalid: %w", err)
		}
		previousJSON, err := json.Marshal(history.PreviousAssignee)
		if err != nil {
			return fmt.Errorf("encode previous assignee: %w", err)
		}
		previousAssignee = string(previousJSON)
		newJSON, err := json.Marshal(history.NewAssignee)
		if err != nil {
			return fmt.Errorf("encode new assignee: %w", err)
		}
		newAssignee = string(newJSON)
	}
	_, err = tx.ExecContext(ctx, db.Rebind(dialect, `INSERT INTO _kora_request_history
		(tenant_id, request_id, sequence, kind, actor_id, assignment_id, response, comment, previous_assignee_json, new_assignee_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		string(tenant), string(requestID), sequence, string(history.Kind), actor, assignmentID, response, history.Comment, previousAssignee, newAssignee, history.At.UTC())
	return err
}

func insertIdempotencyReceipt(ctx context.Context, tx *sql.Tx, dialect db.Dialect, identity requests.WriteIdentity, record requests.RequestRecord) error {
	receipt, err := json.Marshal(createReceipt{RequestID: record.ID})
	if err != nil {
		return fmt.Errorf("encode request receipt: %w", err)
	}
	query := `INSERT INTO _kora_request_idempotency
		(tenant_id, actor_id, operation, idempotency_key, command_digest, receipt_json, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ` + idempotencyExpiryExpression(dialect) + `)`
	_, err = tx.ExecContext(ctx, db.Rebind(dialect, query),
		string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey), identity.CommandDigest,
		string(receipt), record.CreatedAt.UTC())
	return err
}

type createReceipt struct {
	RequestID contract.RequestID `json:"request_id"`
}

func idempotencyExpiryExpression(dialect db.Dialect) string {
	switch dialect.(type) {
	case *db.LibSQLDialect:
		return "datetime('now', '+30 days')"
	case *db.PostgresDialect:
		return "NOW() + INTERVAL '30 days'"
	default:
		return "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL 30 DAY)"
	}
}

func (s *Store) lookupCreateReceipt(ctx context.Context, identity requests.WriteIdentity) (requests.RequestRecord, bool, error) {
	var digest, raw string
	var active bool
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest, receipt_json, (expires_at > `+s.dialect.NowTimestamp()+`) FROM _kora_request_idempotency
		WHERE tenant_id = ? AND actor_id = ? AND operation = ? AND idempotency_key = ?`),
		string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey)).Scan(&digest, &raw, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return requests.RequestRecord{}, false, nil
	}
	if err != nil {
		return requests.RequestRecord{}, false, fmt.Errorf("read request idempotency receipt: %w", err)
	}
	if !active {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeConflict, "request retry window expired; query the request outcome instead")
	}
	if digest != identity.CommandDigest {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key was already used for a different request")
	}
	var receipt createReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return requests.RequestRecord{}, true, fmt.Errorf("decode request idempotency receipt: %w", err)
	}
	if _, err := contract.ParseRequestID(string(receipt.RequestID)); err != nil {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeInternal, "stored request receipt ID failed validation")
	}
	record, err := s.Get(ctx, identity.Tenant, receipt.RequestID)
	if err != nil {
		return requests.RequestRecord{}, true, err
	}
	if record.Tenant != identity.Tenant || record.Requester != identity.Actor {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeInternal, "stored request receipt identity failed validation")
	}
	return record, true, nil
}

func (s *Store) lookupCreateReceiptTx(ctx context.Context, tx *sql.Tx, identity requests.WriteIdentity) (requests.RequestRecord, bool, error) {
	query := `SELECT command_digest, receipt_json, (expires_at > ` + s.dialect.NowTimestamp() + `) FROM _kora_request_idempotency WHERE tenant_id = ? AND actor_id = ? AND operation = ? AND idempotency_key = ?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	var digest, raw string
	var active bool
	err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(identity.Tenant), string(identity.Actor), identity.Operation, string(identity.IdempotencyKey)).Scan(&digest, &raw, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return requests.RequestRecord{}, false, nil
	}
	if err != nil {
		return requests.RequestRecord{}, false, fmt.Errorf("read request idempotency receipt in transaction: %w", err)
	}
	if !active {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeConflict, "request retry window expired; query the request outcome instead")
	}
	if digest != identity.CommandDigest {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeIdempotencyKeyReused, "idempotency key was already used for a different request")
	}
	var receipt createReceipt
	if err := json.Unmarshal([]byte(raw), &receipt); err != nil {
		return requests.RequestRecord{}, true, fmt.Errorf("decode request idempotency receipt: %w", err)
	}
	if _, err := contract.ParseRequestID(string(receipt.RequestID)); err != nil {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeInternal, "stored request receipt ID failed validation")
	}
	record, err := s.get(ctx, tx, identity.Tenant, receipt.RequestID)
	if err != nil {
		return requests.RequestRecord{}, true, err
	}
	if record.Tenant != identity.Tenant || record.Requester != identity.Actor {
		return requests.RequestRecord{}, true, contract.NewError(contract.CodeInternal, "stored request receipt identity failed validation")
	}
	return record, true, nil
}
