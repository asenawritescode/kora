package contract

import (
	"encoding/json"
	"time"
)

//contract:enum:v2
type RequestKind string

const (
	RequestApproval       RequestKind = "approval"
	RequestReview         RequestKind = "review"
	RequestInformation    RequestKind = "information"
	RequestAcknowledgment RequestKind = "acknowledgment"
	RequestTask           RequestKind = "task"
)

//contract:enum:v2
type RequestStatus string

const (
	RequestPending   RequestStatus = "pending"
	RequestResolved  RequestStatus = "resolved"
	RequestCancelled RequestStatus = "cancelled"
	RequestExpired   RequestStatus = "expired"
	RequestStale     RequestStatus = "stale"
)

//contract:enum:v2
type ResponseKind string

const (
	ResponseApprove     ResponseKind = "approve"
	ResponseReject      ResponseKind = "reject"
	ResponseReturn      ResponseKind = "return_for_changes"
	ResponseProvide     ResponseKind = "provide_information"
	ResponseAcknowledge ResponseKind = "acknowledge"
	ResponseComplete    ResponseKind = "complete"
)

//contract:enum:v2
type AssignmentMode string

const (
	AssignmentAny        AssignmentMode = "any"
	AssignmentAll        AssignmentMode = "all"
	AssignmentSequential AssignmentMode = "sequential"
)

//contract:enum:v2
type AssigneeKind string

const (
	AssigneeUser  AssigneeKind = "user"
	AssigneeRole  AssigneeKind = "role"
	AssigneeGroup AssigneeKind = "group"
)

//contract:enum:v2
type RequestOutcome string

const (
	OutcomeApproved     RequestOutcome = "approved"
	OutcomeRejected     RequestOutcome = "rejected"
	OutcomeReturned     RequestOutcome = "returned_for_changes"
	OutcomeProvided     RequestOutcome = "information_provided"
	OutcomeAcknowledged RequestOutcome = "acknowledged"
	OutcomeCompleted    RequestOutcome = "completed"
)

//contract:enum:v2
type AssignmentStatus string

const (
	AssignmentBlocked    AssignmentStatus = "blocked"
	AssignmentPending    AssignmentStatus = "pending"
	AssignmentResponded  AssignmentStatus = "responded"
	AssignmentSuperseded AssignmentStatus = "superseded"
	AssignmentCancelled  AssignmentStatus = "cancelled"
)

//contract:enum:v2
type InboxView string

const (
	InboxAction    InboxView = "needs_action"
	InboxSent      InboxView = "sent"
	InboxWaiting   InboxView = "waiting"
	InboxCompleted InboxView = "completed"
)

//contract:enum:v2
type RequestHistoryKind string

const (
	HistoryCreated    RequestHistoryKind = "created"
	HistoryAssigned   RequestHistoryKind = "assigned"
	HistoryResponded  RequestHistoryKind = "responded"
	HistoryReassigned RequestHistoryKind = "reassigned"
	HistoryResolved   RequestHistoryKind = "resolved"
	HistoryCancelled  RequestHistoryKind = "cancelled"
	HistoryExpired    RequestHistoryKind = "expired"
	HistoryStale      RequestHistoryKind = "stale"
)

//contract:wire:v2
type Assignee struct {
	Kind AssigneeKind `json:"kind"`
	ID   string       `json:"id"`
}

//contract:wire:v2
type AssignmentSpec struct {
	Mode AssignmentMode `json:"mode"`
	//contract:maxItems=50
	Assignees []Assignee `json:"assignees"`
}

//contract:wire:v2
type RequestSource struct {
	InstanceID InstanceID `json:"instance_id,omitempty"`
	StepID     StepID     `json:"step_id,omitempty"`
}

//contract:wire:v2
type InformationResponse struct {
	SchemaID      string `json:"schema_id"`
	SchemaVersion uint32 `json:"schema_version"`
	//contract:json
	//contract:maxJSONBytes=65536
	Values json.RawMessage `json:"values"`
}

//contract:wire:v2
type SubmitRequest struct {
	Meta    CommandMeta `json:"meta"`
	Kind    RequestKind `json:"kind"`
	Subject *SubjectRef `json:"subject,omitempty"`
	//contract:minLength=1
	//contract:maxLength=160
	Title string `json:"title"`
	//contract:maxBytes=8192
	Message           string          `json:"message"`
	Assignment        *AssignmentSpec `json:"assignment,omitempty"`
	DueAt             *time.Time      `json:"due_at,omitempty"`
	PolicyID          string          `json:"policy_id,omitempty"`
	InformationSchema *SchemaRef      `json:"information_schema,omitempty"`
}

//contract:wire:v2
type RespondRequest struct {
	Meta         CommandMeta  `json:"meta"`
	AssignmentID AssignmentID `json:"assignment_id"`
	Response     ResponseKind `json:"response"`
	//contract:maxBytes=8192
	Comment     string               `json:"comment,omitempty"`
	Information *InformationResponse `json:"information,omitempty"`
}

//contract:wire:v2
type AssignmentView struct {
	ID               AssignmentID     `json:"id"`
	Stage            uint16           `json:"stage"`
	Assignee         Assignee         `json:"assignee"`
	Status           AssignmentStatus `json:"status"`
	AllowedResponses []ResponseKind   `json:"allowed_responses"`
}

//contract:wire:v2
type ContinuationView struct {
	OperationID OperationID    `json:"operation_id"`
	State       OperationState `json:"state"`
}

//contract:wire:v2
type RequestView struct {
	ID                RequestID         `json:"id"`
	Revision          Revision          `json:"revision"`
	Kind              RequestKind       `json:"kind"`
	Status            RequestStatus     `json:"status"`
	Requester         PrincipalID       `json:"requester"`
	Subject           *SubjectRef       `json:"subject,omitempty"`
	Title             string            `json:"title"`
	Message           string            `json:"message"`
	Source            *RequestSource    `json:"source,omitempty"`
	InformationSchema *SchemaRef        `json:"information_schema,omitempty"`
	DueAt             *time.Time        `json:"due_at,omitempty"`
	AllowedResponses  []ResponseKind    `json:"allowed_responses"`
	Assignments       []AssignmentView  `json:"assignments"`
	Outcome           *RequestOutcome   `json:"outcome,omitempty"`
	ResolvedAt        *time.Time        `json:"resolved_at,omitempty"`
	Continuation      *ContinuationView `json:"continuation,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

//contract:wire:v2
type ResponseReceipt struct {
	RequestID    RequestID         `json:"request_id"`
	DecisionID   DecisionID        `json:"decision_id"`
	Revision     Revision          `json:"revision"`
	Status       RequestStatus     `json:"status"`
	Outcome      *RequestOutcome   `json:"outcome,omitempty"`
	Continuation *ContinuationView `json:"continuation,omitempty"`
}

//contract:wire:v2
type RequestFilter struct {
	Page        PageRequest  `json:"page"`
	View        InboxView    `json:"view"`
	Kind        *RequestKind `json:"kind,omitempty"`
	OverdueOnly bool         `json:"overdue_only"`
}

//contract:wire:v2
type ReassignRequest struct {
	Meta         CommandMeta  `json:"meta"`
	AssignmentID AssignmentID `json:"assignment_id"`
	To           Assignee     `json:"to"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

//contract:wire:v2
type CancelRequest struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

//contract:wire:v2
type RequestHistoryEntry struct {
	Sequence         Revision           `json:"sequence"`
	Kind             RequestHistoryKind `json:"kind"`
	Actor            *PrincipalID       `json:"actor,omitempty"`
	AssignmentID     AssignmentID       `json:"assignment_id,omitempty"`
	PreviousAssignee *Assignee          `json:"previous_assignee,omitempty"`
	NewAssignee      *Assignee          `json:"new_assignee,omitempty"`
	Response         *ResponseKind      `json:"response,omitempty"`
	Comment          string             `json:"comment,omitempty"`
	At               time.Time          `json:"at"`
}

//contract:wire:v2
type ResponseDetail struct {
	DecisionID   DecisionID           `json:"decision_id"`
	RequestID    RequestID            `json:"request_id"`
	AssignmentID AssignmentID         `json:"assignment_id"`
	Response     ResponseKind         `json:"response"`
	Comment      string               `json:"comment,omitempty"`
	Information  *InformationResponse `json:"information,omitempty"`
	At           time.Time            `json:"at"`
}
