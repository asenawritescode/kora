package ai

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"
)

// ConversationRecord is the durable server-side chat conversation state.
type ConversationRecord struct {
	ID                 string
	Site               string
	Channel            string
	SubjectKey         string
	Title              string
	Summary            string
	Status             string
	LastRunID          string
	LastMessageAt      time.Time
	RetentionExpiresAt time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// RunRecord is the durable state of one AI run.
type RunRecord struct {
	ID                 string
	Site               string
	ConversationID     string
	Channel            string
	Status             string
	Model              string
	Provider           string
	CurrentStepID      string
	Summary            string
	InputMessage       string
	OutputMessage      string
	ErrorMessage       string
	CancelReason       string
	ResumeToken        string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CompletedAt        time.Time
	CancelledAt        time.Time
	RetentionExpiresAt time.Time
}

type ApprovalRecord struct {
	ID                 string
	Site               string
	OperationID        string
	ActorPrincipalID   string
	ActorPrincipalType string
	ToolName           string
	State              string
	TargetFingerprint  string
	ArgumentHash       string
	RecordVersion      int
	RequestedAt        time.Time
	ExpiresAt          time.Time
	GrantedAt          time.Time
	GrantedBy          string
	AuthSessionID      string
}

// TaskRecord is one tracked unit of work inside an AI run.
type TaskRecord struct {
	ID             string
	Site           string
	RunID          string
	ConversationID string
	ParentTaskID   string
	Kind           string
	Title          string
	Description    string
	Status         string
	SortOrder      int
	Notes          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func EnsureAIRunTables(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS _kora_ai_conversation (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			channel VARCHAR(255) NOT NULL DEFAULT 'chat',
			subject_key VARCHAR(255) NOT NULL DEFAULT '',
			title VARCHAR(255) NOT NULL DEFAULT '',
			summary VARCHAR(255) NOT NULL DEFAULT '',
			status VARCHAR(255) NOT NULL DEFAULT 'active',
			last_run_id VARCHAR(255) NOT NULL DEFAULT '',
			last_message_at DATETIME,
			retention_expires_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_ai_conversation_site ON _kora_ai_conversation (site, updated_at)`,
		`CREATE INDEX idx_ai_conversation_subject ON _kora_ai_conversation (site, subject_key)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_run (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			conversation_id VARCHAR(255) NOT NULL DEFAULT '',
			channel VARCHAR(255) NOT NULL DEFAULT 'chat',
			status VARCHAR(255) NOT NULL DEFAULT 'planning',
			model VARCHAR(255) NOT NULL DEFAULT '',
			provider VARCHAR(255) NOT NULL DEFAULT '',
			current_step_id VARCHAR(255) NOT NULL DEFAULT '',
			summary VARCHAR(255) NOT NULL DEFAULT '',
			input_message LONGTEXT NOT NULL,
			output_message LONGTEXT NOT NULL,
			error_message VARCHAR(255) NOT NULL DEFAULT '',
			cancel_reason VARCHAR(255) NOT NULL DEFAULT '',
			resume_token VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME,
			cancelled_at DATETIME,
			retention_expires_at DATETIME
		)`,
		`CREATE INDEX idx_ai_run_site ON _kora_ai_run (site, updated_at)`,
		`CREATE INDEX idx_ai_run_conversation ON _kora_ai_run (conversation_id, updated_at)`,
		`CREATE INDEX idx_ai_run_status ON _kora_ai_run (site, status)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_message (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			conversation_id VARCHAR(255) NOT NULL DEFAULT '',
			run_id VARCHAR(255) NOT NULL DEFAULT '',
			role VARCHAR(255) NOT NULL DEFAULT '',
			content LONGTEXT NOT NULL,
			message_kind VARCHAR(255) NOT NULL DEFAULT 'message',
			step_id VARCHAR(255) NOT NULL DEFAULT '',
			sequence INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_ai_message_conversation ON _kora_ai_message (conversation_id, sequence)`,
		`CREATE INDEX idx_ai_message_run ON _kora_ai_message (run_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_step (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			run_id VARCHAR(255) NOT NULL DEFAULT '',
			conversation_id VARCHAR(255) NOT NULL DEFAULT '',
			step_key VARCHAR(255) NOT NULL DEFAULT '',
			status VARCHAR(255) NOT NULL DEFAULT 'planning',
			summary VARCHAR(255) NOT NULL DEFAULT '',
			tool_name VARCHAR(255) NOT NULL DEFAULT '',
			input_json LONGTEXT NOT NULL,
			output_json LONGTEXT NOT NULL,
			error_message VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_ai_step_run ON _kora_ai_step (run_id, created_at)`,
		`CREATE INDEX idx_ai_step_conversation ON _kora_ai_step (conversation_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_task (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			run_id VARCHAR(255) NOT NULL DEFAULT '',
			conversation_id VARCHAR(255) NOT NULL DEFAULT '',
			parent_task_id VARCHAR(255) NOT NULL DEFAULT '',
			kind VARCHAR(255) NOT NULL DEFAULT 'task',
			title VARCHAR(255) NOT NULL DEFAULT '',
			description VARCHAR(255) NOT NULL DEFAULT '',
			status VARCHAR(255) NOT NULL DEFAULT 'queued',
			sort_order INTEGER NOT NULL DEFAULT 0,
			notes VARCHAR(255) NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME
		)`,
		`CREATE INDEX idx_ai_task_run ON _kora_ai_task (run_id, status, sort_order)`,
		`CREATE INDEX idx_ai_task_conversation ON _kora_ai_task (conversation_id, updated_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_approval (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			operation_id VARCHAR(255) NOT NULL DEFAULT '',
			actor_principal_id VARCHAR(255) NOT NULL DEFAULT '',
			actor_principal_type VARCHAR(255) NOT NULL DEFAULT '',
			tool_name VARCHAR(255) NOT NULL DEFAULT '',
			state VARCHAR(255) NOT NULL DEFAULT 'pending_approval',
			target_fingerprint VARCHAR(255) NOT NULL DEFAULT '',
			argument_hash VARCHAR(255) NOT NULL DEFAULT '',
			record_version INTEGER NOT NULL DEFAULT 0,
			requested_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at DATETIME,
			granted_at DATETIME,
			granted_by VARCHAR(255) NOT NULL DEFAULT '',
			auth_session_id VARCHAR(255) NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX idx_ai_approval_site ON _kora_ai_approval (site, requested_at)`,
		`CREATE INDEX idx_ai_approval_operation ON _kora_ai_approval (operation_id, state)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_audit (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			run_id VARCHAR(255) NOT NULL DEFAULT '',
			step_id VARCHAR(255) NOT NULL DEFAULT '',
			conversation_id VARCHAR(255) NOT NULL DEFAULT '',
			kind VARCHAR(255) NOT NULL DEFAULT '',
			name VARCHAR(255) NOT NULL DEFAULT '',
			status VARCHAR(255) NOT NULL DEFAULT '',
			user_id VARCHAR(255) NOT NULL DEFAULT '',
			session_id VARCHAR(255) NOT NULL DEFAULT '',
			correlation_id VARCHAR(255) NOT NULL DEFAULT '',
			idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
			details LONGTEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_ai_audit_site_run ON _kora_ai_audit (site, run_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_usage (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			organization_id VARCHAR(255) NOT NULL DEFAULT '',
			user_id VARCHAR(255) NOT NULL DEFAULT '',
			model VARCHAR(255) NOT NULL DEFAULT '',
			provider VARCHAR(255) NOT NULL DEFAULT '',
			run_id VARCHAR(255) NOT NULL DEFAULT '',
			step_id VARCHAR(255) NOT NULL DEFAULT '',
			channel VARCHAR(255) NOT NULL DEFAULT '',
			attempt INTEGER NOT NULL DEFAULT 1,
			status VARCHAR(255) NOT NULL DEFAULT 'completed',
			tokens LONGTEXT,
			latency_ms INTEGER NOT NULL DEFAULT 0,
			occurred_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			retry_of VARCHAR(255) NOT NULL DEFAULT '',
			attribution LONGTEXT
		)`,
		`CREATE INDEX idx_ai_usage_site ON _kora_ai_usage (site, occurred_at)`,
		`CREATE INDEX idx_ai_usage_run ON _kora_ai_usage (run_id)`,
		`CREATE INDEX idx_ai_usage_step ON _kora_ai_usage (step_id)`,
		`CREATE TABLE IF NOT EXISTS _kora_ai_budget_reservation (
			id VARCHAR(255) PRIMARY KEY,
			site VARCHAR(255) NOT NULL DEFAULT '',
			model VARCHAR(255) NOT NULL DEFAULT '',
			requested_tokens INTEGER NOT NULL DEFAULT 0,
			consumed_tokens INTEGER NOT NULL DEFAULT 0,
			status VARCHAR(255) NOT NULL DEFAULT 'reserved',
			note VARCHAR(255) NOT NULL DEFAULT '',
			reserved_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			released_at DATETIME
		)`,
		`CREATE INDEX idx_ai_budget_res_site ON _kora_ai_budget_reservation (site, model, status)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !isDuplicateIndexError(err) {
			return err
		}
	}
	return nil
}

func isDuplicateIndexError(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1061
}

var aiExcludedColumn = regexp.MustCompile(`excluded\.([a-zA-Z0-9_]+)`)

// aiUpsertSQL keeps the store compatible with both Kora database dialects.
// SQLite uses ON CONFLICT/excluded; MySQL uses ON DUPLICATE KEY/VALUES.
func aiUpsertSQL(query string) string {
	if !strings.EqualFold(os.Getenv("KORA_DB_TYPE"), "mysql") {
		return query
	}
	query = strings.Replace(query, "ON CONFLICT(id) DO UPDATE SET", "ON DUPLICATE KEY UPDATE", 1)
	return aiExcludedColumn.ReplaceAllString(query, "VALUES($1)")
}

func UpsertConversation(ctx context.Context, db *sql.DB, rec ConversationRecord) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	if rec.Channel == "" {
		rec.Channel = "chat"
	}
	_, err := db.ExecContext(ctx, aiUpsertSQL(`
INSERT INTO _kora_ai_conversation (
	id, site, channel, subject_key, title, summary, status, last_run_id, last_message_at, retention_expires_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	site=excluded.site,
	channel=excluded.channel,
	subject_key=excluded.subject_key,
	title=excluded.title,
	summary=excluded.summary,
	status=excluded.status,
	last_run_id=excluded.last_run_id,
	last_message_at=excluded.last_message_at,
	retention_expires_at=excluded.retention_expires_at,
	updated_at=excluded.updated_at`),
		rec.ID, rec.Site, rec.Channel, rec.SubjectKey, rec.Title, rec.Summary, rec.Status, rec.LastRunID, nullTime(rec.LastMessageAt), nullTime(rec.RetentionExpiresAt), now, now,
	)
	return err
}

func LoadConversation(ctx context.Context, db *sql.DB, site, subjectKey string) (ConversationRecord, error) {
	if db == nil {
		return ConversationRecord{}, fmt.Errorf("conversation store unavailable")
	}
	var rec ConversationRecord
	err := db.QueryRowContext(ctx, `
SELECT id, site, channel, subject_key, title, summary, status, last_run_id, last_message_at, retention_expires_at, created_at, updated_at
FROM _kora_ai_conversation
WHERE site = ? AND subject_key = ?
ORDER BY updated_at DESC
LIMIT 1`, site, subjectKey).Scan(
		&rec.ID, &rec.Site, &rec.Channel, &rec.SubjectKey, &rec.Title, &rec.Summary, &rec.Status, &rec.LastRunID,
		&rec.LastMessageAt, &rec.RetentionExpiresAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return ConversationRecord{}, err
	}
	return rec, nil
}

func LoadConversationByID(ctx context.Context, db *sql.DB, conversationID string) (ConversationRecord, error) {
	if db == nil {
		return ConversationRecord{}, fmt.Errorf("conversation store unavailable")
	}
	var rec ConversationRecord
	err := db.QueryRowContext(ctx, `
SELECT id, site, channel, subject_key, title, summary, status, last_run_id, last_message_at, retention_expires_at, created_at, updated_at
FROM _kora_ai_conversation
WHERE id = ?`, conversationID).Scan(
		&rec.ID, &rec.Site, &rec.Channel, &rec.SubjectKey, &rec.Title, &rec.Summary, &rec.Status, &rec.LastRunID,
		&rec.LastMessageAt, &rec.RetentionExpiresAt, &rec.CreatedAt, &rec.UpdatedAt,
	)
	if err != nil {
		return ConversationRecord{}, err
	}
	return rec, nil
}

func UpsertRun(ctx context.Context, db *sql.DB, rec RunRecord) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	if rec.Channel == "" {
		rec.Channel = "chat"
	}
	_, err := db.ExecContext(ctx, aiUpsertSQL(`
INSERT INTO _kora_ai_run (
	id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	site=excluded.site,
	conversation_id=excluded.conversation_id,
	channel=excluded.channel,
	status=excluded.status,
	model=excluded.model,
	provider=excluded.provider,
	current_step_id=excluded.current_step_id,
	summary=excluded.summary,
	input_message=excluded.input_message,
	output_message=excluded.output_message,
	error_message=excluded.error_message,
	cancel_reason=excluded.cancel_reason,
	resume_token=excluded.resume_token,
	updated_at=excluded.updated_at,
	completed_at=excluded.completed_at,
	cancelled_at=excluded.cancelled_at,
	retention_expires_at=excluded.retention_expires_at`),
		rec.ID, rec.Site, rec.ConversationID, rec.Channel, rec.Status, rec.Model, rec.Provider, rec.CurrentStepID, rec.Summary, rec.InputMessage, rec.OutputMessage, rec.ErrorMessage, rec.CancelReason, rec.ResumeToken, now, now, nullTime(rec.CompletedAt), nullTime(rec.CancelledAt), nullTime(rec.RetentionExpiresAt),
	)
	return err
}

func LoadRun(ctx context.Context, db *sql.DB, runID string) (RunRecord, error) {
	if db == nil {
		return RunRecord{}, fmt.Errorf("run store unavailable")
	}
	var rec RunRecord
	err := db.QueryRowContext(ctx, `
SELECT id, site, conversation_id, channel, status, model, provider, current_step_id, summary, input_message, output_message, error_message, cancel_reason, resume_token, created_at, updated_at, completed_at, cancelled_at, retention_expires_at
FROM _kora_ai_run
WHERE id = ?`, runID).Scan(
		&rec.ID, &rec.Site, &rec.ConversationID, &rec.Channel, &rec.Status, &rec.Model, &rec.Provider, &rec.CurrentStepID, &rec.Summary, &rec.InputMessage, &rec.OutputMessage, &rec.ErrorMessage, &rec.CancelReason, &rec.ResumeToken, &rec.CreatedAt, &rec.UpdatedAt, &rec.CompletedAt, &rec.CancelledAt, &rec.RetentionExpiresAt,
	)
	if err != nil {
		return RunRecord{}, err
	}
	return rec, nil
}

func AppendMessage(ctx context.Context, db *sql.DB, site, conversationID, runID, role, content, kind, stepID string, sequence int) error {
	if db == nil {
		return nil
	}
	if kind == "" {
		kind = "message"
	}
	if sequence <= 0 {
		sequence = 1
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO _kora_ai_message (id, site, conversation_id, run_id, role, content, message_kind, step_id, sequence, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ulid.Make().String(), site, conversationID, runID, role, content, kind, stepID, sequence, time.Now().UTC(),
	)
	return err
}

func UpsertStep(ctx context.Context, db *sql.DB, stepID, site, runID, conversationID, stepKey, status, summary, toolName, inputJSON, outputJSON, errorMessage string) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC()
	if stepID == "" {
		stepID = ulid.Make().String()
	}
	_, err := db.ExecContext(ctx, aiUpsertSQL(`
INSERT INTO _kora_ai_step (
	id, site, run_id, conversation_id, step_key, status, summary, tool_name, input_json, output_json, error_message, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	site=excluded.site,
	run_id=excluded.run_id,
	conversation_id=excluded.conversation_id,
	step_key=excluded.step_key,
	status=excluded.status,
	summary=excluded.summary,
	tool_name=excluded.tool_name,
	input_json=excluded.input_json,
	output_json=excluded.output_json,
	error_message=excluded.error_message,
	updated_at=excluded.updated_at`),
		stepID, site, runID, conversationID, stepKey, status, summary, toolName, inputJSON, outputJSON, errorMessage, now, now,
	)
	return err
}

func UpsertTask(ctx context.Context, db *sql.DB, rec TaskRecord) error {
	if db == nil {
		return nil
	}
	now := time.Now().UTC()
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	_, err := db.ExecContext(ctx, aiUpsertSQL(`
INSERT INTO _kora_ai_task (
	id, site, run_id, conversation_id, parent_task_id, kind, title, description, status, sort_order, notes, created_at, updated_at, completed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	site=excluded.site,
	run_id=excluded.run_id,
	conversation_id=excluded.conversation_id,
	parent_task_id=excluded.parent_task_id,
	kind=excluded.kind,
	title=excluded.title,
	description=excluded.description,
	status=excluded.status,
	sort_order=excluded.sort_order,
	notes=excluded.notes,
	updated_at=excluded.updated_at,
	completed_at=excluded.completed_at`),
		rec.ID, rec.Site, rec.RunID, rec.ConversationID, rec.ParentTaskID, rec.Kind, rec.Title, rec.Description, rec.Status, rec.SortOrder, rec.Notes, now, now, nullTime(rec.CompletedAt),
	)
	return err
}

func ListRunTasks(ctx context.Context, db *sql.DB, runID string) ([]TaskRecord, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, site, run_id, conversation_id, parent_task_id, kind, title, description, status, sort_order, notes, created_at, updated_at, completed_at
FROM _kora_ai_task
WHERE run_id = ?
ORDER BY sort_order ASC, created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskRecord
	for rows.Next() {
		var rec TaskRecord
		var completed sql.NullTime
		if err := rows.Scan(&rec.ID, &rec.Site, &rec.RunID, &rec.ConversationID, &rec.ParentTaskID, &rec.Kind, &rec.Title, &rec.Description, &rec.Status, &rec.SortOrder, &rec.Notes, &rec.CreatedAt, &rec.UpdatedAt, &completed); err != nil {
			return nil, err
		}
		if completed.Valid {
			rec.CompletedAt = completed.Time
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func MarkTaskStatus(ctx context.Context, db *sql.DB, taskID, status, notes string) error {
	if db == nil || taskID == "" {
		return nil
	}
	now := time.Now().UTC()
	var completed any
	if strings.EqualFold(status, "done") || strings.EqualFold(status, "completed") {
		completed = now
	}
	_, err := db.ExecContext(ctx, `
UPDATE _kora_ai_task
SET status = COALESCE(NULLIF(?, ''), status),
	notes = COALESCE(NULLIF(?, ''), notes),
	updated_at = ?,
	completed_at = COALESCE(?, completed_at)
WHERE id = ?`, status, notes, now, completed, taskID)
	return err
}

func QueueFollowUpTasks(ctx context.Context, db *sql.DB, site, runID, conversationID string, tasks []TaskRecord) error {
	for i := range tasks {
		tasks[i].Site = site
		tasks[i].RunID = runID
		tasks[i].ConversationID = conversationID
		if tasks[i].Status == "" {
			tasks[i].Status = "queued"
		}
		if tasks[i].Kind == "" {
			tasks[i].Kind = "task"
		}
		if err := UpsertTask(ctx, db, tasks[i]); err != nil {
			return err
		}
	}
	return nil
}

func UpdateStepStatus(ctx context.Context, db *sql.DB, stepID, status, summary, toolName, inputJSON, outputJSON, errorMessage string) error {
	if db == nil || stepID == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `
UPDATE _kora_ai_step
SET status = COALESCE(NULLIF(?, ''), status),
	summary = COALESCE(NULLIF(?, ''), summary),
	tool_name = COALESCE(NULLIF(?, ''), tool_name),
	input_json = COALESCE(NULLIF(?, ''), input_json),
	output_json = COALESCE(NULLIF(?, ''), output_json),
	error_message = COALESCE(NULLIF(?, ''), error_message),
	updated_at = ?
WHERE id = ?`,
		status, summary, toolName, inputJSON, outputJSON, errorMessage, time.Now().UTC(), stepID,
	)
	return err
}

func ListRunSteps(ctx context.Context, db *sql.DB, runID string) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
SELECT COALESCE(summary, '')
FROM _kora_ai_step
WHERE run_id = ?
ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var summary string
		if err := rows.Scan(&summary); err != nil {
			return nil, err
		}
		if summary != "" {
			out = append(out, summary)
		}
	}
	return out, rows.Err()
}

func SummarizeRun(ctx context.Context, db *sql.DB, runID string) error {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return err
	}
	summaries, err := ListRunSteps(ctx, db, runID)
	if err != nil {
		return err
	}
	summary := rec.Summary
	if len(summaries) > 0 {
		summary = strings.Join(summaries, " | ")
	}
	if summary == "" {
		summary = rec.OutputMessage
	}
	if summary == "" {
		summary = rec.InputMessage
	}
	rec.Summary = summary
	return UpsertRun(ctx, db, rec)
}

func UpsertApproval(ctx context.Context, db *sql.DB, rec ApprovalRecord) error {
	if db == nil {
		return nil
	}
	if rec.ID == "" {
		rec.ID = ulid.Make().String()
	}
	now := time.Now().UTC()
	if rec.RequestedAt.IsZero() {
		rec.RequestedAt = now
	}
	_, err := db.ExecContext(ctx, aiUpsertSQL(`
INSERT INTO _kora_ai_approval (
	id, site, operation_id, actor_principal_id, actor_principal_type, tool_name, state, target_fingerprint, argument_hash, record_version, requested_at, expires_at, granted_at, granted_by, auth_session_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	site=excluded.site,
	operation_id=excluded.operation_id,
	actor_principal_id=excluded.actor_principal_id,
	actor_principal_type=excluded.actor_principal_type,
	tool_name=excluded.tool_name,
	state=excluded.state,
	target_fingerprint=excluded.target_fingerprint,
	argument_hash=excluded.argument_hash,
	record_version=excluded.record_version,
	requested_at=excluded.requested_at,
	expires_at=excluded.expires_at,
	granted_at=excluded.granted_at,
	granted_by=excluded.granted_by,
	auth_session_id=excluded.auth_session_id`),
		rec.ID, rec.Site, rec.OperationID, rec.ActorPrincipalID, rec.ActorPrincipalType, rec.ToolName, rec.State, rec.TargetFingerprint, rec.ArgumentHash, rec.RecordVersion, rec.RequestedAt, nullTime(rec.ExpiresAt), nullTime(rec.GrantedAt), rec.GrantedBy, rec.AuthSessionID,
	)
	return err
}

func LoadApproval(ctx context.Context, db *sql.DB, approvalID string) (ApprovalRecord, error) {
	if db == nil {
		return ApprovalRecord{}, fmt.Errorf("approval store unavailable")
	}
	var rec ApprovalRecord
	var expiresAt, grantedAt sql.NullTime
	err := db.QueryRowContext(ctx, `
SELECT id, site, operation_id, actor_principal_id, actor_principal_type, tool_name, state, target_fingerprint, argument_hash, record_version, requested_at, expires_at, granted_at, granted_by, auth_session_id
FROM _kora_ai_approval
WHERE id = ?`, approvalID).Scan(
		&rec.ID, &rec.Site, &rec.OperationID, &rec.ActorPrincipalID, &rec.ActorPrincipalType, &rec.ToolName, &rec.State, &rec.TargetFingerprint, &rec.ArgumentHash, &rec.RecordVersion, &rec.RequestedAt, &expiresAt, &grantedAt, &rec.GrantedBy, &rec.AuthSessionID,
	)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if expiresAt.Valid {
		rec.ExpiresAt = expiresAt.Time
	}
	if grantedAt.Valid {
		rec.GrantedAt = grantedAt.Time
	}
	return rec, nil
}

func approvalFingerprint(toolName string, args map[string]any) string {
	h := sha256.New()
	_, _ = h.Write([]byte(toolName))
	_, _ = h.Write([]byte("|"))
	b, _ := json.Marshal(args)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

func EnsureApprovalPending(ctx context.Context, db *sql.DB, site, operationID, actorID, actorType, toolName string, recordVersion int, args map[string]any) error {
	return UpsertApproval(ctx, db, ApprovalRecord{
		Site:               site,
		OperationID:        operationID,
		ActorPrincipalID:   actorID,
		ActorPrincipalType: actorType,
		ToolName:           toolName,
		State:              "pending_approval",
		TargetFingerprint:  approvalFingerprint(toolName, args),
		ArgumentHash:       approvalFingerprint(toolName, args),
		RecordVersion:      recordVersion,
		RequestedAt:        time.Now().UTC(),
	})
}

func HasGrantedApproval(ctx context.Context, db *sql.DB, site, operationID, toolName string, args map[string]any) (bool, error) {
	if db == nil {
		return false, nil
	}
	fp := approvalFingerprint(toolName, args)
	var count int
	err := db.QueryRowContext(ctx, `
SELECT COUNT(1)
FROM _kora_ai_approval
WHERE site = ? AND operation_id = ? AND tool_name = ? AND state = 'granted' AND target_fingerprint = ?`,
		site, operationID, toolName, fp).Scan(&count)
	return count > 0, err
}

func GrantApproval(ctx context.Context, db *sql.DB, approvalID, grantedBy string) (ApprovalRecord, error) {
	rec, err := LoadApproval(ctx, db, approvalID)
	if err != nil {
		return ApprovalRecord{}, err
	}
	if !strings.EqualFold(rec.State, "pending_approval") {
		return ApprovalRecord{}, fmt.Errorf("approval is not pending")
	}
	now := time.Now().UTC()
	rec.State = "granted"
	rec.GrantedAt = now
	rec.GrantedBy = grantedBy
	if err := UpsertApproval(ctx, db, rec); err != nil {
		return ApprovalRecord{}, err
	}
	return LoadApproval(ctx, db, approvalID)
}

func GrantApprovalForOperation(ctx context.Context, db *sql.DB, site, operationID, toolName string, args map[string]any, grantedBy string) (ApprovalRecord, error) {
	if db == nil {
		return ApprovalRecord{}, fmt.Errorf("approval store unavailable")
	}
	fp := approvalFingerprint(toolName, args)
	var approvalID string
	err := db.QueryRowContext(ctx, `
SELECT id
FROM _kora_ai_approval
WHERE site = ? AND operation_id = ? AND tool_name = ? AND state = 'pending_approval' AND target_fingerprint = ?
ORDER BY requested_at DESC
LIMIT 1`, site, operationID, toolName, fp).Scan(&approvalID)
	if err != nil {
		return ApprovalRecord{}, err
	}
	return GrantApproval(ctx, db, approvalID, grantedBy)
}

func MarkRunPendingApproval(ctx context.Context, db *sql.DB, runID, stepID, reason string) error {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "pending_approval"
	}
	rec.Status = "pending_approval"
	rec.CurrentStepID = stepID
	rec.ErrorMessage = reason
	rec.UpdatedAt = time.Now().UTC()
	if err := UpsertRun(ctx, db, rec); err != nil {
		return err
	}
	return UpsertStep(ctx, db, stepID, rec.Site, rec.ID, rec.ConversationID, stepID, "pending_approval", reason, "", "", "", reason)
}

func MarkRunPlanning(ctx context.Context, db *sql.DB, runID string) (RunRecord, error) {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return RunRecord{}, err
	}
	rec.Status = "planning"
	rec.ErrorMessage = ""
	rec.UpdatedAt = time.Now().UTC()
	if err := UpsertRun(ctx, db, rec); err != nil {
		return RunRecord{}, err
	}
	return LoadRun(ctx, db, runID)
}

// CancelRun marks a run and its conversation as cancelled.
func CancelRun(ctx context.Context, db *sql.DB, runID, reason string) error {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if reason == "" {
		reason = "cancelled"
	}
	if err := UpsertRun(ctx, db, RunRecord{
		ID:                 rec.ID,
		Site:               rec.Site,
		ConversationID:     rec.ConversationID,
		Channel:            rec.Channel,
		Status:             "cancelled",
		Model:              rec.Model,
		Provider:           rec.Provider,
		CurrentStepID:      rec.CurrentStepID,
		Summary:            rec.Summary,
		InputMessage:       rec.InputMessage,
		OutputMessage:      rec.OutputMessage,
		ErrorMessage:       rec.ErrorMessage,
		CancelReason:       reason,
		ResumeToken:        rec.ResumeToken,
		CreatedAt:          rec.CreatedAt,
		CompletedAt:        rec.CompletedAt,
		CancelledAt:        now,
		RetentionExpiresAt: rec.RetentionExpiresAt,
	}); err != nil {
		return err
	}
	conv, _ := LoadConversationByID(ctx, db, rec.ConversationID)
	conv.Summary = rec.Summary
	conv.Status = "cancelled"
	conv.LastRunID = rec.ID
	conv.LastMessageAt = now
	return UpsertConversation(ctx, db, conv)
}

// ResumeRun returns a terminal-safe run snapshot and assigns a new resume token.
func ResumeRun(ctx context.Context, db *sql.DB, runID, resumeToken string) (RunRecord, error) {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return RunRecord{}, err
	}
	if strings.TrimSpace(rec.ResumeToken) != "" {
		if strings.TrimSpace(resumeToken) == "" || resumeToken != rec.ResumeToken {
			return RunRecord{}, fmt.Errorf("invalid resume token")
		}
	}
	if strings.EqualFold(rec.Status, "cancelled") || strings.EqualFold(rec.Status, "completed") || strings.EqualFold(rec.Status, "failed") {
		return rec, nil
	}
	rec.ResumeToken = ulid.Make().String()
	rec.Status = "planning"
	rec.UpdatedAt = time.Now().UTC()
	if err := UpsertRun(ctx, db, rec); err != nil {
		return RunRecord{}, err
	}
	return LoadRun(ctx, db, runID)
}

// RefreshRunSummary rolls the latest step and message content into the run and conversation summary.
func RefreshRunSummary(ctx context.Context, db *sql.DB, runID string) error {
	rec, err := LoadRun(ctx, db, runID)
	if err != nil {
		return err
	}
	var stepSummary string
	_ = db.QueryRowContext(ctx, `
SELECT COALESCE(summary, '')
FROM _kora_ai_step
WHERE run_id = ?
ORDER BY created_at DESC
LIMIT 1`, runID).Scan(&stepSummary)
	if stepSummary == "" {
		stepSummary = rec.OutputMessage
	}
	if stepSummary == "" {
		stepSummary = rec.Summary
	}
	if stepSummary == "" {
		stepSummary = rec.InputMessage
	}
	if err := UpsertRun(ctx, db, RunRecord{
		ID:                 rec.ID,
		Site:               rec.Site,
		ConversationID:     rec.ConversationID,
		Channel:            rec.Channel,
		Status:             rec.Status,
		Model:              rec.Model,
		Provider:           rec.Provider,
		CurrentStepID:      rec.CurrentStepID,
		Summary:            stepSummary,
		InputMessage:       rec.InputMessage,
		OutputMessage:      rec.OutputMessage,
		ErrorMessage:       rec.ErrorMessage,
		CancelReason:       rec.CancelReason,
		ResumeToken:        rec.ResumeToken,
		CreatedAt:          rec.CreatedAt,
		CompletedAt:        rec.CompletedAt,
		CancelledAt:        rec.CancelledAt,
		RetentionExpiresAt: rec.RetentionExpiresAt,
	}); err != nil {
		return err
	}
	return UpsertConversation(ctx, db, ConversationRecord{
		ID:                 rec.ConversationID,
		Site:               rec.Site,
		Channel:            rec.Channel,
		Summary:            stepSummary,
		Status:             rec.Status,
		LastRunID:          rec.ID,
		LastMessageAt:      time.Now().UTC(),
		RetentionExpiresAt: rec.RetentionExpiresAt,
	})
}

// CleanupExpired removes expired conversations, runs, messages, and steps in one transaction.
func CleanupExpired(ctx context.Context, db *sql.DB, now time.Time) (removed int64, err error) {
	if db == nil {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	now = now.UTC()
	execDelete := func(query string, args ...any) error {
		res, execErr := tx.ExecContext(ctx, query, args...)
		if execErr != nil {
			return execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		removed += n
		return nil
	}

	if err = execDelete(`
DELETE FROM _kora_ai_message
WHERE conversation_id IN (
	SELECT id FROM _kora_ai_conversation
	WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
)
OR run_id IN (
	SELECT id FROM _kora_ai_run
	WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
)`, now, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err = execDelete(`
DELETE FROM _kora_ai_step
WHERE run_id IN (
	SELECT id FROM _kora_ai_run
	WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
	OR conversation_id IN (
		SELECT id FROM _kora_ai_conversation
		WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
	)
)`, now, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err = execDelete(`
DELETE FROM _kora_ai_run
WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
OR conversation_id IN (
	SELECT id FROM _kora_ai_conversation
	WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?
)`, now, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err = execDelete(`
DELETE FROM _kora_ai_conversation
WHERE retention_expires_at IS NOT NULL AND retention_expires_at <= ?`, now); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	return removed, nil
}
