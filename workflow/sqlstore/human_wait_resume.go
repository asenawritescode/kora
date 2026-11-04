package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

const maxHumanWaitResumeBatch = 500

// ResumeTerminalHumanWaits advances a bounded batch of paused instances whose
// source request is terminal. It polls durable request state, so it also
// recovers when outbox delivery or a worker was unavailable. The request,
// pinned definition, waiting step, history, and event are resolved in one
// transaction; conditional updates make retries harmless.
func (s *ExecutionStore) ResumeTerminalHumanWaits(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return 0, fmt.Errorf("workflow execution store is not configured")
	}
	if ctx == nil {
		return 0, fmt.Errorf("human wait recovery requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return 0, err
	}
	if limit == 0 || limit > maxHumanWaitResumeBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "human wait recovery requires a batch limit from 1 to 500")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin human wait recovery: %w", err)
	}
	defer tx.Rollback()
	query := `SELECT i.id,i.current_step,i.revision,i.definition_id,i.definition_version,r.id,r.status,r.outcome,r.revision FROM _kora_workflow_instance i JOIN _kora_request r ON r.tenant_id=i.tenant_id AND r.source_instance_id=i.id AND r.source_step_id=i.current_step WHERE i.tenant_id=? AND i.status='waiting' AND r.status IN ('resolved','expired','cancelled','stale') ORDER BY r.updated_at,i.id LIMIT ?`
	if s.dialect.DriverName() == "mysql" {
		query += " FOR UPDATE SKIP LOCKED"
	} else if s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE OF i SKIP LOCKED"
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), int64(limit))
	if err != nil {
		return 0, fmt.Errorf("select terminal human waits: %w", err)
	}
	type candidate struct {
		instanceID, stepID, definitionID, requestID          string
		instanceRevision, definitionVersion, requestRevision int64
		requestStatus                                        string
		outcome                                              sql.NullString
	}
	items := make([]candidate, 0, limit)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.instanceID, &item.stepID, &item.instanceRevision, &item.definitionID, &item.definitionVersion, &item.requestID, &item.requestStatus, &item.outcome, &item.requestRevision); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan terminal human wait: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read terminal human waits: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close terminal human waits: %w", err)
	}
	advanced := uint16(0)
	for _, item := range items {
		if item.instanceRevision < 1 || item.requestRevision < 1 || item.definitionVersion < 1 {
			return 0, contract.NewError(contract.CodeInternal, "stored human wait revision is invalid")
		}
		var digest string
		var body []byte
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest,definition_json FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?`), string(tenant), item.definitionID, item.definitionVersion).Scan(&digest, &body); err != nil {
			return 0, fmt.Errorf("load pinned human wait definition: %w", err)
		}
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal(body, &definition); err != nil {
			return 0, fmt.Errorf("decode pinned human wait definition: %w", err)
		}
		computedDigest, err := workflow.DefinitionDigest(definition)
		if err != nil {
			return 0, fmt.Errorf("digest pinned human wait definition: %w", err)
		}
		if computedDigest != digest || definition.ID != contract.DefinitionID(item.definitionID) || int64(definition.Version) != item.definitionVersion {
			return 0, contract.NewError(contract.CodeInternal, "pinned human wait definition failed integrity validation")
		}
		step, ok := definitionStep(definition, contract.StepID(item.stepID))
		if !ok || step.Kind != contract.StepHuman || step.Human == nil {
			return 0, contract.NewError(contract.CodeInternal, "waiting instance does not point to a human step")
		}
		next, err := humanWaitOutcome(step.Human, item.requestStatus, item.outcome)
		if err != nil {
			return 0, err
		}
		var sequence uint64
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(tenant), item.instanceID).Scan(&sequence); err != nil {
			return 0, fmt.Errorf("allocate human wait history sequence: %w", err)
		}
		stamp := "CURRENT_TIMESTAMP"
		if s.dialect.DriverName() == "mysql" {
			stamp = "CURRENT_TIMESTAMP(6)"
		}
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status='ready',current_step=?,revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND current_step=? AND status='waiting' AND revision=?`), string(next), string(tenant), item.instanceID, item.stepID, item.instanceRevision)
		if err != nil {
			return 0, fmt.Errorf("advance terminal human wait: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count resumed human waits: %w", err)
		}
		if changed == 0 { // A concurrent recovery already advanced it.
			continue
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), item.instanceID, item.stepID); err != nil {
			return 0, fmt.Errorf("clear resolved human wait timer: %w", err)
		}
		detail, _ := json.Marshal(struct {
			RequestID       string `json:"request_id"`
			RequestStatus   string `json:"request_status"`
			RequestRevision int64  `json:"request_revision"`
		}{item.requestID, item.requestStatus, item.requestRevision})
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,'ready',NULL,?,`+stamp+`)`), string(tenant), item.instanceID, sequence, item.stepID, "human_request_resolved", string(detail)); err != nil {
			return 0, fmt.Errorf("record human wait resolution: %w", err)
		}
		data, err := json.Marshal(contract.WorkflowEventData{InstanceID: contract.InstanceID(item.instanceID), Revision: contract.Revision(fmt.Sprint(item.instanceRevision + 1)), ChangeKind: "human_request_resolved"})
		if err != nil {
			return 0, fmt.Errorf("encode human wait event: %w", err)
		}
		event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.human_request_resolved", Version: contract.CurrentVersion, Source: "workflow", Site: string(tenant), AggregateType: "workflow_instance", AggregateID: item.instanceID, OccurredAt: time.Now().UTC(), Data: data}
		if err := event.Validate(); err != nil {
			return 0, fmt.Errorf("validate human wait event: %w", err)
		}
		if err := s.events.Append(ctx, tx, event); err != nil {
			return 0, fmt.Errorf("append human wait resolution event: %w", err)
		}
		advanced++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit human wait recovery: %w", err)
	}
	return advanced, nil
}

func definitionStep(definition contract.WorkflowDefinition, id contract.StepID) (contract.StepDefinition, bool) {
	for _, step := range definition.Steps {
		if step.ID == id {
			return step, true
		}
	}
	return contract.StepDefinition{}, false
}

func humanWaitOutcome(spec *contract.HumanSpec, status string, outcome sql.NullString) (contract.StepID, error) {
	if spec == nil {
		return "", contract.NewError(contract.CodeInternal, "human wait has no specification")
	}
	switch contract.RequestStatus(status) {
	case contract.RequestResolved:
		if !outcome.Valid {
			return "", contract.NewError(contract.CodeInternal, "resolved human request has no outcome")
		}
		var response contract.ResponseKind
		switch contract.RequestOutcome(outcome.String) {
		case contract.OutcomeApproved:
			response = contract.ResponseApprove
		case contract.OutcomeRejected:
			response = contract.ResponseReject
		case contract.OutcomeReturned:
			response = contract.ResponseReturn
		case contract.OutcomeProvided:
			response = contract.ResponseProvide
		case contract.OutcomeAcknowledged:
			response = contract.ResponseAcknowledge
		case contract.OutcomeCompleted:
			response = contract.ResponseComplete
		default:
			return "", contract.NewError(contract.CodeInternal, "resolved human request has an unknown outcome")
		}
		next := spec.Outcomes[response]
		if next == "" {
			return "", contract.NewError(contract.CodeInternal, "human step has no edge for the resolved request outcome")
		}
		return next, nil
	case contract.RequestExpired:
		return requiredHumanEdge(spec.OnExpired, "expired")
	case contract.RequestCancelled:
		return requiredHumanEdge(spec.OnCancelled, "cancelled")
	case contract.RequestStale:
		return requiredHumanEdge(spec.OnStale, "stale")
	default:
		return "", contract.NewError(contract.CodeInternal, "human wait request has an unsupported terminal state")
	}
}

func requiredHumanEdge(step contract.StepID, state string) (contract.StepID, error) {
	if step == "" {
		return "", contract.NewError(contract.CodeInternal, "human step has no edge for the "+state+" request state")
	}
	return step, nil
}
