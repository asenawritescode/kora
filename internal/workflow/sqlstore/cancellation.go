package sqlstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/workflow"
)

var _ workflow.CancellationRepository = (*InstanceStore)(nil)

func (s *InstanceStore) Cancel(ctx context.Context, change workflow.CancelCommit) (contract.WorkflowInstance, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil {
		return contract.WorkflowInstance{}, fmt.Errorf("workflow cancellation store is not configured")
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
	if _, err := contract.ParseIdempotencyKey(string(change.IdempotencyKey)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if len(change.Reason) == 0 || len(change.Reason) > 8192 {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeValidationFailed, "workflow cancellation reason must be 1 to 8192 bytes")
	}
	command, err := json.Marshal(struct {
		Instance contract.InstanceID
		Revision contract.Revision
		Reason   string
	}{change.InstanceID, change.ExpectedRevision, change.Reason})
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	digest := sha256.Sum256(command)
	digestText := hex.EncodeToString(digest[:])
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("begin workflow cancellation: %w", err)
	}
	defer tx.Rollback()
	lookup := `SELECT command_digest,result_json FROM _kora_workflow_cancel_receipt WHERE tenant_id=? AND actor_id=? AND idempotency_key=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		lookup += " FOR UPDATE"
	}
	var previousDigest string
	var previousResult []byte
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, lookup), string(change.Tenant), string(change.Actor), string(change.IdempotencyKey)).Scan(&previousDigest, &previousResult)
	if err == nil {
		if previousDigest != digestText {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow cancellation key was reused with different content")
		}
		var instance contract.WorkflowInstance
		if err := json.Unmarshal(previousResult, &instance); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("decode workflow cancellation receipt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("commit workflow cancellation replay: %w", err)
		}
		return instance, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contract.WorkflowInstance{}, fmt.Errorf("read workflow cancellation receipt: %w", err)
	}
	selectInstance := `SELECT current_step,status,revision FROM _kora_workflow_instance WHERE tenant_id=? AND id=?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		selectInstance += " FOR UPDATE"
	}
	var stepID, status, revisionText string
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, selectInstance), string(change.Tenant), string(change.InstanceID)).Scan(&stepID, &status, &revisionText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeNotFound, "workflow instance not found")
		}
		return contract.WorkflowInstance{}, fmt.Errorf("lock workflow instance for cancellation: %w", err)
	}
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || fmt.Sprint(revision) != string(change.ExpectedRevision) {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow instance revision changed before cancellation")
	}
	if status != string(contract.InstanceReady) && status != string(contract.InstanceRetrying) && status != string(contract.InstanceWaiting) {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "running or uncertain workflow cannot be cancelled safely")
	}
	if status == string(contract.InstanceWaiting) {
		var pending int
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_request WHERE tenant_id=? AND source_instance_id=? AND source_step_id=? AND status=?`), string(change.Tenant), string(change.InstanceID), stepID, string(contract.RequestPending)).Scan(&pending); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("check pending human request before workflow cancellation: %w", err)
		}
		if pending > 0 {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "cancel the pending human request before cancelling its workflow")
		}
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status=?,revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND revision=? AND status=?`), string(contract.InstanceCancelled), string(change.Tenant), string(change.InstanceID), revision, status)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("cancel workflow instance: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if changed != 1 {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow instance changed during cancellation")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=?`), string(change.Tenant), string(change.InstanceID)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("clear workflow timers during cancellation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_subscription WHERE tenant_id=? AND instance_id=?`), string(change.Tenant), string(change.InstanceID)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("clear workflow subscriptions during cancellation: %w", err)
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(change.Tenant), string(change.InstanceID)).Scan(&sequence); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("allocate workflow cancellation history: %w", err)
	}
	stampExpr := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stampExpr = "CURRENT_TIMESTAMP(6)"
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,?,?,?,`+stampExpr+`)`), string(change.Tenant), string(change.InstanceID), sequence, stepID, "cancelled", string(contract.InstanceCancelled), string(change.Actor), change.Reason); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("record workflow cancellation history: %w", err)
	}
	eventData, err := json.Marshal(contract.WorkflowEventData{InstanceID: change.InstanceID, Revision: contract.Revision(strconv.FormatUint(revision+1, 10)), ChangeKind: "cancelled"})
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.cancelled", Version: 1, Source: "workflow", Site: change.Site, AggregateType: "workflow_instance", AggregateID: string(change.InstanceID), OccurredAt: s.nowUTC(), Data: eventData}
	if err := s.events.Append(ctx, tx, event); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("append workflow cancellation event: %w", err)
	}
	instance, err := getInstanceTx(ctx, tx, s.dialect, change.Tenant, change.InstanceID)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	resultJSON, err := json.Marshal(instance)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_cancel_receipt (tenant_id,actor_id,idempotency_key,command_digest,result_json,created_at) VALUES (?,?,?,?,?,`+stampExpr+`)`), string(change.Tenant), string(change.Actor), string(change.IdempotencyKey), digestText, string(resultJSON)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("record workflow cancellation receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("commit workflow cancellation: %w", err)
	}
	return instance, nil
}

func (s *InstanceStore) nowUTC() time.Time { return time.Now().UTC() }
