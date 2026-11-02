package workflow

import (
	"context"
	"database/sql"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// StatusPolicyRepository resolves transition policy from the latest published
// record.update_status steps inside the record transaction that applies it.
type StatusPolicyRepository interface {
	GetStatusPolicyInTx(context.Context, *sql.Tx, contract.TenantID, string) (*StatusPolicy, bool, error)
}

// StatusPolicyReader resolves published status actions for domain authorization
// such as policy-managed approval assignment.
type StatusPolicyReader interface {
	GetStatusPolicy(context.Context, contract.TenantID, string) (*StatusPolicy, bool, error)
}

type Fence uint64

// Claim is an optimistic, tenant-scoped lease. Stores must validate owner,
// fence, revision, step, and DB-time lease validity on every mutation.
type Claim struct {
	Tenant         contract.TenantID
	BusinessScope  contract.BusinessScope
	CausationDepth uint16
	InstanceID     contract.InstanceID
	StepID         contract.StepID
	// Attempt is the next durable attempt number for this step execution,
	// derived by the store from persisted attempts when the lease is claimed.
	Attempt uint8
	Owner   string
	Fence   Fence
	// LeaseUntil is informational for scheduling. Store mutations compare the
	// persisted lease to database time and never trust this caller timestamp.
	LeaseUntil time.Time
	Revision   contract.Revision
	// FirstAttemptAt is the database timestamp of the first durable attempt for
	// this step and keeps retries bounded across worker restarts.
	FirstAttemptAt time.Time
}

type StartInstanceInput struct {
	Tenant           contract.TenantID
	BusinessScope    contract.BusinessScope
	CausationDepth   uint16
	ID               contract.InstanceID
	Definition       contract.WorkflowDefinition
	DefinitionDigest string
	Subject          contract.SubjectRef
	Actor            contract.PrincipalID
	IdempotencyKey   contract.IdempotencyKey
	Site             string
	Now              time.Time
}

type InstanceRepository interface {
	StartInstance(context.Context, StartInstanceInput) (contract.WorkflowInstance, error)
	GetInstance(context.Context, contract.TenantID, contract.InstanceID) (contract.WorkflowInstance, error)
}

// HistoryRepository exposes bounded, tenant-scoped run history for an
// already-authorized workflow instance.
type HistoryRepository interface {
	ListHistory(context.Context, contract.TenantID, contract.InstanceID, uint16) ([]HistoryEntry, error)
	ListRecentInstances(context.Context, contract.TenantID, uint16) ([]contract.WorkflowInstance, error)
}

type HistoryEntry struct {
	Sequence  uint64    `json:"sequence"`
	StepID    string    `json:"step_id"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// PreviewRun is durable, side-effect-free workflow test history. Sample record
// values are intentionally excluded to avoid retaining customer data.
type PreviewRun struct {
	ID                string                `json:"id"`
	DefinitionID      contract.DefinitionID `json:"definition_id"`
	DefinitionVersion uint32                `json:"definition_version"`
	DefinitionDigest  string                `json:"definition_digest"`
	Status            string                `json:"status"`
	Outcome           string                `json:"outcome,omitempty"`
	Failure           string                `json:"failure,omitempty"`
	Steps             []PreviewStep         `json:"steps"`
	CreatedAt         time.Time             `json:"created_at"`
}

type PreviewHistoryRepository interface {
	RecordPreview(context.Context, contract.TenantID, contract.PrincipalID, PreviewRun) error
	ListPreviews(context.Context, contract.TenantID, uint16) ([]PreviewRun, error)
	HasSuccessfulPreview(context.Context, contract.TenantID, contract.DefinitionID, uint32, string) (bool, error)
}

type StepCommit struct {
	Claim       Claim
	ExecutionID string
	Attempt     uint8
	// AttemptStarted indicates an external attempt row was durably created
	// before the side effect began; commit must finalize that exact row.
	AttemptStarted bool
	IdempotencyKey contract.IdempotencyKey
	Status         contract.InstanceStatus
	NextStep       contract.StepID
	NextWakeAt     *time.Time
	EventWait      *EventWaitSubscription
	Result         ActionResult
	Failure        string
	Events         []contract.EventEnvelope
}

// EventWaitSubscription binds one event wait to the workflow subject. The
// correlation value is derived from the persisted instance, never the event
// caller or workflow definition payload.
type EventWaitSubscription struct {
	EventType        string
	CorrelationField string
	CorrelationValue string
	OnEvent          contract.StepID
}

// Signal is an event delivered by a trusted tenant-scoped consumer. Tenant is
// the stable tenant identity; Event.Site may be a tenant route alias.
type Signal struct {
	Tenant contract.TenantID
	Event  contract.EventEnvelope
}

// SignalStore durably accepts trusted events and resumes bounded batches of
// matching event waits. Duplicate event IDs and concurrent workers are safe.
type SignalStore interface {
	AcceptSignal(context.Context, Signal) (uint32, error)
	ResumeEventSignals(context.Context, contract.TenantID, uint16) (uint16, error)
}

// ExecutionStore is the durable executor boundary. Implementations must make
// claim and commit predicates atomic with their persisted lease state.
type ExecutionStore interface {
	ClaimDue(context.Context, contract.TenantID, string, uint16, time.Duration) ([]Claim, error)
	Renew(context.Context, Claim, time.Duration) (Claim, error)
	BeginExternalAttempt(context.Context, Claim, string, contract.IdempotencyKey, *contract.PinnedDispatch) (*contract.PinnedDispatch, error)
	CommitStep(context.Context, StepCommit) error
	Release(context.Context, Claim) error
}

// PinnedDispatchRepository loads the durable provider identity for an
// uncertain action so reconciliation always addresses the provider that was called.
type PinnedDispatchRepository interface {
	LoadPinnedDispatch(context.Context, contract.TenantID, contract.InstanceID, contract.StepID) (contract.PinnedDispatch, error)
}

// HumanWaitRecoveryStore is the durable terminal-request recovery boundary.
// Implementations must bound both the returned batch and transaction scope.
type HumanWaitRecoveryStore interface {
	ExpireDue(context.Context, contract.TenantID, uint16) (uint16, error)
	ResumeTerminalHumanWaits(context.Context, contract.TenantID, uint16) (uint16, error)
}

// TimedWaitRecoveryStore resumes generic workflow timeout steps. Human waits
// remain owned by request terminal-state recovery, which selects their outcome.
type TimedWaitRecoveryStore interface {
	ResumeDueTimedWaits(context.Context, contract.TenantID, uint16) (uint16, error)
}

// EventWaitRecoveryStore scans durable signal/subscription pairs left by a
// bounded acceptance pass or a process interruption.
type EventWaitRecoveryStore interface {
	ResumeEventSignals(context.Context, contract.TenantID, uint16) (uint16, error)
}

type ReconcileCommit struct {
	Tenant           contract.TenantID
	InstanceID       contract.InstanceID
	Actor            contract.PrincipalID
	Site             string
	ExpectedRevision contract.Revision
	StepID           contract.StepID
	Outcome          contract.ReconciliationOutcome
	EvidenceRef      string
	Reason           string
	NextStep         contract.StepID
	Status           contract.InstanceStatus
	IdempotencyKey   contract.IdempotencyKey
}

// ReconciliationRepository records privileged operator decisions for unknown
// external outcomes with an immutable, scoped idempotency receipt.
type ReconciliationRepository interface {
	Reconcile(context.Context, ReconcileCommit) (contract.WorkflowInstance, error)
}

type CancelCommit struct {
	Tenant           contract.TenantID
	InstanceID       contract.InstanceID
	Actor            contract.PrincipalID
	Site             string
	ExpectedRevision contract.Revision
	Reason           string
	IdempotencyKey   contract.IdempotencyKey
}

type CancellationRepository interface {
	Cancel(context.Context, CancelCommit) (contract.WorkflowInstance, error)
}
