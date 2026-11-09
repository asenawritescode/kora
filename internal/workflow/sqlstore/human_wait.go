package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/asenawritescode/kora/internal/workflow"
)

// CommitHumanWaitInTx creates the request and pauses the claimed workflow in
// the caller-owned transaction. Any error requires the caller to roll back;
// neither request nor workflow store owns the transaction lifecycle.
func (s *ExecutionStore) CommitHumanWaitInTx(ctx context.Context, tx *sql.Tx, prepared *requests.PreparedSubmission, change workflow.StepCommit) (requests.RequestRecord, error) {
	if s == nil || s.database == nil || s.dialect == nil || tx == nil || prepared == nil {
		return requests.RequestRecord{}, fmt.Errorf("human wait requires a configured execution store, prepared request, and transaction")
	}
	if err := s.validateClaim(change.Claim, 0); err != nil {
		return requests.RequestRecord{}, err
	}
	if change.Status != contract.InstanceWaiting || change.NextStep != change.Claim.StepID || change.NextWakeAt == nil {
		return requests.RequestRecord{}, fmt.Errorf("human wait must retain its claimed step and have a deadline")
	}
	request, err := prepared.CommitInTx(ctx, tx)
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("create workflow human request: %w", err)
	}
	if request.Tenant != change.Claim.Tenant || request.Source == nil || request.Source.InstanceID != change.Claim.InstanceID || request.Source.StepID != change.Claim.StepID || request.DueAt == nil || !request.DueAt.Equal(change.NextWakeAt.UTC()) {
		return requests.RequestRecord{}, contract.NewError(contract.CodeConflict, "prepared request source or deadline does not match the claimed workflow wait")
	}
	resultData, err := json.Marshal(struct {
		RequestID contract.RequestID `json:"request_id"`
	}{RequestID: request.ID})
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("encode workflow human wait receipt: %w", err)
	}
	change.Result = workflow.ActionResult{Data: resultData}
	if err := s.CommitStepInTx(ctx, tx, change); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("pause workflow at human step: %w", err)
	}
	return request, nil
}

// CreateHumanWait owns a local tenant transaction for the request and wait
// registration. Both records and their outbox events commit or roll back
// together.
func (s *ExecutionStore) CreateHumanWait(ctx context.Context, prepared *requests.PreparedSubmission, change workflow.StepCommit) (requests.RequestRecord, error) {
	if s == nil || s.database == nil {
		return requests.RequestRecord{}, fmt.Errorf("workflow execution store is not configured")
	}
	if ctx == nil {
		return requests.RequestRecord{}, fmt.Errorf("human wait requires a context")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return requests.RequestRecord{}, fmt.Errorf("begin workflow human wait: %w", err)
	}
	defer tx.Rollback()
	request, err := s.CommitHumanWaitInTx(ctx, tx, prepared, change)
	if err != nil {
		return requests.RequestRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return requests.RequestRecord{}, fmt.Errorf("commit workflow human wait: %w", err)
	}
	return request, nil
}
