package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/workflow"
)

const (
	maxWorkflowSignalBytes            = 64 << 10
	maxWorkflowSignalBatch     uint16 = 100
	maxWorkflowSignalInboxRows int64  = 512
)

// AcceptSignal persists the event inbox item and advances a bounded set of
// matching waits in one transaction. A trusted event consumer should use
// AcceptSignalInTx so its own consumer receipt commits with this inbox write.
func (s *ExecutionStore) AcceptSignal(ctx context.Context, signal workflow.Signal) (uint32, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return 0, fmt.Errorf("workflow signal store is not configured")
	}
	if ctx == nil {
		return 0, fmt.Errorf("workflow signal acceptance requires context")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin workflow signal acceptance: %w", err)
	}
	defer tx.Rollback()
	advanced, err := s.AcceptSignalInTx(ctx, tx, signal)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit workflow signal acceptance: %w", err)
	}
	return advanced, nil
}

// AcceptSignalInTx accepts an event into the durable inbox using a caller-owned
// transaction. Event identity is tenant-scoped and repeated delivery is safe.
func (s *ExecutionStore) AcceptSignalInTx(ctx context.Context, tx *sql.Tx, signal workflow.Signal) (uint32, error) {
	if s == nil || s.database == nil || s.dialect == nil || tx == nil || ctx == nil {
		return 0, fmt.Errorf("workflow signal transaction requires configured store, context, and transaction")
	}
	if _, err := contract.ParseTenantID(string(signal.Tenant)); err != nil {
		return 0, fmt.Errorf("workflow signal tenant: %w", err)
	}
	if err := signal.Event.Validate(); err != nil {
		return 0, fmt.Errorf("workflow signal event: %w", err)
	}
	if len(signal.Event.Data) > maxWorkflowSignalBytes {
		return 0, contract.NewError(contract.CodeValidationFailed, "workflow signal payload exceeds 64 KiB")
	}
	payload, err := canonicalSignalEnvelope(signal.Event)
	if err != nil {
		return 0, fmt.Errorf("encode workflow signal: %w", err)
	}
	if len(payload) > maxWorkflowSignalBytes {
		return 0, contract.NewError(contract.CodeValidationFailed, "workflow signal envelope exceeds 64 KiB")
	}
	if err := lockSignalInboxCapacityTx(ctx, tx, s.dialect, signal.Tenant); err != nil {
		return 0, err
	}
	if _, err := s.pruneExpiredSignalsTx(ctx, tx, signal.Tenant, maxWorkflowSignalBatch); err != nil {
		return 0, err
	}
	var existing string
	existingErr := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT payload_json FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?`), string(signal.Tenant), signal.Event.ID).Scan(&existing)
	if existingErr == nil {
		same, err := sameSignalEnvelope(existing, payload)
		if err != nil {
			return 0, err
		}
		if !same {
			return 0, contract.NewError(contract.CodeConflict, "workflow event ID was reused with different content")
		}
		return s.resumeEventSignalsTx(ctx, tx, signal.Tenant, maxWorkflowSignalBatch, signal.Event.ID, "")
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return 0, fmt.Errorf("read existing workflow signal identity: %w", existingErr)
	}
	var pending int64
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COUNT(*) FROM _kora_workflow_signal WHERE tenant_id=? AND received_at>=`+signalRetentionCutoff(s.dialect)), string(signal.Tenant)).Scan(&pending); err != nil {
		return 0, fmt.Errorf("count retained workflow signal inbox items: %w", err)
	}
	if pending >= maxWorkflowSignalInboxRows {
		return 0, contract.NewError(contract.CodeResourceExhausted, "workflow signal inbox is full; event remains pending for retry")
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	query := `INSERT INTO _kora_workflow_signal (tenant_id,event_id,event_type,correlation_value,payload_json,received_at) VALUES (?,?,?,?,?,` + stamp + `)`
	switch s.dialect.DriverName() {
	case "mysql":
		query = `INSERT IGNORE INTO _kora_workflow_signal (tenant_id,event_id,event_type,correlation_value,payload_json,received_at) VALUES (?,?,?,?,?,` + stamp + `)`
	case "postgres":
		query += ` ON CONFLICT (tenant_id,event_id) DO NOTHING`
	case "libsql":
		query = `INSERT OR IGNORE INTO _kora_workflow_signal (tenant_id,event_id,event_type,correlation_value,payload_json,received_at) VALUES (?,?,?,?,?,` + stamp + `)`
	default:
		return 0, fmt.Errorf("unsupported workflow signal dialect %q", s.dialect.DriverName())
	}
	result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, query), string(signal.Tenant), signal.Event.ID, signal.Event.Type, "", string(payload))
	if err != nil {
		return 0, fmt.Errorf("persist workflow signal inbox item: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("check workflow signal inbox receipt: %w", err)
	}
	if inserted == 0 {
		var previous string
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT payload_json FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?`), string(signal.Tenant), signal.Event.ID).Scan(&previous); err != nil {
			return 0, fmt.Errorf("read existing workflow signal identity after insert conflict: %w", err)
		}
		same, err := sameSignalEnvelope(previous, payload)
		if err != nil {
			return 0, err
		}
		if !same {
			return 0, contract.NewError(contract.CodeConflict, "workflow event ID was reused with different content")
		}
	}
	return s.resumeEventSignalsTx(ctx, tx, signal.Tenant, maxWorkflowSignalBatch, signal.Event.ID, "")
}

// lockSignalInboxCapacityTx serializes acceptance for a tenant before pruning,
// counting, and inserting. Without the lock, concurrent deliveries could both
// observe 511 retained events and exceed the documented 512-row bound.
func lockSignalInboxCapacityTx(ctx context.Context, tx *sql.Tx, dialect db.Dialect, tenant contract.TenantID) error {
	if dialect.DriverName() != "mysql" && dialect.DriverName() != "postgres" && dialect.DriverName() != "libsql" {
		return fmt.Errorf("unsupported workflow signal dialect %q", dialect.DriverName())
	}
	// ExecutionSchemaStatements creates this singleton before serving traffic.
	// The database is tenant-scoped, so one existing row serializes all signal
	// admissions without a first-use unique-key insertion race.
	if _, err := tx.ExecContext(ctx, `UPDATE _kora_workflow_signal_capacity SET lock_id=lock_id WHERE lock_id=1`); err != nil {
		return fmt.Errorf("lock workflow signal capacity row: %w", err)
	}
	query := `SELECT lock_id FROM _kora_workflow_signal_capacity WHERE lock_id=1`
	if dialect.DriverName() == "mysql" || dialect.DriverName() == "postgres" {
		query += ` FOR UPDATE`
	}
	var locked int
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, query)).Scan(&locked); err != nil {
		return fmt.Errorf("lock workflow signal capacity: %w", err)
	}
	if locked != 1 {
		return fmt.Errorf("workflow signal capacity lock row is invalid")
	}
	return nil
}

func sameSignalEnvelope(stored string, current []byte) (bool, error) {
	var previous contract.EventEnvelope
	if err := json.Unmarshal([]byte(stored), &previous); err != nil {
		return false, fmt.Errorf("decode stored workflow signal identity: %w", err)
	}
	canonical, err := canonicalSignalEnvelope(previous)
	if err != nil {
		return false, fmt.Errorf("canonicalize stored workflow signal identity: %w", err)
	}
	return string(canonical) == string(current), nil
}

func canonicalSignalEnvelope(event contract.EventEnvelope) ([]byte, error) {
	var data any
	decoder := json.NewDecoder(strings.NewReader(string(event.Data)))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return nil, fmt.Errorf("decode workflow signal data: %w", err)
	}
	canonicalData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("canonicalize workflow signal data: %w", err)
	}
	event.Data = canonicalData
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode workflow signal envelope: %w", err)
	}
	return payload, nil
}

// ResumeEventSignals recovers accepted signals left pending by a bounded
// consumer pass or process interruption. Each event/instance pair is fenced by
// a unique receipt and commits with the instance transition and outbox event.
func (s *ExecutionStore) ResumeEventSignals(ctx context.Context, tenant contract.TenantID, limit uint16) (uint16, error) {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil {
		return 0, fmt.Errorf("workflow event recovery requires a configured store and context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return 0, err
	}
	if limit == 0 || limit > maxWorkflowSignalBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "workflow event recovery batch must be from 1 to 100")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin workflow event recovery: %w", err)
	}
	defer tx.Rollback()
	if _, err := s.pruneExpiredSignalsTx(ctx, tx, tenant, limit); err != nil {
		return 0, err
	}
	advanced, err := s.resumeEventSignalsTx(ctx, tx, tenant, limit, "", "")
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit workflow event recovery: %w", err)
	}
	return uint16(advanced), nil
}

func signalRetentionCutoff(dialect db.Dialect) string {
	switch dialect.DriverName() {
	case "mysql":
		return "DATE_SUB(CURRENT_TIMESTAMP(6), INTERVAL 31 DAY)"
	case "postgres":
		return "CURRENT_TIMESTAMP - INTERVAL '31 days'"
	default:
		return "datetime('now','-31 days')"
	}
}

func (s *ExecutionStore) pruneExpiredSignalsTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, limit uint16) (uint16, error) {
	if limit == 0 || limit > maxWorkflowSignalBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "workflow signal cleanup batch must be from 1 to 100")
	}
	query := `SELECT event_id FROM _kora_workflow_signal WHERE tenant_id=? AND received_at<` + signalRetentionCutoff(s.dialect) + ` ORDER BY received_at,event_id LIMIT ?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		query += ` FOR UPDATE SKIP LOCKED`
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), int64(limit))
	if err != nil {
		return 0, fmt.Errorf("select expired workflow signals: %w", err)
	}
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired workflow signal: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read expired workflow signals: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close expired workflow signals: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_signal_receipt WHERE tenant_id=? AND event_id=?`), string(tenant), id); err != nil {
			return 0, fmt.Errorf("delete expired workflow signal receipts: %w", err)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_signal WHERE tenant_id=? AND event_id=?`), string(tenant), id); err != nil {
			return 0, fmt.Errorf("delete expired workflow signal: %w", err)
		}
	}
	return uint16(len(ids)), nil
}

type signalPair struct {
	eventID, eventType, envelope string
	instanceID, stepID           string
	field, expected, onEvent     string
	revision                     int64
	definitionID                 string
	definitionVersion            int64
}

func (s *ExecutionStore) resumeEventSignalsTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, limit uint16, eventID, instanceID string) (uint32, error) {
	if limit == 0 || limit > maxWorkflowSignalBatch {
		return 0, contract.NewError(contract.CodeValidationFailed, "workflow event recovery batch must be from 1 to 100")
	}
	query := `SELECT s.event_id,s.event_type,s.payload_json,i.id,i.current_step,sub.correlation_field,sub.correlation_value,sub.on_event,i.revision,i.definition_id,i.definition_version FROM _kora_workflow_signal s JOIN _kora_workflow_subscription sub ON sub.tenant_id=s.tenant_id AND sub.event_type=s.event_type JOIN _kora_workflow_instance i ON i.tenant_id=sub.tenant_id AND i.id=sub.instance_id AND i.current_step=sub.step_id AND i.status='waiting' LEFT JOIN _kora_workflow_signal_receipt r ON r.tenant_id=s.tenant_id AND r.event_id=s.event_id AND r.instance_id=i.id WHERE s.tenant_id=? AND r.event_id IS NULL`
	args := []any{string(tenant)}
	if eventID != "" {
		query += ` AND s.event_id=?`
		args = append(args, eventID)
	}
	if instanceID != "" {
		query += ` AND i.id=?`
		args = append(args, instanceID)
	}
	query += ` ORDER BY s.received_at,s.event_id,i.id LIMIT ?`
	args = append(args, int64(limit))
	if s.dialect.DriverName() == "mysql" {
		query += ` FOR UPDATE SKIP LOCKED`
	} else if s.dialect.DriverName() == "postgres" {
		query += ` FOR UPDATE OF i SKIP LOCKED`
	}
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), args...)
	if err != nil {
		return 0, fmt.Errorf("select pending workflow signals: %w", err)
	}
	pairs := make([]signalPair, 0, limit)
	for rows.Next() {
		var pair signalPair
		if err := rows.Scan(&pair.eventID, &pair.eventType, &pair.envelope, &pair.instanceID, &pair.stepID, &pair.field, &pair.expected, &pair.onEvent, &pair.revision, &pair.definitionID, &pair.definitionVersion); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan pending workflow signal: %w", err)
		}
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("read pending workflow signals: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close pending workflow signals: %w", err)
	}
	var advanced uint32
	for _, pair := range pairs {
		matched, err := s.applySignalPairTx(ctx, tx, tenant, pair)
		if err != nil {
			return advanced, err
		}
		if matched {
			advanced++
		}
	}
	return advanced, nil
}

func (s *ExecutionStore) applySignalPairTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, pair signalPair) (bool, error) {
	if pair.revision < 1 || pair.definitionVersion < 1 || pair.field == "" || pair.expected == "" || pair.onEvent == "" {
		return false, contract.NewError(contract.CodeInternal, "stored workflow signal subscription is invalid")
	}
	var digest string
	var body []byte
	if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest,definition_json FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?`), string(tenant), pair.definitionID, pair.definitionVersion).Scan(&digest, &body); err != nil {
		return false, fmt.Errorf("load pinned workflow event definition: %w", err)
	}
	var definition contract.WorkflowDefinition
	if err := json.Unmarshal(body, &definition); err != nil {
		return false, fmt.Errorf("decode pinned workflow event definition: %w", err)
	}
	computedDigest, err := workflow.DefinitionDigest(definition)
	if err != nil {
		return false, fmt.Errorf("digest pinned workflow event definition: %w", err)
	}
	if computedDigest != digest || definition.ID != contract.DefinitionID(pair.definitionID) || int64(definition.Version) != pair.definitionVersion {
		return false, contract.NewError(contract.CodeInternal, "pinned workflow event definition failed integrity validation")
	}
	step, ok := definitionStep(definition, contract.StepID(pair.stepID))
	if !ok || step.Kind != contract.StepWait || step.Wait == nil || step.Wait.EventType != pair.eventType || step.Wait.CorrelationField != pair.field || string(step.Wait.OnEvent) != pair.onEvent {
		return false, contract.NewError(contract.CodeInternal, "workflow event subscription does not match its pinned step")
	}
	var envelope contract.EventEnvelope
	if err := json.Unmarshal([]byte(pair.envelope), &envelope); err != nil {
		return false, fmt.Errorf("decode workflow signal envelope: %w", err)
	}
	value, found, err := eventCorrelationValue(envelope.Data, pair.field)
	if err != nil {
		return false, err
	}
	matched := found && value == pair.expected
	if matched {
		var sequence uint64
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT COALESCE(MAX(sequence),0)+1 FROM _kora_workflow_history WHERE tenant_id=? AND instance_id=?`), string(tenant), pair.instanceID).Scan(&sequence); err != nil {
			return false, fmt.Errorf("allocate workflow signal history sequence: %w", err)
		}
		stamp := "CURRENT_TIMESTAMP"
		if s.dialect.DriverName() == "mysql" {
			stamp = "CURRENT_TIMESTAMP(6)"
		}
		result, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `UPDATE _kora_workflow_instance SET status='ready',current_step=?,revision=revision+1,lease_fence=lease_fence+1,next_wake_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=`+stamp+` WHERE tenant_id=? AND id=? AND current_step=? AND status='waiting' AND revision=?`), pair.onEvent, string(tenant), pair.instanceID, pair.stepID, pair.revision)
		if err != nil {
			return false, fmt.Errorf("resume workflow event wait: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("count resumed workflow event waits: %w", err)
		}
		if changed == 1 {
			if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_process_timer WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), pair.instanceID, pair.stepID); err != nil {
				return false, fmt.Errorf("delete signalled workflow timer: %w", err)
			}
			if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `DELETE FROM _kora_workflow_subscription WHERE tenant_id=? AND instance_id=? AND step_id=?`), string(tenant), pair.instanceID, pair.stepID); err != nil {
				return false, fmt.Errorf("delete consumed workflow subscription: %w", err)
			}
			if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_history (tenant_id,instance_id,sequence,step_id,kind,status,actor_id,detail,created_at) VALUES (?,?,?,?,'signal','ready',NULL,?,`+stamp+`)`), string(tenant), pair.instanceID, sequence, pair.stepID, pair.eventID); err != nil {
				return false, fmt.Errorf("record workflow signal transition: %w", err)
			}
			data, err := json.Marshal(contract.WorkflowEventData{InstanceID: contract.InstanceID(pair.instanceID), Revision: contract.Revision(fmt.Sprint(pair.revision + 1)), ChangeKind: "advanced"})
			if err != nil {
				return false, fmt.Errorf("encode workflow signal event: %w", err)
			}
			event := contract.EventEnvelope{ID: contract.NewEventID(), Type: "kora.workflow.advanced", Version: contract.CurrentVersion, Source: "workflow", Site: envelope.Site, AggregateType: "workflow_instance", AggregateID: pair.instanceID, OccurredAt: envelope.OccurredAt.UTC(), Data: data}
			if err := event.Validate(); err != nil {
				return false, fmt.Errorf("validate workflow signal event: %w", err)
			}
			if err := s.events.Append(ctx, tx, event); err != nil {
				return false, fmt.Errorf("append workflow signal event: %w", err)
			}
		} else {
			matched = false
		}
	}
	stamp := "CURRENT_TIMESTAMP"
	if s.dialect.DriverName() == "mysql" {
		stamp = "CURRENT_TIMESTAMP(6)"
	}
	insert := `INSERT INTO _kora_workflow_signal_receipt (tenant_id,event_id,instance_id,matched,processed_at) VALUES (?,?,?,?,` + stamp + `)`
	if s.dialect.DriverName() == "mysql" {
		insert = `INSERT IGNORE INTO _kora_workflow_signal_receipt (tenant_id,event_id,instance_id,matched,processed_at) VALUES (?,?,?,?,` + stamp + `)`
	} else if s.dialect.DriverName() == "postgres" {
		insert += ` ON CONFLICT (tenant_id,event_id,instance_id) DO NOTHING`
	} else {
		insert = `INSERT OR IGNORE INTO _kora_workflow_signal_receipt (tenant_id,event_id,instance_id,matched,processed_at) VALUES (?,?,?,?,` + stamp + `)`
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, insert), string(tenant), pair.eventID, pair.instanceID, matched); err != nil {
		return false, fmt.Errorf("record workflow signal receipt: %w", err)
	}
	return matched, nil
}

func eventCorrelationValue(payload json.RawMessage, field string) (string, bool, error) {
	if strings.TrimSpace(field) == "" || len(field) > 160 {
		return "", false, contract.NewError(contract.CodeValidationFailed, "workflow event correlation field is invalid")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return "", false, nil
	}
	raw, ok := object[field]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, nil
	}
	if value == "" || len(value) > 512 {
		return "", false, nil
	}
	return value, true, nil
}

// RegisterSignalConsumer joins workflow inbox acceptance to the outbox
// consumer receipt for the tenant database owned by this store. The allowlist
// must contain only event types whose payload correlation fields have been
// registered by the workflow/domain catalog.
func RegisterSignalConsumer(dispatcher *outbox.Dispatcher, store *ExecutionStore, tenant contract.TenantID, eventTypes []string) error {
	if dispatcher == nil || store == nil {
		return fmt.Errorf("workflow signal consumer requires dispatcher and execution store")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return fmt.Errorf("workflow signal tenant: %w", err)
	}
	return dispatcher.RegisterTransactionalForTypes("workflow-signals", eventTypes, func(ctx context.Context, tx *sql.Tx, event contract.EventEnvelope) error {
		_, err := store.AcceptSignalInTx(ctx, tx, workflow.Signal{Tenant: tenant, Event: event})
		return err
	})
}

var _ workflow.SignalStore = (*ExecutionStore)(nil)
