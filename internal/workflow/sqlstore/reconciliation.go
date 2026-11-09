package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

var _ workflow.ReconciliationRepository = (*InstanceStore)(nil)

// Reconcile applies an authorized operator decision to an uncertain action.
// The command receipt, state/fence transition, immutable history and event are
// written in one transaction. Replays return the original result snapshot.
func (s *InstanceStore) Reconcile(ctx context.Context, change workflow.ReconcileCommit) (contract.WorkflowInstance, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return contract.WorkflowInstance{}, fmt.Errorf("workflow instance store is not configured")
	}
	if ctx == nil {
		return contract.WorkflowInstance{}, fmt.Errorf("workflow reconciliation requires context")
	}
	if _, err := contract.ParseTenantID(string(change.Tenant)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParseInstanceID(string(change.InstanceID)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParsePrincipalID(string(change.Actor)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParseRevision(string(change.ExpectedRevision)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParseStepID(string(change.StepID)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParseIdempotencyKey(string(change.IdempotencyKey)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if len(change.EvidenceRef) == 0 || len(change.EvidenceRef) > 512 || strings.TrimSpace(change.EvidenceRef) != change.EvidenceRef || strings.ContainsAny(change.EvidenceRef, "\r\n\x00") || len(change.Reason) == 0 || len(change.Reason) > 8192 || strings.TrimSpace(change.Reason) == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation evidence and reason are required and bounded")
	}
	if (change.Status != contract.InstanceReady && change.Status != contract.InstanceFailed) || change.NextStep == "" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation transition is invalid")
	}
	if _, err := contract.ParseStepID(string(change.NextStep)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if (change.Outcome == contract.ReconcileAbandon) != (change.Status == contract.InstanceFailed) || (change.Outcome == contract.ReconcileSucceeded && change.NextStep == change.StepID) || (change.Outcome == contract.ReconcileNotApplied && change.NextStep != change.StepID) || (change.Outcome != contract.ReconcileAbandon && change.Outcome != contract.ReconcileSucceeded && change.Outcome != contract.ReconcileNotApplied) {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "reconciliation outcome and transition do not agree")
	}
	commandJSON, err := json.Marshal(struct {
		InstanceID  contract.InstanceID
		Revision    contract.Revision
		StepID      contract.StepID
		Outcome     contract.ReconciliationOutcome
		EvidenceRef string
		Reason      string
		NextStep    contract.StepID
		Status      contract.InstanceStatus
	}{change.InstanceID, change.ExpectedRevision, change.StepID, change.Outcome, change.EvidenceRef, change.Reason, change.NextStep, change.Status})
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("encode workflow reconciliation command: %w", err)
	}
	sum := sha256.Sum256(commandJSON)
	commandDigest := hex.EncodeToString(sum[:])
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("begin workflow reconciliation: %w", err)
	}
	defer tx.Rollback()
	lookup := `SELECT command_digest,result_json FROM _kora_workflow_reconcile_receipt WHERE tenant_id=? AND actor_id=? AND idempotency_key=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		lookup += " FOR UPDATE"
	}
	var previousDigest string
	var previousResult []byte
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, lookup), string(change.Tenant), string(change.Actor), string(change.IdempotencyKey)).Scan(&previousDigest, &previousResult)
	if err == nil {
		if previousDigest != commandDigest {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "reconciliation key was reused with different content")
		}
		var instance contract.WorkflowInstance
		if err := json.Unmarshal(previousResult, &instance); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("decode workflow reconciliation receipt: %w", err)
		}
		if err := validateReconcileReceiptResult(instance, change); err != nil {
			return contract.WorkflowInstance{}, err
		}
		if err := tx.Commit(); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("commit workflow reconciliation replay: %w", err)
		}
		return instance, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contract.WorkflowInstance{}, fmt.Errorf("read workflow reconciliation receipt: %w", err)
	}
	lock := `SELECT status,current_step,revision FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		lock += " FOR UPDATE"
	}
	var status, step string
	var revision uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, lock), string(change.Tenant), string(change.InstanceID)).Scan(&status, &step, &revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeNotFound, "workflow instance not found")
		}
		return contract.WorkflowInstance{}, fmt.Errorf("lock uncertain workflow instance: %w", err)
	}
	if status != string(contract.InstanceUncertain) || step != string(change.StepID) || fmt.Sprint(revision) != string(change.ExpectedRevision) {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow instance changed before reconciliation")
	}
	var executionID, attemptStatus string
	var attempt uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT execution_id,attempt,status FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=? ORDER BY attempt DESC LIMIT 1`), string(change.Tenant), string(change.InstanceID), string(change.StepID)).Scan(&executionID, &attempt, &attemptStatus); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("load uncertain workflow action attempt: %w", err)
	}
	if attemptStatus != "outcome_unknown" {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow action attempt does not have an unresolved external outcome")
	}
	resolvedAttemptStatus := "abandoned"
	switch change.Outcome {
	case contract.ReconcileSucceeded:
		resolvedAttemptStatus = "succeeded"
	case contract.ReconcileNotApplied:
		resolvedAttemptStatus = "failed"
	}
	attemptResult, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_attempt SET status=? WHERE tenant_id=? AND instance_id=? AND step_id=? AND execution_id=? AND attempt=? AND status='outcome_unknown'`), resolvedAttemptStatus, string(change.Tenant), string(change.InstanceID), string(change.StepID), executionID, attempt)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("resolve uncertain workflow action attempt: %w", err)
	}
	attemptChanged, err := attemptResult.RowsAffected()
	if err != nil || attemptChanged != 1 {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow action attempt changed during reconciliation")
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status=?,current_step=?,revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND status=? AND current_step=? AND revision=?`), string(change.Status), string(change.NextStep), string(change.Tenant), string(change.InstanceID), string(contract.InstanceUncertain), string(change.StepID), revision)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("apply workflow reconciliation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow instance changed during reconciliation")
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(change.Tenant), string(change.InstanceID)).Scan(&sequence); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("allocate reconciliation history sequence: %w", err)
	}
	detail, err := json.Marshal(struct {
		Outcome     contract.ReconciliationOutcome `json:"outcome"`
		EvidenceRef string                         `json:"evidence_ref"`
		Reason      string                         `json:"reason"`
	}{change.Outcome, change.EvidenceRef, change.Reason})
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("encode reconciliation history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,?,?,?,`+stamp+`)`), string(change.Tenant), string(change.InstanceID), sequence, string(change.StepID), "reconciled", string(change.Status), string(change.Actor), string(detail)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("append workflow reconciliation history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=?`), string(change.Tenant), string(change.InstanceID)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("clear workflow timer after reconciliation: %w", err)
	}
	instance, err := getInstanceTx(ctx, tx, s.dialect, change.Tenant, change.InstanceID)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("load reconciled workflow instance: %w", err)
	}
	resultJSON, err := json.Marshal(instance)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("encode reconciliation receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_reconcile_receipt (tenant_id,actor_id,idempotency_key,command_digest,result_json,created_at) VALUES (?,?,?,?,?,`+stamp+`)`), string(change.Tenant), string(change.Actor), string(change.IdempotencyKey), commandDigest, string(resultJSON)); err != nil {
		_ = tx.Rollback()
		if stored, found, lookupErr := s.lookupReconcileReceipt(ctx, change, commandDigest); found || lookupErr != nil {
			return stored, lookupErr
		}
		return contract.WorkflowInstance{}, fmt.Errorf("insert workflow reconciliation receipt: %w", err)
	}
	eventData, err := json.Marshal(contract.WorkflowEventData{InstanceID: change.InstanceID, Revision: contract.Revision(fmt.Sprint(revision + 1)), ChangeKind: "reconciled"})
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("encode workflow reconciliation event: %w", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.reconciled", Version: contract.CurrentVersion, Source: "workflow", Site: string(change.Tenant), AggregateType: "workflow_instance", AggregateID: string(change.InstanceID), OccurredAt: time.Now().UTC(), Data: eventData}
	if err := event.Validate(); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("validate workflow reconciliation event: %w", err)
	}
	if err := s.events.Append(ctx, tx, event); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("append workflow reconciliation event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if stored, found, lookupErr := s.lookupReconcileReceipt(ctx, change, commandDigest); found || lookupErr != nil {
			return stored, lookupErr
		}
		return contract.WorkflowInstance{}, fmt.Errorf("commit workflow reconciliation: %w", err)
	}
	return instance, nil
}

func (s *InstanceStore) lookupReconcileReceipt(ctx context.Context, change workflow.ReconcileCommit, digest string) (contract.WorkflowInstance, bool, error) {
	var storedDigest string
	var result []byte
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest,result_json FROM _kora_workflow_reconcile_receipt WHERE tenant_id=? AND actor_id=? AND idempotency_key=?`), string(change.Tenant), string(change.Actor), string(change.IdempotencyKey)).Scan(&storedDigest, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.WorkflowInstance{}, false, nil
	}
	if err != nil {
		return contract.WorkflowInstance{}, false, fmt.Errorf("resolve workflow reconciliation receipt: %w", err)
	}
	if storedDigest != digest {
		return contract.WorkflowInstance{}, true, contract.NewError(contract.CodeConflict, "reconciliation key was reused with different content")
	}
	var instance contract.WorkflowInstance
	if err := json.Unmarshal(result, &instance); err != nil {
		return contract.WorkflowInstance{}, true, fmt.Errorf("decode workflow reconciliation receipt: %w", err)
	}
	if err := validateReconcileReceiptResult(instance, change); err != nil {
		return contract.WorkflowInstance{}, true, err
	}
	return instance, true, nil
}

func validateReconcileReceiptResult(instance contract.WorkflowInstance, change workflow.ReconcileCommit) error {
	if err := validatePersistedInstance(instance); err != nil {
		return fmt.Errorf("validate workflow reconciliation receipt: %w", err)
	}
	if instance.ID != change.InstanceID || instance.Revision != contract.Revision(fmt.Sprint(mustRevision(change.ExpectedRevision)+1)) || instance.Status != change.Status || instance.CurrentStep != change.NextStep {
		return contract.NewError(contract.CodeInternal, "workflow reconciliation receipt does not match its command")
	}
	return nil
}
