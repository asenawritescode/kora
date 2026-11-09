package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/workflow"
)

type ExecutionStore struct {
	database *sql.DB
	dialect  db.Dialect
	events   outbox.Writer
}

func NewExecutionStore(database *sql.DB, dialect db.Dialect) (*ExecutionStore, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("workflow execution store requires database and dialect")
	}
	if _, err := ExecutionSchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &ExecutionStore{database: database, dialect: dialect, events: outbox.NewSQLWriter(dialect)}, nil
}

var _ workflow.ExecutionStore = (*ExecutionStore)(nil)

// BeginExternalAttempt records ownership and retry identity before a provider
// call. If the process dies after the effect starts, recovery can distinguish
// that uncertain outcome from an attempt that was never sent.
func (s *ExecutionStore) BeginExternalAttempt(ctx context.Context, claim workflow.Claim, executionID string, key contract.IdempotencyKey, requested *contract.PinnedDispatch) (*contract.PinnedDispatch, error) {
	if requested != nil {
		return nil, contract.NewError(contract.CodeValidationFailed, "pinned capability attempts require a durable lease")
	}
	_, err := s.beginExternalAttempt(ctx, claim, executionID, key, nil, nil)
	return nil, err
}

func (s *ExecutionStore) BeginPinnedExternalAttempt(ctx context.Context, claim workflow.Claim, executionID string, key contract.IdempotencyKey, requested contract.PinnedDispatch, lease contract.CapabilityLease) (*workflow.PinnedCapabilityAttempt, error) {
	return s.beginExternalAttempt(ctx, claim, executionID, key, &requested, &lease)
}

func (s *ExecutionStore) beginExternalAttempt(ctx context.Context, claim workflow.Claim, executionID string, key contract.IdempotencyKey, requested *contract.PinnedDispatch, requestedLease *contract.CapabilityLease) (*workflow.PinnedCapabilityAttempt, error) {
	if ctx == nil {
		return nil, fmt.Errorf("begin workflow external attempt requires context")
	}
	if err := s.validateClaim(claim, 0); err != nil {
		return nil, err
	}
	if _, err := contract.ParseOperationID(executionID); err != nil {
		return nil, fmt.Errorf("workflow execution identity: %w", err)
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return nil, fmt.Errorf("workflow action idempotency key: %w", err)
	}
	if (requested == nil) != (requestedLease == nil) {
		return nil, contract.NewError(contract.CodeValidationFailed, "pinned dispatch does not match the claimed workflow identity")
	}
	if requested != nil {
		candidate := workflow.PinnedCapabilityAttempt{Dispatch: *requested, Lease: *requestedLease}
		if requested.Validate() != nil || requested.ExecutionID != executionID || requested.Scope != claim.BusinessScope || requested.IdempotencyKey != key || candidate.Validate() != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "pinned dispatch does not match the claimed workflow identity")
		}
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin external workflow attempt transaction: %w", err)
	}
	defer tx.Rollback()
	now := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		now = "CURRENT_TIMESTAMP(6)"
	}
	query := `SELECT id FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > ` + now
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	}
	var instanceID string
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), claim.Owner, uint64(claim.Fence), mustRevision(claim.Revision)).Scan(&instanceID); err != nil {
		if err == sql.ErrNoRows {
			return nil, contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
		}
		return nil, fmt.Errorf("validate external workflow claim: %w", err)
	}
	var previousAttempt uint64
	var previousKey, previousExecution, previousStatus string
	var previousDispatchJSON sql.NullString
	previousErr := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT attempt,idempotency_key,execution_id,status,pinned_dispatch_json FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1`), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID)).Scan(&previousAttempt, &previousKey, &previousExecution, &previousStatus, &previousDispatchJSON)
	if previousErr != nil && previousErr != sql.ErrNoRows {
		return nil, fmt.Errorf("load previous external workflow attempt: %w", previousErr)
	}
	if uint64(claim.Attempt) != previousAttempt+1 {
		return nil, contract.NewError(contract.CodeConflict, "external workflow attempt is not the next durable attempt")
	}
	var selected *workflow.PinnedCapabilityAttempt
	if requested != nil {
		selected = &workflow.PinnedCapabilityAttempt{Dispatch: *requested, Lease: *requestedLease}
	}
	if previousErr == nil {
		if previousStatus != "failed" {
			return nil, contract.NewError(contract.CodeConflict, "previous external workflow outcome is unresolved")
		}
		if previousKey != string(key) || previousExecution != executionID {
			return nil, contract.NewError(contract.CodeConflict, "workflow action idempotency or execution identity changed across attempts")
		}
		if previousDispatchJSON.Valid {
			var prior workflow.PinnedCapabilityAttempt
			if err := json.Unmarshal([]byte(previousDispatchJSON.String), &prior); err != nil || prior.Validate() != nil {
				return nil, contract.NewError(contract.CodeConflict, "persisted pinned capability dispatch is invalid")
			}
			if requested == nil || !samePinnedOperation(prior.Dispatch, *requested) {
				return nil, contract.NewError(contract.CodeConflict, "pinned capability operation or input changed across attempts")
			}
			selected = &prior
		} else if requested != nil {
			return nil, contract.NewError(contract.CodeConflict, "previous attempt has no pinned capability identity")
		}
	}
	var pinnedJSON any
	if selected != nil {
		if err := selected.Validate(); err != nil {
			return nil, contract.NewError(contract.CodeValidationFailed, "pinned capability lease does not match its provider dispatch")
		}
		encoded, marshalErr := json.Marshal(selected)
		if marshalErr != nil {
			return nil, marshalErr
		}
		pinnedJSON = string(encoded)
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_attempt (tenant_id,instance_id,step_id,execution_id,attempt,status,idempotency_key,pinned_dispatch_json,result_json,failure,started_at,finished_at) VALUES (?,?,?,?,?,'running',?,?,NULL,'',`+now+`,`+now+`)`), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), executionID, claim.Attempt, string(key), pinnedJSON)
	if err != nil {
		return nil, fmt.Errorf("persist external workflow attempt start: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit external workflow attempt start: %w", err)
	}
	if selected == nil {
		return nil, nil
	}
	copy := *selected
	return &copy, nil
}

func samePinnedOperation(a, b contract.PinnedDispatch) bool {
	return a.ExecutionID == b.ExecutionID && a.Scope == b.Scope && a.CallerModuleID == b.CallerModuleID && a.Operation == b.Operation && a.IdempotencyKey == b.IdempotencyKey && a.InputHash == b.InputHash
}

func (s *ExecutionStore) ClaimDue(ctx context.Context, tenant contract.TenantID, owner string, limit uint16, lease time.Duration) ([]workflow.Claim, error) {
	if err := s.validateClaimRequest(ctx, tenant, owner, limit, lease); err != nil {
		return nil, err
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin workflow claim: %w", err)
	}
	defer tx.Rollback()
	query := `SELECT i.id, i.current_step, i.revision, i.lease_fence, COALESCE((SELECT MAX(a.attempt) FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step),0), (SELECT a.status FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), (SELECT a.execution_id FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), i.business_scope_json FROM _kora_workflow_instance i WHERE i.tenant_id = ? AND i.status IN ('ready','retrying','running') AND (i.next_wake_at IS NULL OR i.next_wake_at <= CURRENT_TIMESTAMP) AND (i.lease_until IS NULL OR i.lease_until <= CURRENT_TIMESTAMP) ORDER BY COALESCE(i.next_wake_at, i.created_at), i.id LIMIT ?`
	if s.dialect.DriverName() == "mysql" {
		query = `SELECT i.id, i.current_step, i.revision, i.lease_fence, COALESCE((SELECT MAX(a.attempt) FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step),0), (SELECT a.status FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), (SELECT a.execution_id FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), i.business_scope_json FROM _kora_workflow_instance i WHERE i.tenant_id = ? AND i.status IN ('ready','retrying','running') AND (i.next_wake_at IS NULL OR i.next_wake_at <= CURRENT_TIMESTAMP(6)) AND (i.lease_until IS NULL OR i.lease_until <= CURRENT_TIMESTAMP(6)) ORDER BY COALESCE(i.next_wake_at, i.created_at), i.id LIMIT ? FOR UPDATE SKIP LOCKED`
	}
	if s.dialect.DriverName() == "postgres" {
		query = `SELECT i.id, i.current_step, i.revision, i.lease_fence, COALESCE((SELECT MAX(a.attempt) FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step),0), (SELECT a.status FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), (SELECT a.execution_id FROM _kora_workflow_attempt a WHERE a.tenant_id = i.tenant_id AND a.instance_id = i.id AND a.step_id = i.current_step ORDER BY a.attempt DESC LIMIT 1), i.business_scope_json FROM _kora_workflow_instance i WHERE i.tenant_id = ? AND i.status IN ('ready','retrying','running') AND (i.next_wake_at IS NULL OR i.next_wake_at <= CURRENT_TIMESTAMP) AND (i.lease_until IS NULL OR i.lease_until <= CURRENT_TIMESTAMP) ORDER BY COALESCE(i.next_wake_at, i.created_at), i.id LIMIT ? FOR UPDATE SKIP LOCKED`
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), limit)
	if err != nil {
		return nil, fmt.Errorf("select due workflows: %w", err)
	}
	type due struct {
		id, step                     string
		lastStatus, lastExecutionID  sql.NullString
		revision, fence, lastAttempt uint64
		firstAttemptAt               time.Time
		businessScopeJSON            sql.NullString
	}
	dueRows := make([]due, 0, limit)
	for rows.Next() {
		var item due
		if err := rows.Scan(&item.id, &item.step, &item.revision, &item.fence, &item.lastAttempt, &item.lastStatus, &item.lastExecutionID, &item.businessScopeJSON); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due workflow: %w", err)
		}
		dueRows = append(dueRows, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate due workflows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range dueRows {
		var raw any
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT MIN(started_at) FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), dueRows[index].id, dueRows[index].step).Scan(&raw); err != nil {
			return nil, fmt.Errorf("load first workflow attempt time: %w", err)
		}
		firstAttemptAt, err := workflowTimestamp(raw)
		if err != nil {
			return nil, fmt.Errorf("parse first workflow attempt time: %w", err)
		}
		dueRows[index].firstAttemptAt = firstAttemptAt
	}
	claims := make([]workflow.Claim, 0, len(dueRows))
	for _, item := range dueRows {
		if item.lastStatus.Valid && item.lastStatus.String == "running" {
			if err := s.markAbandonedExternalAttemptUnknown(ctx, tx, tenant, item.id, item.step, item.lastExecutionID.String, item.lastAttempt, item.revision); err != nil {
				return nil, fmt.Errorf("recover abandoned external workflow attempt: %w", err)
			}
			continue
		}
		if item.lastAttempt >= uint64(^uint8(0)) {
			return nil, fmt.Errorf("workflow step %s on instance %s exhausted attempt number range", item.step, item.id)
		}
		query := s.claimUpdateQuery()
		leaseArg := s.leaseArgument(lease)
		args := []any{owner, leaseArg, string(tenant), item.id, item.revision}
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), args...)
		if err != nil {
			return nil, fmt.Errorf("fence workflow claim: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("count workflow claim update: %w", err)
		}
		if changed != 1 {
			continue
		}
		var businessScope contract.BusinessScope
		var causationDepth uint16
		if item.businessScopeJSON.Valid {
			storedContext, err := decodePersistedWorkflowBusinessContext(item.businessScopeJSON.String, tenant)
			if err != nil {
				return nil, contract.NewError(contract.CodeConflict, "persisted workflow claim business scope is invalid")
			}
			businessScope, causationDepth = storedContext.BusinessScope, storedContext.CausationDepth
		} else {
			businessScope = contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
		}
		claims = append(claims, workflow.Claim{Tenant: tenant, BusinessScope: businessScope, CausationDepth: causationDepth, InstanceID: contract.InstanceID(item.id), StepID: contract.StepID(item.step), Attempt: uint8(item.lastAttempt + 1), Owner: owner, Fence: workflow.Fence(item.fence + 1), LeaseUntil: time.Now().UTC().Add(lease), Revision: contract.Revision(fmt.Sprint(item.revision)), FirstAttemptAt: item.firstAttemptAt})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit workflow claims: %w", err)
	}
	return claims, nil
}

func workflowTimestamp(raw any) (time.Time, error) {
	if raw == nil {
		return time.Time{}, nil
	}
	if value, ok := raw.(time.Time); ok {
		return value.UTC(), nil
	}
	var value string
	switch typed := raw.(type) {
	case string:
		value = typed
	case []byte:
		value = string(typed)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp value %T", raw)
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999999999Z07:00"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q has an unsupported format", value)
}

// markAbandonedExternalAttemptUnknown converts an in-flight external operation
// whose lease has expired into a durable, operator-visible uncertain state.
// It runs in the same transaction as ClaimDue and therefore cannot race a new
// owner into retrying the side effect.
func (s *ExecutionStore) markAbandonedExternalAttemptUnknown(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, instanceID, stepID, executionID string, attempt, revision uint64) error {
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status='outcome_unknown',revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND current_step=? AND revision=? AND status IN ('ready','retrying','running') AND (lease_until IS NULL OR lease_until<=`+stamp+`)`), string(tenant), instanceID, stepID, revision)
	if err != nil {
		return fmt.Errorf("mark workflow instance uncertain: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count uncertain workflow transition: %w", err)
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "workflow changed before abandoned attempt recovery")
	}
	result, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_attempt SET status='outcome_unknown',failure='worker lease expired before external outcome was committed',finished_at=`+stamp+` WHERE tenant_id=? AND instance_id=? AND step_id=? AND execution_id=? AND attempt=? AND status='running'`), string(tenant), instanceID, stepID, executionID, attempt)
	if err != nil {
		return fmt.Errorf("mark external attempt uncertain: %w", err)
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count uncertain external attempt: %w", err)
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "external workflow attempt changed before recovery")
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(tenant), instanceID).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate abandoned-attempt history sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,?,'outcome_unknown',NULL,?,`+stamp+`)`), string(tenant), instanceID, sequence, stepID, "external_attempt_recovered", "Worker lease expired; external effect requires reconciliation"); err != nil {
		return fmt.Errorf("record abandoned-attempt history: %w", err)
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=?`), string(tenant), instanceID); err != nil {
		return fmt.Errorf("clear uncertain workflow timer: %w", err)
	}
	data, err := json.Marshal(contract.WorkflowEventData{InstanceID: contract.InstanceID(instanceID), Revision: contract.Revision(strconv.FormatUint(revision+1, 10)), ChangeKind: "outcome_unknown"})
	if err != nil {
		return fmt.Errorf("encode uncertain workflow event: %w", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.outcome_unknown", Version: contract.CurrentVersion, Source: "workflow", Site: string(tenant), AggregateType: "workflow_instance", AggregateID: instanceID, OccurredAt: time.Now().UTC(), Data: data}
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate uncertain workflow event: %w", err)
	}
	if err := s.events.Append(ctx, tx, event); err != nil {
		return fmt.Errorf("append uncertain workflow event: %w", err)
	}
	return nil
}

func (s *ExecutionStore) Renew(ctx context.Context, claim workflow.Claim, lease time.Duration) (workflow.Claim, error) {
	if ctx == nil {
		return workflow.Claim{}, fmt.Errorf("renew workflow claim requires context")
	}
	if err := s.validateClaim(claim, lease); err != nil {
		return workflow.Claim{}, err
	}
	query := s.leaseUpdateExpression()
	leaseArg := s.leaseArgument(lease)
	stmt := `UPDATE _kora_workflow_instance SET lease_until = ` + query + `, updated_at = CURRENT_TIMESTAMP WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP`
	if s.dialect.DriverName() == "mysql" {
		stmt = `UPDATE _kora_workflow_instance SET lease_until = ` + query + `, updated_at = CURRENT_TIMESTAMP(6) WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > CURRENT_TIMESTAMP(6)`
	}
	args := []any{leaseArg, string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), claim.Owner, uint64(claim.Fence), mustRevision(claim.Revision)}
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, stmt), args...)
	if err != nil {
		return workflow.Claim{}, fmt.Errorf("renew workflow claim: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return workflow.Claim{}, err
	}
	if changed != 1 {
		return workflow.Claim{}, contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
	}
	claim.LeaseUntil = time.Now().UTC().Add(lease)
	return claim, nil
}

func (s *ExecutionStore) Release(ctx context.Context, claim workflow.Claim) error {
	if ctx == nil {
		return fmt.Errorf("release workflow claim requires context")
	}
	if err := s.validateClaim(claim, 0); err != nil {
		return err
	}
	now := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		now = "CURRENT_TIMESTAMP(6)"
	}
	query := `UPDATE _kora_workflow_instance SET status = 'ready', lease_owner = NULL, lease_until = NULL, updated_at = ` + now + ` WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > ` + now
	result, err := s.database.ExecContext(ctx, db.Rebind(s.dialect, query), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), claim.Owner, uint64(claim.Fence), mustRevision(claim.Revision))
	if err != nil {
		return fmt.Errorf("release workflow claim: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
	}
	return nil
}

// CommitStep persists attempt, instance progress, timer state, history and
// required events under the same fenced transaction. It does not execute a
// business mutation; local actions must use LocalActionExecutor.
func (s *ExecutionStore) CommitStep(ctx context.Context, change workflow.StepCommit) error {
	if ctx == nil {
		return fmt.Errorf("workflow step commit requires context")
	}
	if s == nil || s.database == nil {
		return fmt.Errorf("workflow execution store is not configured")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow step commit: %w", err)
	}
	defer tx.Rollback()
	if err := s.CommitStepInTx(ctx, tx, change); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow step: %w", err)
	}
	return nil
}

// CommitStepInTx appends a fenced workflow transition to a caller-owned local
// transaction. It leaves commit and rollback ownership to the caller, allowing
// coordinated request/domain writes to commit with workflow progress.
func (s *ExecutionStore) CommitStepInTx(ctx context.Context, tx *sql.Tx, change workflow.StepCommit) error {
	if s == nil || s.database == nil || s.dialect == nil || tx == nil {
		return fmt.Errorf("workflow step transaction requires a configured store and caller transaction")
	}
	if err := s.validateClaim(change.Claim, 0); err != nil {
		return err
	}
	if ctx == nil {
		return fmt.Errorf("workflow step commit requires context")
	}
	if change.Attempt != change.Claim.Attempt {
		return contract.NewError(contract.CodeConflict, "workflow step attempt does not match its durable claim")
	}
	if change.ExecutionID == "" || change.Attempt == 0 {
		return fmt.Errorf("workflow execution identity and positive attempt are required")
	}
	if _, err := contract.ParseOperationID(change.ExecutionID); err != nil {
		return fmt.Errorf("workflow execution identity: %w", err)
	}
	if _, err := contract.ParseIdempotencyKey(string(change.IdempotencyKey)); err != nil {
		return fmt.Errorf("workflow action idempotency key: %w", err)
	}
	if !validCommitStatus(change.Status) {
		return fmt.Errorf("invalid workflow commit status %q", change.Status)
	}
	terminal := change.Status == contract.InstanceCompleted || change.Status == contract.InstanceCancelled || change.Status == contract.InstanceFailed || change.Status == contract.InstanceUncertain
	if !terminal && change.NextStep == "" {
		return fmt.Errorf("nonterminal workflow commit requires next step")
	}
	if !terminal {
		if _, err := contract.ParseStepID(string(change.NextStep)); err != nil {
			return fmt.Errorf("workflow next step: %w", err)
		}
	}
	if change.EventWait != nil {
		wait := change.EventWait
		if change.Status != contract.InstanceWaiting || change.NextWakeAt == nil || wait.EventType == "" || len(wait.EventType) > 160 || wait.CorrelationField == "" || len(wait.CorrelationField) > 160 || wait.CorrelationValue == "" || len(wait.CorrelationValue) > 512 || wait.OnEvent == "" {
			return contract.NewError(contract.CodeValidationFailed, "workflow event wait subscription is invalid")
		}
		if _, err := contract.ParseStepID(string(wait.OnEvent)); err != nil {
			return fmt.Errorf("workflow event wait destination: %w", err)
		}
	}
	if len(change.Failure) > 8192 {
		return fmt.Errorf("workflow failure detail exceeds 8 KiB")
	}
	if len(change.Result.Data) > 65536 {
		return fmt.Errorf("workflow action result exceeds 64 KiB")
	}
	if len(change.Result.Data) > 0 && !json.Valid(change.Result.Data) {
		return fmt.Errorf("workflow action result is invalid JSON")
	}
	resultJSON, err := json.Marshal(change.Result)
	if err != nil {
		return err
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id = ? AND instance_id = ?`), string(change.Claim.Tenant), string(change.Claim.InstanceID)).Scan(&sequence); err != nil {
		return fmt.Errorf("allocate workflow history sequence: %w", err)
	}
	nextStep := string(change.NextStep)
	if terminal {
		nextStep = string(change.Claim.StepID)
	}
	var wake any
	if change.NextWakeAt != nil {
		wake = change.NextWakeAt.UTC()
	}
	update := `UPDATE _kora_workflow_instance SET status = ?, current_step = ?, revision = revision + 1, next_wake_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = ` + stamp + ` WHERE tenant_id = ? AND id = ? AND current_step = ? AND status = 'running' AND lease_owner = ? AND lease_fence = ? AND revision = ? AND lease_until > ` + stamp
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, update), string(change.Status), nextStep, wake, string(change.Claim.Tenant), string(change.Claim.InstanceID), string(change.Claim.StepID), change.Claim.Owner, uint64(change.Claim.Fence), mustRevision(change.Claim.Revision))
	if err != nil {
		return fmt.Errorf("advance workflow instance: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
	}
	attemptStatus := "succeeded"
	if change.Failure != "" {
		attemptStatus = "failed"
	}
	if change.Status == contract.InstanceUncertain {
		attemptStatus = "outcome_unknown"
	}
	stampExpr := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stampExpr = "CURRENT_TIMESTAMP(6)"
	}
	if change.AttemptStarted {
		var previousKey, previousStatus string
		attemptErr := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT idempotency_key,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? AND execution_id = ? AND attempt = ?`), string(change.Claim.Tenant), string(change.Claim.InstanceID), string(change.Claim.StepID), change.ExecutionID, change.Attempt).Scan(&previousKey, &previousStatus)
		if attemptErr != nil {
			return fmt.Errorf("load started workflow attempt: %w", attemptErr)
		}
		if previousStatus != "running" || previousKey != string(change.IdempotencyKey) {
			return contract.NewError(contract.CodeConflict, "external workflow attempt is not running with the claimed identity")
		}
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_attempt SET status = ?, result_json = ?, failure = ?, finished_at = `+stampExpr+` WHERE tenant_id = ? AND instance_id = ? AND step_id = ? AND execution_id = ? AND attempt = ? AND status = 'running' AND idempotency_key = ?`), attemptStatus, string(resultJSON), change.Failure, string(change.Claim.Tenant), string(change.Claim.InstanceID), string(change.Claim.StepID), change.ExecutionID, change.Attempt, string(change.IdempotencyKey))
		if err != nil {
			return fmt.Errorf("finish external workflow attempt: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return contract.NewError(contract.CodeConflict, "external workflow attempt changed before completion")
		}
	} else {
		var previousAttempt uint64
		var previousKey, previousExecution, previousStatus string
		attemptErr := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT attempt,idempotency_key,execution_id,status FROM _kora_workflow_attempt WHERE tenant_id = ? AND instance_id = ? AND step_id = ? ORDER BY attempt DESC LIMIT 1`), string(change.Claim.Tenant), string(change.Claim.InstanceID), string(change.Claim.StepID)).Scan(&previousAttempt, &previousKey, &previousExecution, &previousStatus)
		if attemptErr != nil && attemptErr != sql.ErrNoRows {
			return fmt.Errorf("load previous workflow step attempt: %w", attemptErr)
		}
		if uint64(change.Attempt) != previousAttempt+1 {
			return contract.NewError(contract.CodeConflict, "workflow attempt number is not the next durable claimed attempt")
		}
		if attemptErr == nil {
			if previousStatus != "failed" {
				return contract.NewError(contract.CodeConflict, "previous workflow attempt outcome is unresolved")
			}
			if previousKey != string(change.IdempotencyKey) || previousExecution != change.ExecutionID {
				return contract.NewError(contract.CodeConflict, "workflow action idempotency or execution identity changed across attempts")
			}
		}
		_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_attempt (tenant_id, instance_id, step_id, execution_id, attempt, status, idempotency_key, result_json, failure, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, `+stampExpr+`, `+stampExpr+`)`), string(change.Claim.Tenant), string(change.Claim.InstanceID), string(change.Claim.StepID), change.ExecutionID, change.Attempt, attemptStatus, string(change.IdempotencyKey), string(resultJSON), change.Failure)
		if err != nil {
			return fmt.Errorf("record workflow attempt: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id, instance_id, sequence, step_id, kind, status, actor_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, `+stampExpr+`)`), string(change.Claim.Tenant), string(change.Claim.InstanceID), sequence, string(change.Claim.StepID), "step_committed", string(change.Status), nil, change.Failure)
	if err != nil {
		return fmt.Errorf("record workflow history: %w", err)
	}
	if _, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id = ? AND instance_id = ?`), string(change.Claim.Tenant), string(change.Claim.InstanceID)); err != nil {
		return fmt.Errorf("clear workflow timer: %w", err)
	}
	if change.NextWakeAt != nil && !terminal {
		if _, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_process_timer (tenant_id, instance_id, step_id, generation, due_at) VALUES (?, ?, ?, ?, ?)`), string(change.Claim.Tenant), string(change.Claim.InstanceID), nextStep, uint64(change.Claim.Fence), change.NextWakeAt.UTC()); err != nil {
			return fmt.Errorf("schedule workflow timer: %w", err)
		}
	}
	if change.EventWait != nil {
		wait := change.EventWait
		if _, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_subscription (tenant_id,instance_id,step_id,event_type,correlation_field,correlation_value,on_event) VALUES (?,?,?,?,?,?,?)`), string(change.Claim.Tenant), string(change.Claim.InstanceID), nextStep, wait.EventType, wait.CorrelationField, wait.CorrelationValue, string(wait.OnEvent)); err != nil {
			return fmt.Errorf("register workflow event wait: %w", err)
		}
		if _, err = s.resumeEventSignalsTx(ctx, tx, change.Claim.Tenant, maxWorkflowSignalBatch, "", string(change.Claim.InstanceID)); err != nil {
			return fmt.Errorf("check workflow event inbox during wait registration: %w", err)
		}
	}
	for _, event := range change.Events {
		if err := s.events.Append(ctx, tx, event); err != nil {
			return fmt.Errorf("append workflow event %s: %w", event.Type, err)
		}
	}
	return nil
}

// ValidateClaimInTx locks the running instance row before a transaction-bound
// local domain operation. The caller must keep the returned transaction open
// through the domain mutation and CommitStepInTx, so another worker cannot
// replace the fence between validation and commit.
func (s *ExecutionStore) ValidateClaimInTx(ctx context.Context, tx *sql.Tx, claim workflow.Claim) error {
	if s == nil || s.database == nil || s.dialect == nil || tx == nil || ctx == nil {
		return fmt.Errorf("workflow claim validation requires store, context and caller transaction")
	}
	if err := s.validateClaim(claim, 0); err != nil {
		return err
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	query := `SELECT id FROM _kora_workflow_instance WHERE tenant_id=? AND id=? AND current_step=? AND status='running' AND lease_owner=? AND lease_fence=? AND revision=? AND lease_until>` + stamp
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += " FOR UPDATE"
	} else if s.dialect.DriverName() == "libsql" {
		// SQLite transactions are deferred. A matched no-op update acquires the
		// write lock before the business operation and fails closed on stale claims.
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET lease_fence=lease_fence WHERE tenant_id=? AND id=? AND current_step=? AND status='running' AND lease_owner=? AND lease_fence=? AND revision=? AND lease_until>`+stamp), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), claim.Owner, uint64(claim.Fence), mustRevision(claim.Revision))
		if err != nil {
			return fmt.Errorf("lock workflow claim for local action: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count workflow claim lock: %w", err)
		}
		if changed != 1 {
			return contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
		}
		return nil
	}
	var id string
	err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(claim.Tenant), string(claim.InstanceID), string(claim.StepID), claim.Owner, uint64(claim.Fence), mustRevision(claim.Revision)).Scan(&id)
	if err == sql.ErrNoRows {
		return contract.NewError(contract.CodeConflict, "workflow claim is stale or expired")
	}
	if err != nil {
		return fmt.Errorf("validate and lock workflow claim: %w", err)
	}
	return nil
}

func (s *ExecutionStore) validateClaimRequest(ctx context.Context, tenant contract.TenantID, owner string, limit uint16, lease time.Duration) error {
	if s == nil || s.database == nil || s.dialect == nil {
		return fmt.Errorf("workflow execution store is not configured")
	}
	if ctx == nil {
		return fmt.Errorf("workflow claim requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return fmt.Errorf("workflow tenant: %w", err)
	}
	if owner == "" || len(owner) > 160 {
		return fmt.Errorf("workflow owner must be 1 to 160 bytes")
	}
	if limit == 0 || limit > 100 {
		return fmt.Errorf("workflow claim limit must be between 1 and 100")
	}
	return validateLease(lease)
}
func (s *ExecutionStore) validateClaim(claim workflow.Claim, lease time.Duration) error {
	if s == nil || s.database == nil || s.dialect == nil {
		return fmt.Errorf("workflow execution store is not configured")
	}
	if _, err := contract.ParseTenantID(string(claim.Tenant)); err != nil {
		return fmt.Errorf("workflow tenant: %w", err)
	}
	if claim.BusinessScope.Validate() != nil || claim.BusinessScope.TenantID != claim.Tenant {
		return contract.NewError(contract.CodeValidationFailed, "workflow claim business scope is invalid")
	}
	if _, err := contract.ParseInstanceID(string(claim.InstanceID)); err != nil {
		return fmt.Errorf("workflow instance: %w", err)
	}
	if _, err := contract.ParseStepID(string(claim.StepID)); err != nil {
		return fmt.Errorf("workflow step: %w", err)
	}
	if claim.Owner == "" || claim.Fence == 0 || claim.Attempt == 0 {
		return fmt.Errorf("workflow claim owner, fence and attempt are required")
	}
	if len(claim.Owner) > 160 {
		return fmt.Errorf("workflow claim owner exceeds 160 bytes")
	}
	if _, err := contract.ParseRevision(string(claim.Revision)); err != nil {
		return fmt.Errorf("workflow claim revision: %w", err)
	}
	if lease > 0 {
		return validateLease(lease)
	}
	return nil
}
func validateLease(lease time.Duration) error {
	if lease < time.Second || lease > 5*time.Minute {
		return fmt.Errorf("workflow lease must be between 1 second and 5 minutes")
	}
	return nil
}
func (s *ExecutionStore) leaseUpdateExpression() string {
	switch s.dialect.DriverName() {
	case "mysql":
		return "DATE_ADD(CURRENT_TIMESTAMP(6), INTERVAL ? MICROSECOND)"
	case "postgres":
		return "CURRENT_TIMESTAMP + (? * INTERVAL '1 microsecond')"
	default:
		return "datetime('now', '+' || ? || ' seconds')"
	}
}
func (s *ExecutionStore) claimUpdateQuery() string {
	expression := s.leaseUpdateExpression()
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	query := `UPDATE _kora_workflow_instance SET status = 'running', lease_owner = ?, lease_fence = lease_fence + 1, lease_until = ` + expression + `, updated_at = ` + stamp + ` WHERE tenant_id = ? AND id = ? AND revision = ? AND status IN ('ready','retrying','running') AND (lease_until IS NULL OR lease_until <= ` + stamp + `)`
	if s.dialect.DriverName() == "libsql" {
		query = `UPDATE _kora_workflow_instance SET status = 'running', lease_owner = ?, lease_fence = lease_fence + 1, lease_until = ` + expression + `, updated_at = CURRENT_TIMESTAMP WHERE tenant_id = ? AND id = ? AND revision = ? AND status IN ('ready','retrying','running') AND (lease_until IS NULL OR lease_until <= CURRENT_TIMESTAMP)`
	}
	return query
}
func validCommitStatus(status contract.InstanceStatus) bool {
	switch status {
	case contract.InstanceReady, contract.InstanceRunning, contract.InstanceWaiting, contract.InstanceRetrying, contract.InstanceCompleted, contract.InstanceCancelled, contract.InstanceFailed, contract.InstanceUncertain:
		return true
	default:
		return false
	}
}
func mustRevision(revision contract.Revision) uint64 {
	n, _ := strconv.ParseUint(string(revision), 10, 64)
	return n
}

func (s *ExecutionStore) leaseArgument(lease time.Duration) any {
	if s.dialect.DriverName() == "libsql" {
		return strconv.FormatFloat(lease.Seconds(), 'f', 6, 64)
	}
	return lease.Microseconds()
}
