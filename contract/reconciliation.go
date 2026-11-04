package contract

import (
	"fmt"
	"strings"
	"time"
)

//contract:enum:v2
type BusinessFindingKind string

const (
	FindingMissingEffect     BusinessFindingKind = "missing_effect"
	FindingDuplicateEffect   BusinessFindingKind = "duplicate_effect"
	FindingAmountMismatch    BusinessFindingKind = "amount_mismatch"
	FindingScopeMismatch     BusinessFindingKind = "scope_mismatch"
	FindingUnknownOutcome    BusinessFindingKind = "unknown_outcome"
	FindingPendingGrace      BusinessFindingKind = "pending_grace"
	FindingLifecycleMismatch BusinessFindingKind = "lifecycle_mismatch"
)

//contract:enum:v2
type BusinessFindingState string

const (
	BusinessFindingOpen          BusinessFindingState = "open"
	BusinessFindingInvestigating BusinessFindingState = "investigating"
	BusinessFindingResolved      BusinessFindingState = "resolved"
	BusinessFindingWaived        BusinessFindingState = "waived"
)

// BusinessObligation is an owner-supplied source fact used in a reconciliation
// report. Amounts remain decimal strings to avoid floating point loss.
//
//contract:wire:v2
type BusinessObligation struct {
	//contract:minLength=1
	//contract:maxLength=160
	Key string `json:"key"`
	//contract:minLength=1
	//contract:maxLength=512
	SourceRef string `json:"source_ref"`
	//contract:maxLength=160
	EntityID string `json:"entity_id"`
	//contract:minLength=1
	//contract:maxLength=64
	Amount string `json:"amount"`
	//contract:minLength=1
	//contract:maxLength=16
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
	//contract:maxLength=160
	Lifecycle string `json:"lifecycle,omitempty"`
}

// BusinessEffect is an owner-supplied downstream effect fact.
//
//contract:wire:v2
type BusinessEffect struct {
	//contract:minLength=1
	//contract:maxLength=160
	Key string `json:"key"`
	//contract:minLength=1
	//contract:maxLength=512
	TargetRef string `json:"target_ref"`
	//contract:maxLength=160
	EntityID string `json:"entity_id"`
	//contract:minLength=1
	//contract:maxLength=64
	Amount string `json:"amount"`
	//contract:minLength=1
	//contract:maxLength=16
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
	//contract:maxLength=160
	Lifecycle string `json:"lifecycle,omitempty"`
}

// BusinessFinding is redacted, provider-qualified reconciliation evidence.
//
//contract:wire:v2
type BusinessFinding struct {
	//contract:minLength=1
	//contract:maxLength=160
	ID   string              `json:"id"`
	Kind BusinessFindingKind `json:"kind"`
	//contract:minLength=1
	//contract:maxLength=160
	Key string `json:"key"`
	//contract:minLength=1
	//contract:maxLength=512
	SourceRef string `json:"source_ref"`
	//contract:maxItems=100
	TargetRefs []string             `json:"target_refs,omitempty"`
	Expected   BusinessObligation   `json:"expected"`
	Actual     []BusinessEffect     `json:"actual,omitempty"`
	State      BusinessFindingState `json:"state"`
	//contract:maxBytes=8192
	Evidence string `json:"evidence,omitempty"`
}

// BusinessRunWatermarks pin both source query snapshots.
//
//contract:wire:v2
type BusinessRunWatermarks struct {
	SourceRevision string    `json:"source_revision,omitempty"`
	TargetRevision string    `json:"target_revision,omitempty"`
	SourceSnapshot string    `json:"source_snapshot,omitempty"`
	TargetSnapshot string    `json:"target_snapshot,omitempty"`
	SourceAsOf     time.Time `json:"source_as_of,omitempty"`
	TargetAsOf     time.Time `json:"target_as_of,omitempty"`
}

// BusinessRunCursor preserves the bounded replay state for an incomplete run.
//
//contract:wire:v2
type BusinessRunCursor struct {
	//contract:maxLength=1024
	Source string `json:"source,omitempty"`
	//contract:maxLength=1024
	Target string `json:"target,omitempty"`
	//contract:maxItems=10000
	SourceRows []BusinessObligation `json:"source_rows,omitempty"`
	//contract:maxItems=10000
	TargetRows     []BusinessEffect      `json:"target_rows,omitempty"`
	Watermarks     BusinessRunWatermarks `json:"watermarks,omitempty"`
	SourceComplete bool                  `json:"source_complete,omitempty"`
	TargetComplete bool                  `json:"target_complete,omitempty"`
}

// BusinessRun is an immutable reconciliation result scoped to one tenant and
// business entity.
//
//contract:wire:v2
type BusinessRun struct {
	TenantID      TenantID              `json:"tenant_id"`
	OwnerModuleID ModuleID              `json:"owner_module_id"`
	ID            string                `json:"id"`
	RuleID        string                `json:"rule_id"`
	Version       uint32                `json:"version"`
	Scope         BusinessScope         `json:"scope"`
	Cutoff        time.Time             `json:"cutoff"`
	Complete      bool                  `json:"complete"`
	Cursors       BusinessRunCursor     `json:"cursors,omitempty"`
	Watermarks    BusinessRunWatermarks `json:"watermarks,omitempty"`
	//contract:maxItems=10000
	Findings  []BusinessFinding `json:"findings,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// BusinessReconciliationRunRequest starts or continues a durable report.
//
//contract:wire:v2
type BusinessReconciliationRunRequest struct {
	//contract:minLength=1
	//contract:maxLength=160
	OwnerModuleID ModuleID `json:"owner_module_id"`
	//contract:minLength=1
	//contract:maxLength=160
	RuleID string `json:"rule_id"`
	//contract:minLength=1
	//contract:maxLength=160
	RunID string `json:"run_id"`
	//contract:maxLength=160
	PreviousRunID string `json:"previous_run_id,omitempty"`
}

func (request BusinessReconciliationRunRequest) Validate() error {
	if request.OwnerModuleID == "" || len(request.OwnerModuleID) > 160 || request.RuleID == "" || len(request.RuleID) > 160 || request.RunID == "" || len(request.RunID) > 160 || len(request.PreviousRunID) > 160 {
		return fmt.Errorf("business reconciliation run identity is invalid")
	}
	return nil
}

// BusinessFindingDecision identifies an owner-published reconciliation
// finding to waive or repair.
//
//contract:wire:v2
type BusinessFindingDecision struct {
	//contract:minLength=1
	//contract:maxLength=160
	FindingID string `json:"finding_id"`
	//contract:maxBytes=8192
	Reason string `json:"reason,omitempty"`
}

func (request BusinessFindingDecision) Validate(requireReason bool) error {
	if strings.TrimSpace(request.FindingID) == "" || len(request.FindingID) > 160 || len(request.Reason) > 8192 || requireReason && strings.TrimSpace(request.Reason) == "" {
		return fmt.Errorf("business reconciliation finding decision is invalid")
	}
	return nil
}

//contract:enum:v2
type BusinessFindingAction string

const (
	BusinessFindingActionWaived   BusinessFindingAction = "waived"
	BusinessFindingActionRepaired BusinessFindingAction = "repaired"
)

// BusinessFindingActionResult confirms one accepted owner-governed action.
//
//contract:wire:v2
type BusinessFindingActionResult struct {
	FindingID string                `json:"finding_id"`
	Action    BusinessFindingAction `json:"action"`
}

// BusinessReconciliationSummary is the exact-scope unresolved finding count.
//
//contract:wire:v2
type BusinessReconciliationSummary struct {
	Scope                      BusinessScope `json:"scope"`
	UnresolvedBusinessFindings uint32        `json:"unresolved_business_findings"`
}
