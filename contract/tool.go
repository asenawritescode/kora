package contract

import "time"

// ToolSafetyLevel classifies the risk of a tool. Adapters render this into their
// own presentation (e.g., confirmation dialogs), but the value originates here.
//
//contract:enum
type ToolSafetyLevel string

const (
	ToolSafetySafe     ToolSafetyLevel = "safe"
	ToolSafetyStandard ToolSafetyLevel = "standard"
	ToolSafetyElevated ToolSafetyLevel = "elevated"
	ToolSafetyHigh     ToolSafetyLevel = "high"
	ToolSafetySystem   ToolSafetyLevel = "system"
)

// ToolArgumentVersion labels the frozen argument schema for a tool. It is bumped
// whenever a tool's arguments change in a breaking way (RFC §10.4.2).
//
//contract:enum
type ToolArgumentVersion string

// FieldHint is the canonical, serialized description of a doctype field exposed
// through a tool projection. It is intentionally free of doctype package types.
//
//contract:wire
type FieldHint struct {
	Name           string   `json:"name"`
	Label          string   `json:"label,omitempty"`
	Fieldtype      string   `json:"fieldtype"`
	Type           string   `json:"type,omitempty"`
	Format         string   `json:"format,omitempty"`
	Options        []string `json:"options,omitempty"`
	LinkTarget     string   `json:"link_target,omitempty"`
	TableTarget    string   `json:"table_target,omitempty"`
	Required       bool     `json:"required,omitempty"`
	ReadOnly       bool     `json:"read_only,omitempty"`
	Computed       bool     `json:"computed,omitempty"`
	Writable       bool     `json:"writable"`
	StandardFilter bool     `json:"standard_filter,omitempty"`
	SearchIndex    bool     `json:"search_index,omitempty"`
	InListView     bool     `json:"in_list_view,omitempty"`
	Unique         bool     `json:"unique,omitempty"`
	Description    string   `json:"description,omitempty"`
}

// SystemFieldHint describes a system column exposed through a tool projection.
//
//contract:wire
type SystemFieldHint struct {
	Name      string `json:"name"`
	Fieldtype string `json:"fieldtype"`
	Writable  bool   `json:"writable"`
}

// ToolDescriptor is the canonical, versioned description of a callable tool. The
// RFC mandates that BuildToolCatalog becomes the sole registry projection and
// that every adapter (chat, channel, MCP, SDK, UI, HTTP, NATS) renders from this
// shape. This type carries no provider-specific or doctype-package dependencies
// so it can be serialized on the wire and compared across adapters.
type ToolDescriptor struct {
	ID                      string              `json:"id"`
	Source                  string              `json:"source"`
	Name                    string              `json:"name"`
	Description             string              `json:"description"`
	InputSchema             map[string]any      `json:"input_schema"`
	SafetyLevel             ToolSafetyLevel     `json:"safety_level"`
	RequiresConfirmation    bool                `json:"requires_confirmation"`
	RequiresRecentAuth      bool                `json:"requires_recent_auth"`
	ChannelAllowlist        []string            `json:"channel_allowlist"`
	ArgumentContractVersion ToolArgumentVersion `json:"argument_contract_version"`
	Operation               string              `json:"operation"`
	Doctype                 string              `json:"doctype,omitempty"`
	DoctypeLabel            string              `json:"doctype_label,omitempty"`
	TitleField              string              `json:"title_field,omitempty"`
	SearchFields            []string            `json:"search_fields,omitempty"`
	SortField               string              `json:"sort_field,omitempty"`
	SortOrder               string              `json:"sort_order,omitempty"`
	FieldHints              []FieldHint         `json:"field_hints,omitempty"`
	SystemFields            []SystemFieldHint   `json:"system_fields,omitempty"`
}

// ToolCatalog is the versioned registry projection shared across adapters.
type ToolCatalog struct {
	Version string           `json:"version"`
	Tools   []ToolDescriptor `json:"tools"`
}

// UsageClass classifies a token/usage record per the FinOps FOCUS-normalized
// attribution model referenced in RFC §18.
//
//contract:enum
type UsageClass string

const (
	UsageClassInput     UsageClass = "input"
	UsageClassOutput    UsageClass = "output"
	UsageClassReasoning UsageClass = "reasoning"
	UsageClassTotal     UsageClass = "total"
	UsageClassPartial   UsageClass = "partial"
)

// UsageEvent is an immutable record of one provider attempt (RFC §10.4.4 /
// §14.4.7). It is never overwritten; corrections and reconciliation create new
// records rather than mutating this one.
type UsageEvent struct {
	ID           string               `json:"id"`
	Site         string               `json:"site"`
	Organization string               `json:"organization_id,omitempty"`
	UserID       string               `json:"user_id,omitempty"`
	Model        string               `json:"model"`
	Provider     string               `json:"provider"`
	RunID        string               `json:"run_id,omitempty"`
	StepID       string               `json:"step_id,omitempty"`
	Channel      string               `json:"channel,omitempty"`
	Attempt      int                  `json:"attempt"`
	Status       string               `json:"status"` // completed|partial|failed|zero
	Tokens       map[UsageClass]int64 `json:"tokens,omitempty"`
	LatencyMs    int64                `json:"latency_ms,omitempty"`
	OccurredAt   time.Time            `json:"occurred_at"`
	RetryOf      string               `json:"retry_of,omitempty"`
	Attribution  map[string]string    `json:"attribution,omitempty"`
}

// ApprovalState is the durable state of a confirmation-required operation
// (RFC §10.4.2 / validation gate 4). Confirmation cannot be satisfied by prompt
// text or client-only headers.
//
//contract:enum
type ApprovalState string

const (
	ApprovalPending ApprovalState = "pending_approval"
	ApprovalGranted ApprovalState = "granted"
	ApprovalDenied  ApprovalState = "denied"
	ApprovalExpired ApprovalState = "expired"
	ApprovalRevoked ApprovalState = "revoked"
)

// Approval is the durable record of a required confirmation or recent-auth
// check, including the full argument/target fingerprint so that a pending
// approval is invalidated when its targets change.
//
//contract:wire
type Approval struct {
	ID                string        `json:"id"`
	Site              string        `json:"site"`
	OperationID       string        `json:"operation_id"`
	Actor             ActorContext  `json:"actor"`
	ToolName          string        `json:"tool_name"`
	State             ApprovalState `json:"state"`
	TargetFingerprint string        `json:"target_fingerprint"`
	ArgumentHash      string        `json:"argument_hash"`
	RecordVersion     int           `json:"record_version,omitempty"`
	RequestedAt       time.Time     `json:"requested_at"`
	ExpiresAt         time.Time     `json:"expires_at,omitempty"`
	GrantedAt         time.Time     `json:"granted_at,omitempty"`
	GrantedBy         string        `json:"granted_by,omitempty"`
	AuthSessionID     string        `json:"auth_session_id,omitempty"`
}

// Cursor is an opaque, resumable position for a durable stream: an AI run
// checkpoint, a conversation delivery cursor, or an offline sync cursor.
//
//contract:wire
type Cursor struct {
	Token   string    `json:"token"`
	Version int64     `json:"version,omitempty"`
	Nonce   string    `json:"nonce,omitempty"`
	At      time.Time `json:"at"`
}
