// Package sqlstore persists immutable workflow definitions in a tenant DB.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/workflow"
)

type DefinitionStore struct {
	database *sql.DB
	dialect  db.Dialect
}

var _ workflow.DefinitionRepository = (*DefinitionStore)(nil)
var _ workflow.StatusPolicyRepository = (*DefinitionStore)(nil)
var _ workflow.StatusPolicyReader = (*DefinitionStore)(nil)

func NewDefinitionStore(database *sql.DB, dialect db.Dialect) (*DefinitionStore, error) {
	if database == nil || dialect == nil {
		return nil, fmt.Errorf("workflow definition store requires database and dialect")
	}
	if _, err := DefinitionSchemaStatements(dialect); err != nil {
		return nil, err
	}
	return &DefinitionStore{database: database, dialect: dialect}, nil
}

func (s *DefinitionStore) GetStatusPolicy(ctx context.Context, tenant contract.TenantID, table string) (*workflow.StatusPolicy, bool, error) {
	if s == nil || s.database == nil || ctx == nil {
		return nil, false, fmt.Errorf("workflow status-policy lookup requires context and database")
	}
	tx, err := s.database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, fmt.Errorf("begin workflow status-policy read: %w", err)
	}
	defer tx.Rollback()
	return s.GetStatusPolicyInTx(ctx, tx, tenant, table)
}

func DefinitionSchemaStatements(dialect db.Dialect) ([]string, error) {
	if dialect == nil {
		return nil, fmt.Errorf("workflow definition schema requires dialect")
	}
	switch dialect.DriverName() {
	case "libsql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_definition (tenant_id TEXT NOT NULL, definition_id TEXT NOT NULL, version INTEGER NOT NULL, digest TEXT NOT NULL, definition_json TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, definition_id, version))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_publish_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest TEXT NOT NULL, definition_id TEXT NOT NULL, version INTEGER NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
		}, nil
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_definition (tenant_id TEXT NOT NULL, definition_id TEXT NOT NULL, version BIGINT NOT NULL, digest CHAR(64) NOT NULL, definition_json JSONB NOT NULL, created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, definition_id, version))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_publish_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest CHAR(64) NOT NULL, definition_id TEXT NOT NULL, version BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
		}, nil
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_definition (tenant_id VARCHAR(160) NOT NULL, definition_id VARCHAR(64) NOT NULL, version BIGINT NOT NULL, digest CHAR(64) NOT NULL, definition_json JSON NOT NULL, created_by VARCHAR(160) NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, definition_id, version)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_publish_receipt (tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_digest CHAR(64) NOT NULL, definition_id VARCHAR(64) NOT NULL, version BIGINT NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported workflow definition dialect %q", dialect.DriverName())
	}
}

func (s *DefinitionStore) Ensure(ctx context.Context) error {
	// The legacy actor runtime is retired and its state has no supported
	// continuation in the unified process model. Remove its obsolete storage
	// when a tenant runtime upgrades; do not load or migrate stale instances.
	for _, table := range []string{"_kora_workflow_audit", "_kora_workflow_timer", "_kora_workflow_actor"} {
		statement := "DROP TABLE IF EXISTS " + s.dialect.QuoteIdent(table)
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("retire legacy workflow table %s: %w", table, err)
		}
	}
	statements, err := DefinitionSchemaStatements(s.dialect)
	if err != nil {
		return err
	}
	executionStatements, err := ExecutionSchemaStatements(s.dialect)
	if err != nil {
		return err
	}
	statements = append(statements, executionStatements...)
	for _, statement := range statements {
		if _, err := s.database.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure workflow definition table: %w", err)
		}
	}
	if err := s.retireLifecycleDefinitions(ctx); err != nil {
		return err
	}
	return nil
}

// retireLifecycleDefinitions discards only obsolete lifecycle-only definition
// rows. Unified record-trigger definitions and their execution history remain.
func (s *DefinitionStore) retireLifecycleDefinitions(ctx context.Context) error {
	var query string
	switch s.dialect.DriverName() {
	case "libsql":
		query = `DELETE FROM _kora_workflow_definition WHERE definition_json LIKE '%"lifecycle"%'`
	case "postgres":
		query = `DELETE FROM _kora_workflow_definition WHERE definition_json ? 'lifecycle'`
	case "mysql":
		query = `DELETE FROM _kora_workflow_definition WHERE JSON_EXTRACT(definition_json, '$.lifecycle') IS NOT NULL`
	default:
		return fmt.Errorf("unsupported workflow definition dialect %q", s.dialect.DriverName())
	}
	if _, err := s.database.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("retire obsolete lifecycle workflow definitions: %w", err)
	}
	return nil
}

// PublishDefinition stores one immutable version and a scoped idempotency
// receipt atomically. It must only be called after definition validation.
func (s *DefinitionStore) PublishDefinition(ctx context.Context, tenant contract.TenantID, definition contract.WorkflowDefinition, actor contract.PrincipalID, key contract.IdempotencyKey, now time.Time) (contract.DefinitionVersionRef, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition store is not configured")
	}
	if ctx == nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition publish requires context")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow tenant: %w", err)
	}
	if _, err := contract.ParseDefinitionID(string(definition.ID)); err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition ID: %w", err)
	}
	if definition.Version == 0 {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition version must be positive")
	}
	if _, err := contract.ParsePrincipalID(string(actor)); err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow publisher: %w", err)
	}
	if _, err := contract.ParseIdempotencyKey(string(key)); err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow publish key: %w", err)
	}
	body, err := json.Marshal(definition)
	if err != nil {
		return contract.DefinitionVersionRef{}, err
	}
	if len(body) > 1<<20 {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition exceeds 1 MiB")
	}
	if now.IsZero() {
		return contract.DefinitionVersionRef{}, fmt.Errorf("workflow publish time is required")
	}
	digest, err := workflow.DefinitionDigest(definition)
	if err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("digest workflow definition: %w", err)
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return contract.DefinitionVersionRef{}, fmt.Errorf("begin workflow publish: %w", err)
	}
	defer tx.Rollback()
	var priorDigest, priorID string
	var priorVersion uint32
	lookup := `SELECT command_digest, definition_id, version FROM _kora_workflow_publish_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ?`
	if s.dialect.DriverName() == "mysql" || s.dialect.DriverName() == "postgres" {
		lookup += " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, lookup), string(tenant), string(actor), string(key)).Scan(&priorDigest, &priorID, &priorVersion)
	if err == nil {
		if priorDigest != digest {
			return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeConflict, "workflow publish key was reused with different content")
		}
		var storedDigest string
		if err := tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?`), string(tenant), priorID, priorVersion).Scan(&storedDigest); err != nil {
			return contract.DefinitionVersionRef{}, fmt.Errorf("load workflow publish receipt: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return contract.DefinitionVersionRef{}, fmt.Errorf("commit workflow publish replay: %w", err)
		}
		return contract.DefinitionVersionRef{DefinitionID: contract.DefinitionID(priorID), Version: priorVersion, Digest: storedDigest}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return contract.DefinitionVersionRef{}, fmt.Errorf("read workflow publish receipt: %w", err)
	}
	var oldDigest string
	err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?`), string(tenant), string(definition.ID), definition.Version).Scan(&oldDigest)
	if err == nil && oldDigest != digest {
		return contract.DefinitionVersionRef{}, contract.NewError(contract.CodeConflict, "workflow definition versions are immutable")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return contract.DefinitionVersionRef{}, fmt.Errorf("check workflow definition version: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_definition (tenant_id, definition_id, version, digest, definition_json, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), string(definition.ID), definition.Version, digest, string(body), string(actor), now)
		if err != nil {
			return contract.DefinitionVersionRef{}, fmt.Errorf("insert immutable workflow definition: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_publish_receipt (tenant_id, actor_id, idempotency_key, command_digest, definition_id, version, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`), string(tenant), string(actor), string(key), digest, string(definition.ID), definition.Version, now)
	if err != nil {
		_ = tx.Rollback()
		if ref, found, lookupErr := s.lookupPublishReceipt(ctx, tenant, actor, key, digest); found || lookupErr != nil {
			return ref, lookupErr
		}
		return contract.DefinitionVersionRef{}, fmt.Errorf("insert workflow publish receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		if ref, found, lookupErr := s.lookupPublishReceipt(ctx, tenant, actor, key, digest); found || lookupErr != nil {
			return ref, lookupErr
		}
		return contract.DefinitionVersionRef{}, fmt.Errorf("commit workflow publish: %w", err)
	}
	return contract.DefinitionVersionRef{DefinitionID: definition.ID, Version: definition.Version, Digest: digest}, nil
}

func (s *DefinitionStore) lookupPublishReceipt(ctx context.Context, tenant contract.TenantID, actor contract.PrincipalID, key contract.IdempotencyKey, digest string) (contract.DefinitionVersionRef, bool, error) {
	var priorDigest, definitionID string
	var version uint32
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT command_digest,definition_id,version FROM _kora_workflow_publish_receipt WHERE tenant_id = ? AND actor_id = ? AND idempotency_key = ?`), string(tenant), string(actor), string(key)).Scan(&priorDigest, &definitionID, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return contract.DefinitionVersionRef{}, false, nil
	}
	if err != nil {
		return contract.DefinitionVersionRef{}, false, fmt.Errorf("resolve workflow publish receipt: %w", err)
	}
	if priorDigest != digest {
		return contract.DefinitionVersionRef{}, true, contract.NewError(contract.CodeConflict, "workflow publish key was reused with different content")
	}
	var storedDigest string
	if err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?`), string(tenant), definitionID, version).Scan(&storedDigest); err != nil {
		return contract.DefinitionVersionRef{}, true, fmt.Errorf("load committed workflow definition: %w", err)
	}
	return contract.DefinitionVersionRef{DefinitionID: contract.DefinitionID(definitionID), Version: version, Digest: storedDigest}, true, nil
}

func (s *DefinitionStore) GetDefinition(ctx context.Context, tenant contract.TenantID, id contract.DefinitionID, version uint32) (contract.WorkflowDefinition, contract.DefinitionVersionRef, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition store is not configured")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("workflow tenant: %w", err)
	}
	if _, err := contract.ParseDefinitionID(string(id)); err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition ID: %w", err)
	}
	if version == 0 {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("workflow definition version must be positive")
	}
	var digest string
	var body []byte
	err := s.database.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest, definition_json FROM _kora_workflow_definition WHERE tenant_id = ? AND definition_id = ? AND version = ?`), string(tenant), string(id), version).Scan(&digest, &body)
	if err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, err
	}
	var definition contract.WorkflowDefinition
	if err := json.Unmarshal(body, &definition); err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("decode stored workflow definition: %w", err)
	}
	if definition.ID != id || definition.Version != version {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("stored workflow definition identity does not match its key")
	}
	computedDigest, err := workflow.DefinitionDigest(definition)
	if err != nil {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("digest stored workflow definition: %w", err)
	}
	if computedDigest != digest {
		return contract.WorkflowDefinition{}, contract.DefinitionVersionRef{}, fmt.Errorf("stored workflow definition digest mismatch")
	}
	return definition, contract.DefinitionVersionRef{DefinitionID: id, Version: version, Digest: digest}, nil
}

// ListLatestDefinitions returns the newest published version of each workflow
// for one tenant. The tenant predicate is applied before grouping so no
// cross-tenant metadata can be exposed.
func (s *DefinitionStore) ListLatestDefinitions(ctx context.Context, tenant contract.TenantID) ([]contract.DefinitionVersionRef, error) {
	if s == nil || s.database == nil || s.dialect == nil {
		return nil, fmt.Errorf("workflow definition store is not configured")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("workflow tenant: %w", err)
	}
	query := `SELECT d.definition_id, d.version, d.digest FROM _kora_workflow_definition d
		INNER JOIN (SELECT definition_id, MAX(version) AS version FROM _kora_workflow_definition WHERE tenant_id = ? GROUP BY definition_id) latest
		ON latest.definition_id = d.definition_id AND latest.version = d.version WHERE d.tenant_id = ? ORDER BY d.definition_id`
	rows, err := s.database.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(tenant))
	if err != nil {
		return nil, fmt.Errorf("list latest workflow definitions: %w", err)
	}
	defer rows.Close()
	refs := make([]contract.DefinitionVersionRef, 0)
	for rows.Next() {
		var id, digest string
		var version uint32
		if err := rows.Scan(&id, &version, &digest); err != nil {
			return nil, fmt.Errorf("scan latest workflow definition: %w", err)
		}
		refs = append(refs, contract.DefinitionVersionRef{DefinitionID: contract.DefinitionID(id), Version: version, Digest: digest})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest workflow definitions: %w", err)
	}
	return refs, nil
}

// GetStatusPolicyInTx assembles policy from latest published status-action
// steps in the caller's record transaction.
func (s *DefinitionStore) GetStatusPolicyInTx(ctx context.Context, tx *sql.Tx, tenant contract.TenantID, table string) (*workflow.StatusPolicy, bool, error) {
	if s == nil || tx == nil || s.dialect == nil || table == "" {
		return nil, false, fmt.Errorf("workflow status-policy lookup requires transaction, tenant, and table")
	}
	query := `SELECT d.definition_id,d.version,d.digest,d.definition_json FROM _kora_workflow_definition d INNER JOIN (SELECT definition_id,MAX(version) AS version FROM _kora_workflow_definition WHERE tenant_id=? GROUP BY definition_id) latest ON latest.definition_id=d.definition_id AND latest.version=d.version WHERE d.tenant_id=? ORDER BY d.definition_id`
	rows, err := tx.QueryContext(ctx, db.Rebind(s.dialect, query), string(tenant), string(tenant))
	if err != nil {
		return nil, false, fmt.Errorf("query published status actions: %w", err)
	}
	defer rows.Close()
	policy := &workflow.StatusPolicy{Table: table}
	seenTransitions := map[string]struct{}{}
	for rows.Next() {
		var id, storedDigest string
		var version uint32
		var body []byte
		if err := rows.Scan(&id, &version, &storedDigest, &body); err != nil {
			return nil, false, fmt.Errorf("scan published status-action definition: %w", err)
		}
		var definition contract.WorkflowDefinition
		if err := json.Unmarshal(body, &definition); err != nil {
			return nil, false, fmt.Errorf("decode status-action definition: %w", err)
		}
		computedDigest, err := workflow.DefinitionDigest(definition)
		if err != nil {
			return nil, false, fmt.Errorf("digest status-action definition: %w", err)
		}
		if definition.ID != contract.DefinitionID(id) || definition.Version != version || computedDigest != storedDigest {
			return nil, false, fmt.Errorf("status-action definition identity or digest mismatch")
		}
		if definition.Trigger == nil || definition.Trigger.Table != table {
			continue
		}
		for _, step := range definition.Steps {
			if step.Action == nil || step.Action.Name != "record.update_status" || step.Action.Version != 1 {
				continue
			}
			var config workflow.StatusActionConfig
			if err := json.Unmarshal(step.Action.Input, &config); err != nil {
				return nil, false, fmt.Errorf("decode status action %s/%s: %w", definition.ID, step.ID, err)
			}
			if policy.StateField == "" {
				policy.StateField, policy.InitialState = config.StateField, config.InitialState
			} else if policy.StateField != config.StateField || policy.InitialState != config.InitialState {
				return nil, false, contract.NewError(contract.CodeConflict, "published status actions disagree about the table status field or initial value")
			}
			key := strings.ToLower(config.Action) + "\x00" + config.From
			if _, exists := seenTransitions[key]; exists {
				return nil, false, contract.NewError(contract.CodeConflict, "multiple published status actions define the same transition")
			}
			seenTransitions[key] = struct{}{}
			policy.Transitions = append(policy.Transitions, config.StatusTransition)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate published status actions: %w", err)
	}
	return policy, len(policy.Transitions) > 0, nil
}

// PublishImportedBatch atomically publishes a validated set of v2 definitions
// from a tenant configuration import. Matching immutable rows make retries
// safe; conflicts roll back the whole batch.
func (s *DefinitionStore) PublishImportedBatch(ctx context.Context, tenant contract.TenantID, actor contract.PrincipalID, definitions []contract.WorkflowDefinition, now time.Time) error {
	if s == nil || s.database == nil || s.dialect == nil || ctx == nil {
		return fmt.Errorf("workflow definition import dependencies are incomplete")
	}
	if _, err := contract.ParseTenantID(string(tenant)); err != nil {
		return err
	}
	if _, err := contract.ParsePrincipalID(string(actor)); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("workflow definition import time is required")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow definition import batch: %w", err)
	}
	defer tx.Rollback()
	for _, definition := range definitions {
		if definition.ID == "" || definition.Version == 0 || len(definition.Steps) == 0 {
			return fmt.Errorf("batch contains an invalid workflow definition")
		}
		if _, err := contract.ParseDefinitionID(string(definition.ID)); err != nil {
			return err
		}
		body, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		digest, err := workflow.DefinitionDigest(definition)
		if err != nil {
			return fmt.Errorf("digest imported workflow definition: %w", err)
		}
		var existing string
		err = tx.QueryRowContext(ctx, db.Rebind(s.dialect, `SELECT digest FROM _kora_workflow_definition WHERE tenant_id=? AND definition_id=? AND version=?`), string(tenant), string(definition.ID), definition.Version).Scan(&existing)
		if err == nil {
			if existing != digest {
				return contract.NewError(contract.CodeConflict, "imported workflow definition conflicts with an existing immutable version")
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check imported workflow definition: %w", err)
		}
		if _, err := tx.ExecContext(ctx, db.Rebind(s.dialect, `INSERT INTO _kora_workflow_definition (tenant_id,definition_id,version,digest,definition_json,created_by,created_at) VALUES (?,?,?,?,?,?,?)`), string(tenant), string(definition.ID), definition.Version, digest, string(body), string(actor), now.UTC()); err != nil {
			return fmt.Errorf("insert imported workflow definition %s: %w", definition.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow definition import batch: %w", err)
	}
	return nil
}
