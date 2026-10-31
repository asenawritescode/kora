package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

const maxTimedWaitResumeBatch uint16 = 500

// ResumeDueTimedWaits advances due generic wait steps to their timeout edge.
// It verifies the immutable definition and commits the fenced state transition,
// timer deletion, history, and event in one tenant transaction. Human steps
// are left to request terminal-state recovery.
func (s *ExecutionStore) ResumeDueTimedWaits(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return 0, fmt.Errorf("workflow execution store is not configured")
	}
	if ctx == nil {
		return 0, fmt.Errorf("timed wait recovery requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return 0, err
	}
	if limit == 0 || limit > maxTimedWaitResumeBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "timed wait recovery batch must be from 1 to 500")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin timed wait recovery: %w", err)
	}
	defer tx.Rollback()
	nowExpr := s.dialect.NowTimestamp()
	query := `SELECT i.id,i.current_step,i.revision,i.definition_id,i.definition_version,` + nowExpr + ` FROM _kora_workflow_instance i JOIN _kora_workflow_process_timer t ON t.tenant_id=i.tenant_id AND t.instance_id=i.id AND t.step_id=i.current_step WHERE i.tenant_id=? AND i.status='waiting' AND t.due_at<=` + nowExpr + ` AND NOT EXISTS (SELECT 1 FROM _kora_request r WHERE r.tenant_id=i.tenant_id AND r.source_instance_id=i.id AND r.source_step_id=i.current_step) ORDER BY t.due_at,i.id LIMIT ?`
	if s.dialect.DriverName() == "mysql" {
		query += " FOR UPDATE SKIP LOCKED"
	} else if s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE OF i SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), int64(limit))
	if err != nil {
		return 0, fmt.Errorf("select due workflow timers: %w", err)
	}
	type candidate struct {
		instanceID, stepID, definitionID string
		revision, version                int64
		databaseNow                      db.Timestamp
	}
	items := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.instanceID, &item.stepID, &item.revision, &item.definitionID, &item.version, &item.databaseNow); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan due workflow timer: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read due workflow timers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close due workflow timers: %w", err)
	}
	resumed := uint16(0)
	for _, item := range items {
		if item.revision < 1 || item.version < 1 {
			return 0, contract.NewError(contract.CodeInternal, "stored timed wait identity is invalid")
		}
		var digest string
		var body []byte
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest,definition_json FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?`), string(tenant), item.definitionID, item.version).Scan(&digest, &body); err != nil {
			return 0, fmt.Errorf("load pinned timed wait definition: %w", err)
		}
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal(body, &definition); err != nil {
			return 0, fmt.Errorf("decode pinned timed wait definition: %w", err)
		}
		computedDigest, err := workflow.DefinitionDigest(definition)
		if err != nil {
			return 0, fmt.Errorf("digest pinned timed wait definition: %w", err)
		}
		if computedDigest != digest || definition.ID != contract.DefinitionID(item.definitionID) || int64(definition.Version) != item.version {
			return 0, contract.NewError(contract.CodeInternal, "pinned timed wait definition failed integrity validation")
		}
		step, ok := definitionStep(definition, contract.StepID(item.stepID))
		if !ok || step.Kind != contract.StepWait || step.Wait == nil {
			return 0, contract.NewError(contract.CodeInternal, "due workflow timer does not point to a wait or human step")
		}
		if step.Wait.OnTimeout == "" {
			return 0, contract.NewError(contract.CodeInternal, "pinned workflow wait has no timeout destination")
		}
		var sequence uint64
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(tenant), item.instanceID).Scan(&sequence); err != nil {
			return 0, fmt.Errorf("allocate timed wait history sequence: %w", err)
		}
		stamp := "CURRENT_TIMESTAMP"
		if s.dialect.DriverName() == "mysql" {
			stamp = "CURRENT_TIMESTAMP(6)"
		}
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status='ready',current_step=?,revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND current_step=? AND status='waiting' AND revision=?`), string(step.Wait.OnTimeout), string(tenant), item.instanceID, item.stepID, item.revision)
		if err != nil {
			return 0, fmt.Errorf("resume timed workflow wait: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count resumed timed workflow waits: %w", err)
		}
		if changed == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), item.instanceID, item.stepID); err != nil {
			return 0, fmt.Errorf("delete resumed workflow timer: %w", err)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_subscription WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), item.instanceID, item.stepID); err != nil {
			return 0, fmt.Errorf("delete timed-out workflow event subscription: %w", err)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,'ready',NULL,?,`+stamp+`)`), string(tenant), item.instanceID, sequence, item.stepID, "timeout", string(step.Wait.OnTimeout)); err != nil {
			return 0, fmt.Errorf("record workflow timeout: %w", err)
		}
		data, err := json.Marshal(contract.WorkflowEventData{InstanceID: contract.InstanceID(item.instanceID), Revision: contract.Revision(fmt.Sprint(item.revision + 1)), ChangeKind: "timeout"})
		if err != nil {
			return 0, fmt.Errorf("encode workflow timeout event: %w", err)
		}
		event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.timeout", Version: contract.CurrentVersion, Source: "workflow", Site: string(tenant), AggregateType: "workflow_instance", AggregateID: item.instanceID, OccurredAt: item.databaseNow.UTC(), Data: data}
		if err := event.Validate(); err != nil {
			return 0, fmt.Errorf("validate workflow timeout event: %w", err)
		}
		if err := s.events.Append(ctx, tx, event); err != nil {
			return 0, fmt.Errorf("append workflow timeout event: %w", err)
		}
		resumed++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit timed wait recovery: %w", err)
	}
	return resumed, nil
}

var _ workflow.TimedWaitRecoveryStore = (*ExecutionStore)(nil)
