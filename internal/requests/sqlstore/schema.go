// Package sqlstore owns SQL schema details for durable human requests.
package sqlstore

import (
	"fmt"

	"github.com/asenawritescode/kora/internal/db"
)

// SchemaStatements returns additive request tables and indexes for a tenant
// database. Tenant identity is retained in each row even though each tenant
// has a separate database, so moving/querying records cannot silently erase
// the security boundary.
func SchemaStatements(dialect db.Dialect) ([]string, error) {
	switch value := dialect.(type) {
	case *db.LibSQLDialect:
		if value == nil {
			return nil, fmt.Errorf("request database dialect is nil")
		}
		return libSQLSchema(), nil
	case *db.PostgresDialect:
		if value == nil {
			return nil, fmt.Errorf("request database dialect is nil")
		}
		return postgresSchema(), nil
	case *db.MySQLDialect:
		if value == nil {
			return nil, fmt.Errorf("request database dialect is nil")
		}
		return mysqlSchema(), nil
	default:
		return nil, fmt.Errorf("unsupported request database dialect %T", dialect)
	}
}

func libSQLSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_request (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, revision INTEGER NOT NULL,
			kind TEXT NOT NULL, assignment_mode TEXT NOT NULL DEFAULT 'any', status TEXT NOT NULL, requester_id TEXT NOT NULL, supersedes_request_id TEXT,
			subject_kind TEXT, subject_id TEXT, subject_revision TEXT, subject_digest TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL, message TEXT NOT NULL, due_at TEXT, outcome TEXT, resolved_at TEXT,
			continuation_operation_id TEXT NOT NULL DEFAULT '', continuation_state TEXT NOT NULL DEFAULT '',
			policy_id TEXT NOT NULL DEFAULT '', policy_revision TEXT NOT NULL DEFAULT '',
			information_schema_id TEXT NOT NULL DEFAULT '', information_schema_version INTEGER NOT NULL DEFAULT 0,
			source_instance_id TEXT NOT NULL DEFAULT '', source_step_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_status_due ON _kora_request (status, due_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_workflow_source ON _kora_request (tenant_id, source_instance_id, source_step_id, status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_requester_created ON _kora_request (requester_id, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_assignment (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, request_id TEXT NOT NULL,
			stage INTEGER NOT NULL, assignee_kind TEXT NOT NULL, assignee_id TEXT NOT NULL,
			original_assignee_kind TEXT NOT NULL, original_assignee_id TEXT NOT NULL,
			status TEXT NOT NULL, allowed_responses TEXT NOT NULL, membership_revision TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			CONSTRAINT uq_kora_request_assignment_participant UNIQUE (request_id, stage, assignee_kind, assignee_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_assignment_actor_status ON _kora_request_assignment (assignee_kind, assignee_id, status, request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_assignment_request ON _kora_request_assignment (request_id, stage, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_decision (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, request_id TEXT NOT NULL,
			assignment_id TEXT NOT NULL, actor_id TEXT NOT NULL, on_behalf_of_id TEXT NOT NULL DEFAULT '',
			response TEXT NOT NULL, comment TEXT NOT NULL DEFAULT '', information_json TEXT,
			created_at TEXT NOT NULL, CONSTRAINT uq_kora_request_final_decision UNIQUE (request_id, assignment_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_decision_request ON _kora_request_decision (request_id, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_history (
			tenant_id TEXT NOT NULL, request_id TEXT NOT NULL, sequence INTEGER NOT NULL,
			kind TEXT NOT NULL, actor_id TEXT, assignment_id TEXT, response TEXT, comment TEXT NOT NULL DEFAULT '',
			previous_assignee_json TEXT, new_assignee_json TEXT, created_at TEXT NOT NULL, PRIMARY KEY (request_id, sequence)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_history_tenant_request ON _kora_request_history (tenant_id, request_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_read (
			request_id TEXT NOT NULL, principal_id TEXT NOT NULL, history_sequence INTEGER NOT NULL,
			read_at TEXT NOT NULL, PRIMARY KEY (request_id, principal_id)
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_idempotency (
			tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, operation TEXT NOT NULL, idempotency_key TEXT NOT NULL,
			command_digest TEXT NOT NULL, receipt_json TEXT NOT NULL, created_at TEXT NOT NULL, expires_at TEXT NOT NULL,
			PRIMARY KEY (tenant_id, actor_id, operation, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_idempotency_expiry ON _kora_request_idempotency (expires_at)`,
	}
}

func postgresSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_request (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, revision BIGINT NOT NULL,
			kind TEXT NOT NULL, assignment_mode TEXT NOT NULL DEFAULT 'any', status TEXT NOT NULL, requester_id TEXT NOT NULL, supersedes_request_id TEXT,
			subject_kind TEXT, subject_id TEXT, subject_revision TEXT, subject_digest TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL, message TEXT NOT NULL, due_at TIMESTAMPTZ, outcome TEXT, resolved_at TIMESTAMPTZ,
			continuation_operation_id TEXT NOT NULL DEFAULT '', continuation_state TEXT NOT NULL DEFAULT '',
			policy_id TEXT NOT NULL DEFAULT '', policy_revision TEXT NOT NULL DEFAULT '',
			information_schema_id TEXT NOT NULL DEFAULT '', information_schema_version BIGINT NOT NULL DEFAULT 0,
			source_instance_id TEXT NOT NULL DEFAULT '', source_step_id TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_status_due ON _kora_request (status, due_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_workflow_source ON _kora_request (tenant_id, source_instance_id, source_step_id, status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_requester_created ON _kora_request (requester_id, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_assignment (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, request_id TEXT NOT NULL,
			stage INTEGER NOT NULL, assignee_kind TEXT NOT NULL, assignee_id TEXT NOT NULL,
			original_assignee_kind TEXT NOT NULL, original_assignee_id TEXT NOT NULL,
			status TEXT NOT NULL, allowed_responses JSONB NOT NULL, membership_revision TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
			CONSTRAINT uq_kora_request_assignment_participant UNIQUE (request_id, stage, assignee_kind, assignee_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_assignment_actor_status ON _kora_request_assignment (assignee_kind, assignee_id, status, request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_assignment_request ON _kora_request_assignment (request_id, stage, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_decision (
			id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, request_id TEXT NOT NULL,
			assignment_id TEXT NOT NULL, actor_id TEXT NOT NULL, on_behalf_of_id TEXT NOT NULL DEFAULT '',
			response TEXT NOT NULL, comment TEXT NOT NULL DEFAULT '', information_json JSONB,
			created_at TIMESTAMPTZ NOT NULL, CONSTRAINT uq_kora_request_final_decision UNIQUE (request_id, assignment_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_decision_request ON _kora_request_decision (request_id, created_at, id)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_history (
			tenant_id TEXT NOT NULL, request_id TEXT NOT NULL, sequence BIGINT NOT NULL,
			kind TEXT NOT NULL, actor_id TEXT, assignment_id TEXT, response TEXT, comment TEXT NOT NULL DEFAULT '',
			previous_assignee_json JSONB, new_assignee_json JSONB, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (request_id, sequence)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_history_tenant_request ON _kora_request_history (tenant_id, request_id, sequence)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_read (
			request_id TEXT NOT NULL, principal_id TEXT NOT NULL, history_sequence BIGINT NOT NULL,
			read_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (request_id, principal_id)
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_request_idempotency (
			tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, operation TEXT NOT NULL, idempotency_key TEXT NOT NULL,
			command_digest TEXT NOT NULL, receipt_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (tenant_id, actor_id, operation, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_request_idempotency_expiry ON _kora_request_idempotency (expires_at)`,
	}
}

func mysqlSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_request (
			id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, revision BIGINT NOT NULL,
			kind VARCHAR(32) NOT NULL, assignment_mode VARCHAR(24) NOT NULL DEFAULT 'any', status VARCHAR(32) NOT NULL, requester_id VARCHAR(160) NOT NULL, supersedes_request_id VARCHAR(64),
			subject_kind VARCHAR(140), subject_id VARCHAR(160), subject_revision VARCHAR(32), subject_digest VARCHAR(64) NOT NULL DEFAULT '',
			title VARCHAR(640) NOT NULL, message TEXT NOT NULL, due_at DATETIME(6), outcome VARCHAR(32), resolved_at DATETIME(6),
			continuation_operation_id VARCHAR(64) NOT NULL DEFAULT '', continuation_state VARCHAR(32) NOT NULL DEFAULT '',
			policy_id VARCHAR(140) NOT NULL DEFAULT '', policy_revision VARCHAR(32) NOT NULL DEFAULT '',
			information_schema_id VARCHAR(140) NOT NULL DEFAULT '', information_schema_version BIGINT NOT NULL DEFAULT 0,
			source_instance_id VARCHAR(64) NOT NULL DEFAULT '', source_step_id VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
			INDEX idx_kora_request_status_due (status, due_at, id),
			INDEX idx_kora_request_workflow_source (tenant_id, source_instance_id, source_step_id, status, updated_at),
			INDEX idx_kora_request_requester_created (requester_id, created_at, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_request_assignment (
			id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, request_id VARCHAR(64) NOT NULL,
			stage INT NOT NULL, assignee_kind VARCHAR(16) NOT NULL, assignee_id VARCHAR(160) NOT NULL,
			original_assignee_kind VARCHAR(16) NOT NULL, original_assignee_id VARCHAR(160) NOT NULL,
			status VARCHAR(24) NOT NULL, allowed_responses JSON NOT NULL, membership_revision VARCHAR(32) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
			UNIQUE KEY uq_kora_request_assignment_participant (request_id, stage, assignee_kind, assignee_id),
			INDEX idx_kora_request_assignment_actor_status (assignee_kind, assignee_id, status, request_id),
			INDEX idx_kora_request_assignment_request (request_id, stage, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_request_decision (
			id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, request_id VARCHAR(64) NOT NULL,
			assignment_id VARCHAR(64) NOT NULL, actor_id VARCHAR(160) NOT NULL, on_behalf_of_id VARCHAR(160) NOT NULL DEFAULT '',
			response VARCHAR(32) NOT NULL, comment TEXT NOT NULL, information_json JSON,
			created_at DATETIME(6) NOT NULL, UNIQUE KEY uq_kora_request_final_decision (request_id, assignment_id),
			INDEX idx_kora_request_decision_request (request_id, created_at, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_request_history (
			tenant_id VARCHAR(160) NOT NULL, request_id VARCHAR(64) NOT NULL, sequence BIGINT NOT NULL,
			kind VARCHAR(32) NOT NULL, actor_id VARCHAR(160), assignment_id VARCHAR(64), response VARCHAR(32), comment TEXT NOT NULL,
			previous_assignee_json JSON NULL, new_assignee_json JSON NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (request_id, sequence),
			INDEX idx_kora_request_history_tenant_request (tenant_id, request_id, sequence)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_request_read (
			request_id VARCHAR(64) NOT NULL, principal_id VARCHAR(160) NOT NULL, history_sequence BIGINT NOT NULL,
			read_at DATETIME(6) NOT NULL, PRIMARY KEY (request_id, principal_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_request_idempotency (
			tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, operation VARCHAR(32) NOT NULL, idempotency_key VARCHAR(160) NOT NULL,
			command_digest CHAR(64) NOT NULL, receipt_json JSON NOT NULL, created_at DATETIME(6) NOT NULL, expires_at DATETIME(6) NOT NULL,
			PRIMARY KEY (tenant_id, actor_id, operation, idempotency_key),
			INDEX idx_kora_request_idempotency_expiry (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}
