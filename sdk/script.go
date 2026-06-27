package sdk

import "time"

const (
	TypeDocEvent       = "doc_event"
	TypeAPIMethod      = "api_method"
	TypeWorkflowAction = "workflow_action"
	TypeScheduled      = "scheduled"
	TypeComputed       = "computed"
	TypeValidate       = "validate"
)

const (
	EventBeforeInsert = "before_insert"
	EventAfterInsert  = "after_insert"
	EventBeforeSave   = "before_save"
	EventAfterSave    = "after_save"
	EventBeforeDelete = "before_delete"
	EventAfterDelete  = "after_delete"
	EventValidate     = "validate"
	EventComputed     = "computed"
)

type ScriptRecord struct {
	Name           string    `json:"name"`
	ScriptType     string    `json:"script_type"`
	DocType        string    `json:"doctype,omitempty"`
	Event          string    `json:"event,omitempty"`
	MethodPath     string    `json:"method_path,omitempty"`
	WorkflowAction string    `json:"workflow_action,omitempty"`
	Schedule       string    `json:"schedule,omitempty"`
	Script         string    `json:"script"`
	Priority       int       `json:"priority"`
	IsActive       bool      `json:"is_active"`
	RunAs          string    `json:"run_as,omitempty"`
	TimeoutMs      int       `json:"timeout_ms"`
	Description    string    `json:"description,omitempty"`
	Site           string    `json:"site"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	CreatedBy      string    `json:"created_by"`
}

type ExecuteRequest struct {
	Script      string
	ScriptType  string
	ScriptName  string
	User        string
	UserRoles   []string
	Site        string
	Document    map[string]any
	OldDocument map[string]any
	Provider    KoraProvider
	Timeout     time.Duration
}

type ExecuteResult struct {
	Document map[string]any
	Result   any
	Modified bool
	Duration time.Duration
	Logs     []LogEntry
}

type LogEntry struct {
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}
