// Package script provides the JavaScript runtime for Kora's extensibility system.
// It defines the Runner interface and types for script execution.
package script

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Type classifies the kind of script.
type Type string

const (
	TypeDocEvent       Type = "doc_event"
	TypeAPIMethod      Type = "api_method"
	TypeWorkflowAction Type = "workflow_action"
	TypeScheduled      Type = "scheduled"
)

// Event is the lifecycle hook point.
type Event string

const (
	EventBeforeInsert Event = "before_insert"
	EventAfterInsert  Event = "after_insert"
	EventBeforeSave   Event = "before_save"
	EventAfterSave    Event = "after_save"
	EventBeforeDelete Event = "before_delete"
	EventAfterDelete  Event = "after_delete"
	EventBeforeSubmit Event = "before_submit"
	EventAfterSubmit  Event = "after_submit"
	EventBeforeCancel Event = "before_cancel"
	EventAfterCancel  Event = "after_cancel"
	EventValidate     Event = "validate"
	EventComputed     Event = "computed"
)

// ExecuteRequest carries all context for a single script execution.
type ExecuteRequest struct {
	Script      string         // JavaScript source
	ScriptType  Type           // doc_event, api_method, etc.
	ScriptName  string         // from _kora_script.name
	DocType     string         // doctype being operated on
	Event       Event          // lifecycle hook point
	Document    map[string]any // current document state
	OldDocument map[string]any // previous document state (nil for inserts)
	User        string         // authenticated user email
	UserRoles   []string       // user's roles
	Site        string         // site hostname
	Timeout     time.Duration  // per-execution timeout

	// Provider bridges the JS runtime to the Kora engine.
	// If nil, kora.getDoc/kora.saveDoc etc. return safe defaults.
	Provider KoraProvider
}

// ExecuteResult is returned by a successful script execution.
type ExecuteResult struct {
	Document map[string]any `json:"document,omitempty"` // modified document (nil if unchanged)
	Modified bool           `json:"modified"`           // true if document was changed
	Result   any            `json:"result,omitempty"`   // return value (for api_method scripts)
	Logs     []LogEntry     `json:"logs,omitempty"`     // log messages from the script
	Duration time.Duration  `json:"duration_ms"`        // execution time
}

// LogEntry is a single log message from a script execution.
type LogEntry struct {
	Level   string `json:"level"`   // debug, info, warn, error
	Message string `json:"message"` // log message
}

// Runner executes JavaScript scripts with sandboxing.
type Runner interface {
	// Execute runs a script and returns the result or an error.
	Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error)

	// Validate checks whether a script compiles without executing it.
	Validate(script string) error

	// Close releases resources held by the runner.
	Close() error
}

// Store persists and retrieves script definitions.
type Store struct {
	DB      *sql.DB
	Dialect Dialect
}

// Dialect abstracts SQL dialect differences for script storage.
type Dialect interface {
	Placeholder(n int) string
}

// ScriptRecord mirrors a _kora_script row.
type ScriptRecord struct {
	Name           string     `json:"name"`
	Site           string     `json:"site"`
	ScriptType     Type       `json:"script_type"`
	DocType        string     `json:"doctype"`
	Event          Event      `json:"event"`
	MethodPath     string     `json:"method_path"`
	WorkflowAction string     `json:"workflow_action"`
	Schedule       string     `json:"schedule"`
	Priority       int        `json:"priority"`
	IsActive       bool       `json:"is_active"`
	RunAs          string     `json:"run_as"`
	TimeoutMs      int        `json:"timeout_ms"`
	Script         string     `json:"script"`
	CompiledAt     *time.Time `json:"compiled_at,omitempty"`
	CompileError   string     `json:"compile_error,omitempty"`
	CreatedBy      string     `json:"created_by"`
	UpdatedBy      string     `json:"updated_by"`
	CreatedAt      time.Time  `json:"creation"`
	UpdatedAt      time.Time  `json:"modified"`
}

// LoadActiveScripts returns all active scripts for a site, filtered by optional doctype+event.
func (s *Store) LoadActiveScripts(site string, doctype string, event Event) ([]ScriptRecord, error) {
	query := `SELECT name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		created_by, updated_by, creation, modified
		FROM _kora_script
		WHERE site = ? AND is_active = 1 AND script_type = 'doc_event'`
	args := []any{site}

	if doctype != "" {
		query += " AND doctype = ?"
		args = append(args, doctype)
	}
	if event != "" {
		query += " AND event = ?"
		args = append(args, string(event))
	}
	query += " ORDER BY priority ASC"

	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("script: load active: %w", err)
	}
	defer rows.Close()

	var scripts []ScriptRecord
	for rows.Next() {
		var r ScriptRecord
		var compiledAt, compileError, createdBy, updatedBy sql.NullString
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&r.Name, &r.Site, &r.ScriptType, &r.DocType, &r.Event,
			&r.MethodPath, &r.WorkflowAction, &r.Schedule,
			&r.Priority, &r.IsActive, &r.RunAs, &r.TimeoutMs, &r.Script,
			&compiledAt, &compileError, &createdBy, &updatedBy, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("script: scan: %w", err)
		}
		r.CompileError = compileError.String
		r.CreatedBy = createdBy.String
		r.UpdatedBy = updatedBy.String
		r.CreatedAt = createdAt
		r.UpdatedAt = updatedAt
		if compiledAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05.999999", compiledAt.String)
			r.CompiledAt = &t
		}
		scripts = append(scripts, r)
	}
	return scripts, rows.Err()
}

// LoadMethodScript returns the active api_method script for a given method path, or nil.
func (s *Store) LoadMethodScript(site string, methodPath string) (*ScriptRecord, error) {
	query := `SELECT name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		created_by, updated_by, creation, modified
		FROM _kora_script
		WHERE site = ? AND is_active = 1 AND script_type = 'api_method' AND method_path = ?
		LIMIT 1`

	var r ScriptRecord
	var compiledAt, compileError, createdBy, updatedBy sql.NullString
	var createdAt, updatedAt time.Time
	err := s.DB.QueryRow(query, site, methodPath).Scan(
		&r.Name, &r.Site, &r.ScriptType, &r.DocType, &r.Event,
		&r.MethodPath, &r.WorkflowAction, &r.Schedule,
		&r.Priority, &r.IsActive, &r.RunAs, &r.TimeoutMs, &r.Script,
		&compiledAt, &compileError, &createdBy, &updatedBy, &createdAt, &updatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("script: load method: %w", err)
	}
	r.CompileError = compileError.String
	r.CreatedBy = createdBy.String
	r.UpdatedBy = updatedBy.String
	r.CreatedAt = createdAt
	r.UpdatedAt = updatedAt
	if compiledAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05.999999", compiledAt.String)
		r.CompiledAt = &t
	}
	return &r, nil
}

// LoadWorkflowActionScripts returns active workflow_action scripts for a given workflow action name.
func (s *Store) LoadWorkflowActionScripts(site string, actionName string) ([]ScriptRecord, error) {
	query := `SELECT name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		created_by, updated_by, creation, modified
		FROM _kora_script
		WHERE site = ? AND is_active = 1 AND script_type = 'workflow_action' AND workflow_action = ?
		ORDER BY priority ASC`

	rows, err := s.DB.Query(query, site, actionName)
	if err != nil {
		return nil, fmt.Errorf("script: load workflow actions: %w", err)
	}
	defer rows.Close()

	var scripts []ScriptRecord
	for rows.Next() {
		var r ScriptRecord
		var compiledAt, compileError, createdBy, updatedBy sql.NullString
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&r.Name, &r.Site, &r.ScriptType, &r.DocType, &r.Event,
			&r.MethodPath, &r.WorkflowAction, &r.Schedule,
			&r.Priority, &r.IsActive, &r.RunAs, &r.TimeoutMs, &r.Script,
			&compiledAt, &compileError, &createdBy, &updatedBy, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("script: scan workflow action: %w", err)
		}
		r.CompileError = compileError.String
		r.CreatedBy = createdBy.String
		r.UpdatedBy = updatedBy.String
		r.CreatedAt = createdAt
		r.UpdatedAt = updatedAt
		if compiledAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05.999999", compiledAt.String)
			r.CompiledAt = &t
		}
		scripts = append(scripts, r)
	}
	return scripts, rows.Err()
}

// LogExecution records a script execution in _kora_script_execution.
func (s *Store) LogExecution(site string, rec ScriptRecord, docType, docName string, event Event, triggerUser string, durationMs int, status string, errMsg string) error {
	id, err := newULID()
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(
		`INSERT INTO _kora_script_execution (id, site, script_name, script_type, doctype, docname, event, trigger_user, duration_ms, status, error_message, logged_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))`,
		id, site, rec.Name, string(rec.ScriptType), docType, docName, string(event), triggerUser, durationMs, status, errMsg,
	)
	return err
}

// newULID generates a ULID for the execution log entry.
func newULID() (string, error) {
	// Use a simple time-based ID to avoid adding a ULID dependency here.
	// The orm package uses github.com/oklog/ulid/v2 — this package keeps it minimal.
	return fmt.Sprintf("sex_%d", time.Now().UnixNano()), nil
}

// IsBeforeEvent returns true if the event is a before_* (can reject) hook.
func IsBeforeEvent(e Event) bool {
	switch e {
	case EventBeforeInsert, EventBeforeSave, EventBeforeDelete,
		EventBeforeSubmit, EventBeforeCancel, EventValidate, EventComputed:
		return true
	}
	return false
}

// IsAfterEvent returns true if the event is an after_* (best-effort) hook.
func IsAfterEvent(e Event) bool {
	switch e {
	case EventAfterInsert, EventAfterSave, EventAfterDelete,
		EventAfterSubmit, EventAfterCancel:
		return true
	}
	return false
}

// LoadAllForSite returns all scripts for a site.
func (s *Store) LoadAllForSite(site string) ([]ScriptRecord, error) {
	rows, err := s.DB.Query(
		`SELECT name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		 priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		 created_by, updated_by, creation, modified
		 FROM _kora_script WHERE site = ? ORDER BY creation DESC`, site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScripts(rows)
}

// LoadByName returns a single script by site and name.
func (s *Store) LoadByName(site, name string) (*ScriptRecord, error) {
	var r ScriptRecord
	var compiledAt, compileError, createdBy, updatedBy sql.NullString
	var createdAt, updatedAt time.Time
	err := s.DB.QueryRow(
		`SELECT name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		 priority, is_active, run_as, timeout_ms, script, compiled_at, compile_error,
		 created_by, updated_by, creation, modified
		 FROM _kora_script WHERE site = ? AND name = ?`, site, name).Scan(
		&r.Name, &r.Site, &r.ScriptType, &r.DocType, &r.Event, &r.MethodPath, &r.WorkflowAction, &r.Schedule,
		&r.Priority, &r.IsActive, &r.RunAs, &r.TimeoutMs, &r.Script,
		&compiledAt, &compileError, &createdBy, &updatedBy, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.CompileError = compileError.String
	r.CreatedBy = createdBy.String
	r.UpdatedBy = updatedBy.String
	r.CreatedAt = createdAt
	r.UpdatedAt = updatedAt
	if compiledAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05.999999", compiledAt.String)
		r.CompiledAt = &t
	}
	return &r, nil
}

// Insert creates a new script record.
func (s *Store) Insert(r ScriptRecord) error {
	_, err := s.DB.Exec(
		`INSERT INTO _kora_script (name, site, script_type, doctype, event, method_path, workflow_action, schedule,
		 priority, is_active, run_as, timeout_ms, script, created_by, updated_by, creation, modified)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6), NOW(6))`,
		r.Name, r.Site, string(r.ScriptType), r.DocType, string(r.Event), r.MethodPath, r.WorkflowAction, r.Schedule,
		r.Priority, r.IsActive, r.RunAs, r.TimeoutMs, r.Script, r.CreatedBy, r.UpdatedBy)
	return err
}

// ScriptUpdateRequest carries the fields that can be updated on a script.
type ScriptUpdateRequest struct {
	ScriptType     *string `json:"script_type,omitempty"`
	DocType        *string `json:"doctype,omitempty"`
	Event          *string `json:"event,omitempty"`
	MethodPath     *string `json:"method_path,omitempty"`
	WorkflowAction *string `json:"workflow_action,omitempty"`
	Schedule       *string `json:"schedule,omitempty"`
	Priority       *int    `json:"priority,omitempty"`
	IsActive       *bool   `json:"is_active,omitempty"`
	RunAs          *string `json:"run_as,omitempty"`
	TimeoutMs      *int    `json:"timeout_ms,omitempty"`
	Script         *string `json:"script,omitempty"`
}

// Update modifies an existing script record. Only non-nil fields are applied.
func (s *Store) Update(site, name string, req ScriptUpdateRequest, updatedBy string) error {
	var sets []string
	var args []any

	if req.ScriptType != nil {
		sets = append(sets, "script_type = ?")
		args = append(args, *req.ScriptType)
	}
	if req.DocType != nil {
		sets = append(sets, "doctype = ?")
		args = append(args, *req.DocType)
	}
	if req.Event != nil {
		sets = append(sets, "event = ?")
		args = append(args, *req.Event)
	}
	if req.MethodPath != nil {
		sets = append(sets, "method_path = ?")
		args = append(args, *req.MethodPath)
	}
	if req.WorkflowAction != nil {
		sets = append(sets, "workflow_action = ?")
		args = append(args, *req.WorkflowAction)
	}
	if req.Schedule != nil {
		sets = append(sets, "schedule = ?")
		args = append(args, *req.Schedule)
	}
	if req.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *req.Priority)
	}
	if req.IsActive != nil {
		sets = append(sets, "is_active = ?")
		args = append(args, *req.IsActive)
	}
	if req.RunAs != nil {
		sets = append(sets, "run_as = ?")
		args = append(args, *req.RunAs)
	}
	if req.TimeoutMs != nil {
		sets = append(sets, "timeout_ms = ?")
		args = append(args, *req.TimeoutMs)
	}
	if req.Script != nil {
		sets = append(sets, "script = ?")
		args = append(args, *req.Script)
	}

	if len(sets) == 0 {
		return nil // nothing to update
	}

	sets = append(sets, "updated_by = ?")
	args = append(args, updatedBy)
	sets = append(sets, "modified = NOW(6)")
	args = append(args, site, name)

	query := fmt.Sprintf("UPDATE _kora_script SET %s WHERE site = ? AND name = ?", strings.Join(sets, ", "))
	_, err := s.DB.Exec(query, args...)
	return err
}

// Delete removes a script by site and name.
func (s *Store) Delete(site, name string) error {
	_, err := s.DB.Exec(`DELETE FROM _kora_script WHERE site = ? AND name = ?`, site, name)
	return err
}

// LoadExecutions returns recent executions for a script.
func (s *Store) LoadExecutions(site, name string, limit int) ([]map[string]any, error) {
	rows, err := s.DB.Query(
		`SELECT id, script_name, script_type, doctype, docname, event, trigger_user, duration_ms, status, error_message, logged_at
		 FROM _kora_script_execution WHERE site = ? AND script_name = ? ORDER BY logged_at DESC LIMIT ?`,
		site, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []map[string]any
	for rows.Next() {
		var id, scriptName, scriptType, doctype, docname, event, triggerUser, status, loggedAt string
		var errMsg sql.NullString
		var durationMs int
		if err := rows.Scan(&id, &scriptName, &scriptType, &doctype, &docname, &event, &triggerUser, &durationMs, &status, &errMsg, &loggedAt); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"id": id, "script_name": scriptName, "script_type": scriptType,
			"doctype": doctype, "docname": docname, "event": event,
			"trigger_user": triggerUser, "duration_ms": durationMs,
			"status": status, "error_message": errMsg.String, "logged_at": loggedAt,
		})
	}
	return results, rows.Err()
}

func scanScripts(rows *sql.Rows) ([]ScriptRecord, error) {
	var scripts []ScriptRecord
	for rows.Next() {
		var r ScriptRecord
		var compiledAt, compileError, createdBy, updatedBy sql.NullString
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&r.Name, &r.Site, &r.ScriptType, &r.DocType, &r.Event,
			&r.MethodPath, &r.WorkflowAction, &r.Schedule,
			&r.Priority, &r.IsActive, &r.RunAs, &r.TimeoutMs, &r.Script,
			&compiledAt, &compileError, &createdBy, &updatedBy, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		r.CompileError = compileError.String
		r.CreatedBy = createdBy.String
		r.UpdatedBy = updatedBy.String
		r.CreatedAt = createdAt
		r.UpdatedAt = updatedAt
		if compiledAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05.999999", compiledAt.String)
			r.CompiledAt = &t
		}
		scripts = append(scripts, r)
	}
	return scripts, rows.Err()
}
