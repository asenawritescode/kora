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
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/workflow"
)

type InstanceStore struct {
	database *sql.DB
	dialect  db.Dialect
	events   outbox.Writer
}

func NewInstanceStore(database *sql.DB, dialect db.Dialect) (*InstanceStore, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("workflow instance store requires database and dialect")
	}
	if _, err := ExecutionSchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &InstanceStore{database: database, dialect: dialect, events: outbox.NewSQLWriter(dialect)}, nil
}

var _ workflow.InstanceRepository = (*InstanceStore)(nil)

func (s *InstanceStore) StartInstance(ctx context.Context, input workflow.StartInstanceInput) (contract.WorkflowInstance, error) {
	for attempt := 0; ; attempt++ {
		instance, err := s.startInstanceOnce(ctx, input)
		if err == nil || s == nil || s.dialect == nil || !s.dialect.IsWriteConflict(err) || attempt >= 4 {
			return instance, err
		}
		delay := time.Duration(10*(1<<attempt)) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return contract.WorkflowInstance{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// InspectOpenWork reports bounded nonterminal workflow instances whose pinned
// definition is owned by the module. The query uses existing definition and
// instance stores and returns opaque IDs only.
func (s *InstanceStore) InspectOpenWork(ctx context.Context, tenant contract.TenantID, module contract.ModuleID, limit uint16) ([]contract.ModuleBlocker, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil || tenant == "" || module == "" {
		return nil, fmt.Errorf("workflow open-work inspection identity is required")
	}
	if limit == 0 || limit > 20 {
		limit = 20
	}
	var count uint64
	query := `SELECT COUNT(*) FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND JSON_UNQUOTE(JSON_EXTRACT(d.definition_json,'$.owner_module_id'))=?`
	if s.dialect.DriverName() == "libsql" {
		query = `SELECT COUNT(*) FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND json_extract(d.definition_json,'$.owner_module_id')=?`
	} else if s.dialect.DriverName() == "postgres" {
		query = `SELECT COUNT(*) FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND d.definition_json->>'owner_module_id'=?`
	}
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(module)).Scan(&count); err != nil {
		return nil, fmt.Errorf("count module-owned workflow instances: %w", err)
	}
	query = `SELECT i.id FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND JSON_UNQUOTE(JSON_EXTRACT(d.definition_json,'$.owner_module_id'))=? ORDER BY i.updated_at,i.id LIMIT ?`
	if s.dialect.DriverName() == "libsql" {
		query = `SELECT i.id FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND json_extract(d.definition_json,'$.owner_module_id')=? ORDER BY i.updated_at,i.id LIMIT ?`
	}
	if s.dialect.DriverName() == "postgres" {
		query = `SELECT i.id FROM _kora_workflow_instance i JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE i.tenant_id=? AND i.status NOT IN ('completed','cancelled','failed') AND d.definition_json->>'owner_module_id'=? ORDER BY i.updated_at,i.id LIMIT ?`
	}
	examples := make([]string, 0, limit)
	if count > 0 {
		rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(module), int(limit))
		if err != nil {
			return nil, fmt.Errorf("list module-owned workflow instances: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			examples = append(examples, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	pinnedOwnerExpr := `JSON_UNQUOTE(JSON_EXTRACT(pinned_dispatch_json,'$.dispatch.caller_module_id'))`
	pinnedProviderOwnerExpr := `JSON_UNQUOTE(JSON_EXTRACT(pinned_dispatch_json,'$.dispatch.provider_module_id'))`
	if s.dialect.DriverName() == "libsql" {
		pinnedOwnerExpr = `json_extract(pinned_dispatch_json,'$.dispatch.caller_module_id')`
		pinnedProviderOwnerExpr = `json_extract(pinned_dispatch_json,'$.dispatch.provider_module_id')`
	}
	if s.dialect.DriverName() == "postgres" {
		pinnedOwnerExpr = `pinned_dispatch_json->'dispatch'->>'caller_module_id'`
		pinnedProviderOwnerExpr = `pinned_dispatch_json->'dispatch'->>'provider_module_id'`
	}
	pinnedQuery := `SELECT COUNT(*) FROM _kora_workflow_attempt WHERE tenant_id=? AND status IN ('running','outcome_unknown') AND pinned_dispatch_json IS NOT NULL AND (` + pinnedOwnerExpr + `=? OR ` + pinnedProviderOwnerExpr + `=?)`
	var pinnedCount uint64
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, pinnedQuery), string(tenant), string(module), string(module)).Scan(&pinnedCount); err != nil {
		return nil, fmt.Errorf("count pinned module capability attempts: %w", err)
	}
	pinnedQuery = `SELECT execution_id FROM _kora_workflow_attempt WHERE tenant_id=? AND status IN ('running','outcome_unknown') AND pinned_dispatch_json IS NOT NULL AND (` + pinnedOwnerExpr + `=? OR ` + pinnedProviderOwnerExpr + `=?) ORDER BY started_at,execution_id LIMIT ?`
	var pinnedExamples []string
	if pinnedCount > 0 {
		rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, pinnedQuery), string(tenant), string(module), string(module), int(limit))
		if err != nil {
			return nil, fmt.Errorf("list pinned module capability attempts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var executionID string
			if err := rows.Scan(&executionID); err != nil {
				return nil, err
			}
			pinnedExamples = append(pinnedExamples, executionID)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	blockers := make([]contract.ModuleBlocker, 0, 2)
	if count > 0 {
		blockers = append(blockers, contract.ModuleBlocker{Code: "workflow_instance_open", Count: clampModuleWorkCount(count), Examples: examples})
	}
	unknownQuery := `SELECT COUNT(*) FROM _kora_workflow_attempt a JOIN _kora_workflow_definition d ON d.tenant_id=a.tenant_id AND d.definition_id=(SELECT i.definition_id FROM _kora_workflow_instance i WHERE i.tenant_id=a.tenant_id AND i.id=a.instance_id) AND d.version=(SELECT i.definition_version FROM _kora_workflow_instance i WHERE i.tenant_id=a.tenant_id AND i.id=a.instance_id) WHERE a.tenant_id=? AND a.status='outcome_unknown' AND JSON_UNQUOTE(JSON_EXTRACT(d.definition_json,'$.owner_module_id'))=?`
	if s.dialect.DriverName() == "libsql" {
		unknownQuery = `SELECT COUNT(*) FROM _kora_workflow_attempt a JOIN _kora_workflow_instance i ON i.tenant_id=a.tenant_id AND i.id=a.instance_id JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE a.tenant_id=? AND a.status='outcome_unknown' AND json_extract(d.definition_json,'$.owner_module_id')=?`
	}
	if s.dialect.DriverName() == "postgres" {
		unknownQuery = `SELECT COUNT(*) FROM _kora_workflow_attempt a JOIN _kora_workflow_instance i ON i.tenant_id=a.tenant_id AND i.id=a.instance_id JOIN _kora_workflow_definition d ON d.tenant_id=i.tenant_id AND d.definition_id=i.definition_id AND d.version=i.definition_version WHERE a.tenant_id=? AND a.status='outcome_unknown' AND d.definition_json->>'owner_module_id'=?`
	}
	var unknown uint64
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, unknownQuery), string(tenant), string(module)).Scan(&unknown); err != nil {
		return nil, fmt.Errorf("count unknown module-owned workflow outcomes: %w", err)
	}
	if unknown > 0 {
		blockers = append(blockers, contract.ModuleBlocker{Code: "external_outcome_unknown", Count: clampModuleWorkCount(unknown)})
	}
	if len(pinnedExamples) > 0 {
		blockers = append(blockers, contract.ModuleBlocker{Code: "pinned_provider_work_open", Count: clampModuleWorkCount(pinnedCount), Examples: pinnedExamples})
	}
	return blockers, nil
}

func clampModuleWorkCount(value uint64) uint32 {
	if value > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func admitWorkflowModuleActive(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant contract.TenantID, owner contract.ModuleID) error {
	if owner == "" {
		return nil
	}
	if tx == nil || dialect == nil || tenant == "" {
		return fmt.Errorf("workflow module admission identity is incomplete")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_module_snapshot SET revision=revision WHERE tenant_id=?`), string(tenant)); err != nil {
		return fmt.Errorf("fence workflow write against module lifecycle: %w", err)
	}
	var raw string
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT snapshot_json FROM _kora_module_snapshot WHERE tenant_id=?`), string(tenant)).Scan(&raw); err != nil {
		return fmt.Errorf("load workflow module lifecycle state: %w", err)
	}
	var snapshot contract.ModuleSnapshot
	if json.Unmarshal([]byte(raw), &snapshot) != nil || snapshot.TenantID != tenant {
		return fmt.Errorf("workflow module lifecycle snapshot is invalid")
	}
	for _, installation := range snapshot.Installations {
		if installation.ModuleID != owner {
			continue
		}
		if installation.State == string(contract.ModuleActive) {
			return nil
		}
		return contract.NewError(contract.CodePermissionDenied, "owning module is not accepting new workflow writes")
	}
	return contract.NewError(contract.CodePermissionDenied, "owning workflow module is not installed")
}

func (s *InstanceStore) startInstanceOnce(ctx context.Context, input workflow.StartInstanceInput) (contract.WorkflowInstance, error) {
	if err := s.validateStart(ctx, input); err != nil {
		return contract.WorkflowInstance{}, err
	}
	commandJSON, err := json.Marshal(struct {
		DefinitionID   contract.DefinitionID  `json:"definition_id"`
		Version        uint32                 `json:"version"`
		Digest         string                 `json:"digest"`
		Subject        contract.SubjectRef    `json:"subject"`
		BusinessScope  contract.BusinessScope `json:"business_scope"`
		CausationDepth uint16                 `json:"causation_depth"`
	}{input.Definition.ID, input.Definition.Version, input.DefinitionDigest, input.Subject, input.BusinessScope, input.CausationDepth})
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	commandHash := sha256.Sum256(commandJSON)
	commandDigest := hex.EncodeToString(commandHash[:])
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("begin workflow start: %w", err)
	}
	defer tx.Rollback()
	var previousDigest, previousID string
	lookup := `SELECT command_digest, instance_id FROM _kora_workflow_start_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		lookup += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, lookup), string(input.Tenant), string(input.Actor), string(input.IdempotencyKey)).Scan(&previousDigest, &previousID)
	if err == nil {
		if previousDigest != commandDigest {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow start key was reused with different content")
		}
		instance, loadErr := getInstanceTx(ctx, tx, s.dialect, input.Tenant, contract.InstanceID(previousID))
		if loadErr != nil {
			return contract.WorkflowInstance{}, loadErr
		}
		if err := tx.Commit(); err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("commit workflow start replay: %w", err)
		}
		return instance, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contract.WorkflowInstance{}, fmt.Errorf("read workflow start receipt: %w", err)
	}
	var storedDefinitionDigest string
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?`), string(input.Tenant), string(input.Definition.ID), input.Definition.Version).Scan(&storedDefinitionDigest)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("load pinned workflow definition: %w", err)
	}
	if storedDefinitionDigest != input.DefinitionDigest {
		return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "workflow definition changed from the published version")
	}
	if err := admitWorkflowModuleActive(ctx, tx, s.dialect, input.Tenant, input.Definition.OwnerModuleID); err != nil {
		return contract.WorkflowInstance{}, err
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	var businessScopeJSON any
	if input.BusinessScope.TenantID != "" {
		businessScopeJSON, err = json.Marshal(persistedWorkflowBusinessContext{BusinessScope: input.BusinessScope, CausationDepth: input.CausationDepth})
		if err != nil {
			return contract.WorkflowInstance{}, fmt.Errorf("encode workflow business scope: %w", err)
		}
	}
	insert := `INSERT INTO _kora_workflow_instance (id,tenant_id,definition_id,definition_version,subject_kind,subject_id,subject_revision,business_scope_json,status,current_step,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,1,` + stamp + `,` + stamp + `)`
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, insert), string(input.ID), string(input.Tenant), string(input.Definition.ID), input.Definition.Version, input.Subject.Kind, input.Subject.ID, input.Subject.Revision, businessScopeJSON, string(contract.InstanceReady), string(input.Definition.Entry)); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("insert workflow instance: %w", err)
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,1,?,?,?,?,?,`+stamp+`)`), string(input.Tenant), string(input.ID), string(input.Definition.Entry), "started", string(contract.InstanceReady), string(input.Actor), "")
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("insert workflow start history: %w", err)
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_start_receipt (tenant_id,actor_id,idempotency_key,command_digest,instance_id,created_at) VALUES (?,?,?,?,?,`+stamp+`)`), string(input.Tenant), string(input.Actor), string(input.IdempotencyKey), commandDigest, string(input.ID))
	if err != nil {
		_ = tx.Rollback()
		if instance, found, lookupErr := s.lookupStartReceipt(ctx, input, commandDigest); found || lookupErr != nil {
			return instance, lookupErr
		}
		return contract.WorkflowInstance{}, fmt.Errorf("insert workflow start receipt: %w", err)
	}
	eventData, err := json.Marshal(contract.WorkflowEventData{InstanceID: input.ID, Revision: "1", ChangeKind: "started"})
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("encode workflow event data: %w", err)
	}
	event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.started", Version: 1, Source: "workflow", Site: input.Site, BusinessScope: input.BusinessScope, CausationDepth: input.CausationDepth, AggregateType: "workflow_instance", AggregateID: string(input.ID), OccurredAt: input.Now.UTC(), Data: eventData}
	if err := event.Validate(); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("validate workflow event: %w", err)
	}
	if err := s.events.Append(ctx, tx, event); err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("append workflow started event: %w", err)
	}
	instance, err := getInstanceTx(ctx, tx, s.dialect, input.Tenant, input.ID)
	if err != nil {
		return contract.WorkflowInstance{}, err
	}
	if err := tx.Commit(); err != nil {
		if resolved, found, lookupErr := s.lookupStartReceipt(ctx, input, commandDigest); found || lookupErr != nil {
			return resolved, lookupErr
		}
		return contract.WorkflowInstance{}, fmt.Errorf("commit workflow start: %w", err)
	}
	return instance, nil
}

func (s *InstanceStore) lookupStartReceipt(ctx context.Context, input workflow.StartInstanceInput, commandDigest string) (contract.WorkflowInstance, bool, error) {
	var storedDigest, instanceID string
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest,instance_id FROM _kora_workflow_start_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ?`), string(input.Tenant), string(input.Actor), string(input.IdempotencyKey)).Scan(&storedDigest, &instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.WorkflowInstance{}, false, nil
	}
	if err != nil {
		return contract.WorkflowInstance{}, false, fmt.Errorf("resolve workflow start receipt: %w", err)
	}
	if storedDigest != commandDigest {
		return contract.WorkflowInstance{}, true, contract.NewError(contract.CodeConflict, "workflow start key was reused with different content")
	}
	instance, err := getInstanceTx(ctx, s.database, s.dialect, input.Tenant, contract.InstanceID(instanceID))
	if err != nil {
		return contract.WorkflowInstance{}, true, fmt.Errorf("load committed workflow start: %w", err)
	}
	return instance, true, nil
}

func (s *InstanceStore) GetInstance(ctx context.Context, tenant contract.TenantID, id contract.InstanceID) (contract.WorkflowInstance, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return contract.WorkflowInstance{}, fmt.Errorf("workflow instance store is not configured")
	}
	if ctx == nil {
		return contract.WorkflowInstance{}, fmt.Errorf("workflow lookup requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	if _, err := contract.ParseInstanceID(string(id)); err != nil {
		return contract.WorkflowInstance{}, err
	}
	return getInstanceTx(ctx, s.database, s.dialect, tenant, id)
}

func (s *InstanceStore) LoadPinnedCapabilityAttempt(ctx context.Context, tenant contract.TenantID, instance contract.InstanceID, step contract.StepID) (workflow.PinnedCapabilityAttempt, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil {
		return workflow.PinnedCapabilityAttempt{}, fmt.Errorf("workflow pinned dispatch repository is not configured")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return workflow.PinnedCapabilityAttempt{}, err
	}
	if _, err := contract.ParseInstanceID(string(instance)); err != nil {
		return workflow.PinnedCapabilityAttempt{}, err
	}
	if _, err := contract.ParseStepID(string(step)); err != nil {
		return workflow.PinnedCapabilityAttempt{}, err
	}
	var raw sql.NullString
	var executionID, idempotencyKey string
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT pinned_dispatch_json,execution_id,idempotency_key FROM _kora_workflow_attempt WHERE tenant_id=? AND instance_id=? AND step_id=? ORDER BY attempt DESC LIMIT 1`), string(tenant), string(instance), string(step)).Scan(&raw, &executionID, &idempotencyKey)
	if err != nil {
		return workflow.PinnedCapabilityAttempt{}, fmt.Errorf("load pinned workflow provider identity: %w", err)
	}
	if !raw.Valid {
		return workflow.PinnedCapabilityAttempt{}, contract.NewError(contract.CodeNotFound, "workflow step has no pinned provider dispatch")
	}
	var attempt workflow.PinnedCapabilityAttempt
	if err := json.Unmarshal([]byte(raw.String), &attempt); err != nil || attempt.Validate() != nil || attempt.Dispatch.ExecutionID != executionID || string(attempt.Dispatch.IdempotencyKey) != idempotencyKey {
		return workflow.PinnedCapabilityAttempt{}, contract.NewError(contract.CodeConflict, "persisted pinned provider dispatch is invalid")
	}
	if attempt.Dispatch.Scope.TenantID != tenant {
		return workflow.PinnedCapabilityAttempt{}, contract.NewError(contract.CodePermissionDenied, "pinned provider dispatch belongs to another tenant")
	}
	return attempt, nil
}

func getInstanceTx(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, dialect db.Dialect, tenant contract.TenantID, id contract.InstanceID) (contract.WorkflowInstance, error) {
	var value contract.WorkflowInstance
	var revision uint64
	var createdRaw, updatedRaw any
	var businessScopeJSON sql.NullString
	var wakeRaw sql.NullTime
	var wakeText sql.NullString
	if dialect.DriverName() == "libsql" {
		err := q.QueryRowContext(ctx, db.Rebind(dialect, `SELECT id,revision,definition_id,definition_version,subject_kind,subject_id,subject_revision,business_scope_json,status,current_step,next_wake_at,created_at,updated_at FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ?`), string(tenant), string(id)).Scan(&value.ID, &revision, &value.DefinitionID, &value.DefinitionVersion, &value.Subject.Kind, &value.Subject.ID, &value.Subject.Revision, &businessScopeJSON, &value.Status, &value.CurrentStep, &wakeText, &createdRaw, &updatedRaw)
		if err != nil {
			return contract.WorkflowInstance{}, err
		}
		if wakeText.Valid {
			parsed, err := parseDBTime(wakeText.String)
			if err != nil {
				return contract.WorkflowInstance{}, err
			}
			value.NextWakeAt = &parsed
		}
	} else {
		err := q.QueryRowContext(ctx, db.Rebind(dialect, `SELECT id,revision,definition_id,definition_version,subject_kind,subject_id,subject_revision,business_scope_json,status,current_step,next_wake_at,created_at,updated_at FROM _kora_workflow_instance WHERE tenant_id = ? AND id = ?`), string(tenant), string(id)).Scan(&value.ID, &revision, &value.DefinitionID, &value.DefinitionVersion, &value.Subject.Kind, &value.Subject.ID, &value.Subject.Revision, &businessScopeJSON, &value.Status, &value.CurrentStep, &wakeRaw, &createdRaw, &updatedRaw)
		if err != nil {
			return contract.WorkflowInstance{}, err
		}
		if wakeRaw.Valid {
			value.NextWakeAt = &wakeRaw.Time
		}
	}
	if businessScopeJSON.Valid {
		storedContext, err := decodePersistedWorkflowBusinessContext(businessScopeJSON.String, tenant)
		if err != nil {
			return contract.WorkflowInstance{}, contract.NewError(contract.CodeConflict, "stored workflow business scope is invalid")
		}
		value.BusinessScope, value.CausationDepth = storedContext.BusinessScope, storedContext.CausationDepth
	} else {
		value.BusinessScope = contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}
	}
	created, err := parseDBTime(createdRaw)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("parse workflow creation time: %w", err)
	}
	updated, err := parseDBTime(updatedRaw)
	if err != nil {
		return contract.WorkflowInstance{}, fmt.Errorf("parse workflow update time: %w", err)
	}
	value.Revision = contract.Revision(fmt.Sprint(revision))
	value.CreatedAt = created
	value.UpdatedAt = updated
	if err := validatePersistedInstance(value); err != nil {
		return contract.WorkflowInstance{}, err
	}
	return value, nil
}

func parseDBTime(raw any) (time.Time, error) {
	switch value := raw.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseTimeText(value)
	case []byte:
		return parseTimeText(string(value))
	default:
		return time.Time{}, fmt.Errorf("unsupported database time value %T", raw)
	}
}
func parseTimeText(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999Z07:00", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999Z07:00"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid database timestamp")
}
func validatePersistedInstance(instance contract.WorkflowInstance) error {
	if _, err := contract.ParseInstanceID(string(instance.ID)); err != nil {
		return fmt.Errorf("stored workflow instance ID: %w", err)
	}
	if _, err := contract.ParseRevision(string(instance.Revision)); err != nil {
		return fmt.Errorf("stored workflow revision: %w", err)
	}
	if _, err := contract.ParseDefinitionID(string(instance.DefinitionID)); err != nil {
		return fmt.Errorf("stored workflow definition ID: %w", err)
	}
	if instance.DefinitionVersion == 0 || instance.Subject.Kind == "" || instance.Subject.ID == "" || instance.Subject.Revision == "" || instance.CurrentStep == "" || instance.CausationDepth > contract.MaxEventCausationDepth || !validCommitStatus(instance.Status) || instance.CreatedAt.IsZero() || instance.UpdatedAt.IsZero() {
		return fmt.Errorf("stored workflow instance is invalid")
	}
	return nil
}

func (s *InstanceStore) validateStart(ctx context.Context, input workflow.StartInstanceInput) error {
	if s == nil || s.database == nil || s.dialect == nil {
		return fmt.Errorf("workflow instance store is not configured")
	}
	if ctx == nil {
		return fmt.Errorf("workflow start requires context")
	}
	if input.CausationDepth > contract.MaxEventCausationDepth {
		return contract.NewError(contract.CodeResourceExhausted, "workflow causation depth exceeds the dispatch bound")
	}
	if _, err := contract.ParseTenantID(string(input.Tenant)); err != nil {
		return fmt.Errorf("workflow tenant: %w", err)
	}
	if _, err := contract.ParseInstanceID(string(input.ID)); err != nil {
		return fmt.Errorf("workflow instance: %w", err)
	}
	if _, err := contract.ParseDefinitionID(string(input.Definition.ID)); err != nil {
		return fmt.Errorf("workflow definition: %w", err)
	}
	if input.Definition.Version == 0 || input.Definition.Entry == "" || len(input.DefinitionDigest) != 64 || strings.TrimSpace(input.DefinitionDigest) != input.DefinitionDigest {
		return fmt.Errorf("workflow definition version, entry and digest are required")
	}
	if _, err := hex.DecodeString(input.DefinitionDigest); err != nil {
		return fmt.Errorf("workflow definition digest is invalid")
	}
	if _, err := contract.ParseStepID(string(input.Definition.Entry)); err != nil {
		return fmt.Errorf("workflow entry step: %w", err)
	}
	if input.Subject.Kind == "" || input.Subject.ID == "" || input.Subject.Revision == "" {
		return fmt.Errorf("workflow subject binding is required")
	}
	if _, err := contract.ParseRevision(string(input.Subject.Revision)); err != nil {
		return fmt.Errorf("workflow subject revision: %w", err)
	}
	if _, err := contract.ParsePrincipalID(string(input.Actor)); err != nil {
		return fmt.Errorf("workflow starter: %w", err)
	}
	if _, err := contract.ParseIdempotencyKey(string(input.IdempotencyKey)); err != nil {
		return fmt.Errorf("workflow start idempotency key: %w", err)
	}
	if input.Site == "" || input.Now.IsZero() {
		return fmt.Errorf("workflow start site and time are required")
	}
	if input.BusinessScope.TenantID != "" || input.BusinessScope.Kind != "" || input.BusinessScope.LegalEntityID != "" {
		if err := input.BusinessScope.Validate(); err != nil || input.BusinessScope.TenantID != input.Tenant {
			return fmt.Errorf("workflow business scope is invalid")
		}
	}
	return nil
}

type persistedWorkflowBusinessContext struct {
	contract.BusinessScope
	CausationDepth uint16 `json:"causation_depth,omitempty"`
}

func decodePersistedWorkflowBusinessContext(raw string, tenant contract.TenantID) (persistedWorkflowBusinessContext, error) {
	var stored persistedWorkflowBusinessContext
	if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.BusinessScope.Validate() != nil || stored.BusinessScope.TenantID != tenant || stored.CausationDepth > contract.MaxEventCausationDepth {
		return persistedWorkflowBusinessContext{}, fmt.Errorf("workflow business context is invalid")
	}
	return stored, nil
}

var _ = errors.Is
