package contract

import (
	"encoding/json"
	"time"
)

//contract:enum:v2
type StepKind string

const (
	StepAction    StepKind = "action"
	StepCondition StepKind = "condition"
	StepHuman     StepKind = "human"
	StepWait      StepKind = "wait"
	StepEnd       StepKind = "end"
)

//contract:enum:v2
type InstanceStatus string

const (
	InstanceReady     InstanceStatus = "ready"
	InstanceRunning   InstanceStatus = "running"
	InstanceWaiting   InstanceStatus = "waiting"
	InstanceRetrying  InstanceStatus = "retrying"
	InstanceCompleted InstanceStatus = "completed"
	InstanceCancelled InstanceStatus = "cancelled"
	InstanceFailed    InstanceStatus = "failed"
	InstanceUncertain InstanceStatus = "outcome_unknown"
)

//contract:enum:v2
type SubjectBinding string

const SubjectInstance SubjectBinding = "instance_subject"

//contract:enum:v2
type ReconciliationOutcome string

const (
	ReconcileSucceeded  ReconciliationOutcome = "confirmed_succeeded"
	ReconcileNotApplied ReconciliationOutcome = "confirmed_not_applied"
	ReconcileAbandon    ReconciliationOutcome = "abandon"
)

//contract:wire:v2
type ActionSpec struct {
	Name        string `json:"name"`
	Version     uint32 `json:"version"`
	InputSchema string `json:"input_schema"`
	//contract:json
	//contract:maxJSONBytes=65536
	Input json.RawMessage `json:"input"`
	Next  StepID          `json:"next"`
}

//contract:wire:v2
type ConditionSpec struct {
	//contract:maxLength=8192
	Expression string `json:"expression"`
	IfTrue     StepID `json:"if_true"`
	IfFalse    StepID `json:"if_false"`
}

//contract:wire:v2
type HumanRequestTemplate struct {
	Kind    RequestKind    `json:"kind"`
	Subject SubjectBinding `json:"subject"`
	//contract:minLength=1
	//contract:maxLength=160
	Title string `json:"title"`
	//contract:maxBytes=8192
	Message    string          `json:"message"`
	PolicyID   string          `json:"policy_id,omitempty"`
	Assignment *AssignmentSpec `json:"assignment,omitempty"`
	//contract:min=1
	//contract:max=2678400
	DueAfterSeconds   uint32     `json:"due_after_seconds"`
	InformationSchema *SchemaRef `json:"information_schema,omitempty"`
}

//contract:wire:v2
type HumanSpec struct {
	Request     HumanRequestTemplate    `json:"request"`
	Outcomes    map[ResponseKind]StepID `json:"outcomes"`
	OnExpired   StepID                  `json:"on_expired"`
	OnCancelled StepID                  `json:"on_cancelled"`
	OnStale     StepID                  `json:"on_stale"`
}

//contract:wire:v2
type WaitSpec struct {
	EventType        string `json:"event_type,omitempty"`
	CorrelationField string `json:"correlation_field,omitempty"`
	//contract:min=1
	//contract:max=2678400
	TimeoutSeconds uint32 `json:"timeout_seconds"`
	OnEvent        StepID `json:"on_event,omitempty"`
	OnTimeout      StepID `json:"on_timeout"`
}

//contract:wire:v2
type EndSpec struct {
	//contract:minLength=1
	//contract:maxLength=120
	Outcome string `json:"outcome"`
}

//contract:wire:v2
//contract:taggedUnion:v2
//contract:discriminator=Kind
//contract:variants=Action,Condition,Human,Wait,End
type StepDefinition struct {
	ID        StepID         `json:"id"`
	Kind      StepKind       `json:"kind"`
	Action    *ActionSpec    `json:"action,omitempty"`
	Condition *ConditionSpec `json:"condition,omitempty"`
	Human     *HumanSpec     `json:"human,omitempty"`
	Wait      *WaitSpec      `json:"wait,omitempty"`
	End       *EndSpec       `json:"end,omitempty"`
}

//contract:wire:v2
type WorkflowDefinition struct {
	ID      DefinitionID `json:"id"`
	Version uint32       `json:"version"`
	Entry   StepID       `json:"entry"`
	//contract:minItems=1
	//contract:maxItems=100
	Steps []StepDefinition `json:"steps"`
}

//contract:wire:v2
type DefinitionVersionRef struct {
	DefinitionID DefinitionID `json:"definition_id"`
	Version      uint32       `json:"version"`
	Digest       string       `json:"digest"`
}

//contract:wire:v2
type ValidationIssue struct {
	Path   string `json:"path"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

//contract:wire:v2
type ValidationResult struct {
	Valid bool `json:"valid"`
	//contract:maxItems=100
	Issues []ValidationIssue `json:"issues"`
}

//contract:wire:v2
type StartWorkflow struct {
	Meta              CommandMeta  `json:"meta"`
	DefinitionID      DefinitionID `json:"definition_id"`
	DefinitionVersion uint32       `json:"definition_version"`
	Subject           SubjectRef   `json:"subject"`
}

//contract:wire:v2
type WorkflowInstance struct {
	ID                InstanceID     `json:"id"`
	Revision          Revision       `json:"revision"`
	DefinitionID      DefinitionID   `json:"definition_id"`
	DefinitionVersion uint32         `json:"definition_version"`
	Subject           SubjectRef     `json:"subject"`
	Status            InstanceStatus `json:"status"`
	CurrentStep       StepID         `json:"current_step"`
	NextWakeAt        *time.Time     `json:"next_wake_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

//contract:wire:v2
type WorkflowEventData struct {
	InstanceID InstanceID `json:"instance_id"`
	Revision   Revision   `json:"revision"`
	ChangeKind string     `json:"change_kind"`
}

//contract:wire:v2
type CancelWorkflow struct {
	Meta CommandMeta `json:"meta"`
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}

//contract:wire:v2
type ReconcileWorkflow struct {
	Meta    CommandMeta           `json:"meta"`
	StepID  StepID                `json:"step_id"`
	Outcome ReconciliationOutcome `json:"outcome"`
	//contract:maxLength=512
	EvidenceRef string `json:"evidence_ref"`
	//contract:minLength=1
	//contract:maxBytes=8192
	Reason string `json:"reason"`
}
