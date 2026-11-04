package sqlstore

import (
	"fmt"

	"github.com/asenawritescode/kora/db"
)

// ExecutionSchemaStatements returns the tables used by unified durable workflow
// execution.
func ExecutionSchemaStatements(dialect db.Dialect) ([]string, error) {
	if dialect == nil {
		return nil, fmt.Errorf("workflow execution schema requires dialect")
	}
	switch dialect.DriverName() {
	case "libsql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_instance (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, definition_id TEXT NOT NULL, definition_version INTEGER NOT NULL, subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL, subject_revision TEXT NOT NULL, business_scope_json TEXT, status TEXT NOT NULL, current_step TEXT NOT NULL, revision INTEGER NOT NULL, next_wake_at TEXT, lease_owner TEXT, lease_fence INTEGER NOT NULL DEFAULT 0, lease_until TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_instance_due ON _kora_workflow_instance (tenant_id, status, next_wake_at, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_capacity (lock_id INTEGER PRIMARY KEY)`,
			`INSERT OR IGNORE INTO _kora_workflow_signal_capacity (lock_id) VALUES (1)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_attempt (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, execution_id TEXT NOT NULL, attempt INTEGER NOT NULL, status TEXT NOT NULL, idempotency_key TEXT NOT NULL, pinned_dispatch_json TEXT, result_json TEXT, failure TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, finished_at TEXT NOT NULL, PRIMARY KEY (instance_id, execution_id, attempt))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_process_timer (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, generation INTEGER NOT NULL, due_at TEXT NOT NULL, PRIMARY KEY (instance_id, step_id))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal (tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, event_type TEXT NOT NULL, correlation_value TEXT NOT NULL, payload_json TEXT NOT NULL, received_at TEXT NOT NULL, consumed_instance_id TEXT, consumed_at TEXT, PRIMARY KEY (tenant_id, event_id))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_signal_correlation ON _kora_workflow_signal (tenant_id, event_type, correlation_value, received_at)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_subscription (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, event_type TEXT NOT NULL, correlation_field TEXT NOT NULL, correlation_value TEXT NOT NULL, on_event TEXT NOT NULL, PRIMARY KEY (tenant_id, instance_id, step_id))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_subscription_match ON _kora_workflow_subscription (tenant_id, event_type, correlation_value, instance_id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_receipt (tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, instance_id TEXT NOT NULL, matched INTEGER NOT NULL, processed_at TEXT NOT NULL, PRIMARY KEY (tenant_id, event_id, instance_id))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, sequence INTEGER NOT NULL, step_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, actor_id TEXT, detail TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY (instance_id, sequence))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_preview (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, definition_id TEXT NOT NULL, definition_version INTEGER NOT NULL, definition_digest TEXT NOT NULL, status TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '', steps_json TEXT NOT NULL, created_at TEXT NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_preview_recent ON _kora_workflow_preview (tenant_id, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_start_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest TEXT NOT NULL, instance_id TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_reconcile_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest TEXT NOT NULL, result_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_cancel_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest TEXT NOT NULL, result_json TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
		}, nil
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_instance (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, definition_id TEXT NOT NULL, definition_version BIGINT NOT NULL, subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL, subject_revision TEXT NOT NULL, business_scope_json JSONB, status TEXT NOT NULL, current_step TEXT NOT NULL, revision BIGINT NOT NULL, next_wake_at TIMESTAMPTZ, lease_owner TEXT, lease_fence BIGINT NOT NULL DEFAULT 0, lease_until TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_instance_due ON _kora_workflow_instance (tenant_id, status, next_wake_at, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_capacity (lock_id INTEGER PRIMARY KEY)`,
			`INSERT INTO _kora_workflow_signal_capacity (lock_id) VALUES (1) ON CONFLICT (lock_id) DO NOTHING`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_attempt (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, execution_id TEXT NOT NULL, attempt INTEGER NOT NULL, status TEXT NOT NULL, idempotency_key TEXT NOT NULL, pinned_dispatch_json JSONB, result_json JSONB, failure TEXT NOT NULL DEFAULT '', started_at TIMESTAMPTZ NOT NULL, finished_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (instance_id, execution_id, attempt))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_process_timer (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, generation BIGINT NOT NULL, due_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (instance_id, step_id))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal (tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, event_type TEXT NOT NULL, correlation_value TEXT NOT NULL, payload_json JSONB NOT NULL, received_at TIMESTAMPTZ NOT NULL, consumed_instance_id TEXT, consumed_at TIMESTAMPTZ, PRIMARY KEY (tenant_id, event_id))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_signal_correlation ON _kora_workflow_signal (tenant_id, event_type, correlation_value, received_at)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_subscription (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, step_id TEXT NOT NULL, event_type TEXT NOT NULL, correlation_field TEXT NOT NULL, correlation_value TEXT NOT NULL, on_event TEXT NOT NULL, PRIMARY KEY (tenant_id, instance_id, step_id))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_subscription_match ON _kora_workflow_subscription (tenant_id, event_type, correlation_value, instance_id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_receipt (tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, instance_id TEXT NOT NULL, matched BOOLEAN NOT NULL, processed_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, event_id, instance_id))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id TEXT NOT NULL, instance_id TEXT NOT NULL, sequence BIGINT NOT NULL, step_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, actor_id TEXT, detail TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (instance_id, sequence))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_preview (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, definition_id TEXT NOT NULL, definition_version BIGINT NOT NULL, definition_digest CHAR(64) NOT NULL, status TEXT NOT NULL, outcome TEXT NOT NULL DEFAULT '', failure TEXT NOT NULL DEFAULT '', steps_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_workflow_preview_recent ON _kora_workflow_preview (tenant_id, created_at DESC, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_start_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest CHAR(64) NOT NULL, instance_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_reconcile_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest CHAR(64) NOT NULL, result_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_cancel_receipt (tenant_id TEXT NOT NULL, actor_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest CHAR(64) NOT NULL, result_json JSONB NOT NULL, created_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key))`,
		}, nil
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_workflow_instance (id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, definition_id VARCHAR(64) NOT NULL, definition_version BIGINT NOT NULL, subject_kind VARCHAR(140) NOT NULL, subject_id VARCHAR(160) NOT NULL, subject_revision VARCHAR(32) NOT NULL, business_scope_json JSON NULL, status VARCHAR(32) NOT NULL, current_step VARCHAR(160) NOT NULL, revision BIGINT NOT NULL, next_wake_at DATETIME(6) NULL, lease_owner VARCHAR(160) NULL, lease_fence BIGINT NOT NULL DEFAULT 0, lease_until DATETIME(6) NULL, created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, INDEX idx_kora_workflow_instance_due (tenant_id, status, next_wake_at, id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_attempt (tenant_id VARCHAR(160) NOT NULL, instance_id VARCHAR(64) NOT NULL, step_id VARCHAR(160) NOT NULL, execution_id VARCHAR(64) NOT NULL, attempt INT NOT NULL, status VARCHAR(32) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, pinned_dispatch_json JSON NULL, result_json JSON NULL, failure TEXT NOT NULL, started_at DATETIME(6) NOT NULL, finished_at DATETIME(6) NOT NULL, PRIMARY KEY (instance_id, execution_id, attempt)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_capacity (lock_id INT NOT NULL PRIMARY KEY) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`INSERT IGNORE INTO _kora_workflow_signal_capacity (lock_id) VALUES (1)`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_process_timer (tenant_id VARCHAR(160) NOT NULL, instance_id VARCHAR(64) NOT NULL, step_id VARCHAR(160) NOT NULL, generation BIGINT NOT NULL, due_at DATETIME(6) NOT NULL, PRIMARY KEY (instance_id, step_id), INDEX idx_kora_workflow_process_timer_due (tenant_id, due_at, instance_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal (tenant_id VARCHAR(160) NOT NULL, event_id VARCHAR(64) NOT NULL, event_type VARCHAR(160) NOT NULL, correlation_value VARCHAR(512) NOT NULL, payload_json JSON NOT NULL, received_at DATETIME(6) NOT NULL, consumed_instance_id VARCHAR(64), consumed_at DATETIME(6), PRIMARY KEY (tenant_id, event_id), INDEX idx_kora_workflow_signal_correlation (tenant_id, event_type, correlation_value(191), received_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_subscription (tenant_id VARCHAR(160) NOT NULL, instance_id VARCHAR(64) NOT NULL, step_id VARCHAR(160) NOT NULL, event_type VARCHAR(160) NOT NULL, correlation_field VARCHAR(160) NOT NULL, correlation_value VARCHAR(512) NOT NULL, on_event VARCHAR(160) NOT NULL, PRIMARY KEY (tenant_id, instance_id, step_id), INDEX idx_kora_workflow_subscription_match (tenant_id, event_type, correlation_value(191), instance_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_signal_receipt (tenant_id VARCHAR(160) NOT NULL, event_id VARCHAR(64) NOT NULL, instance_id VARCHAR(64) NOT NULL, matched BOOLEAN NOT NULL, processed_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, event_id, instance_id), INDEX idx_kora_workflow_signal_receipt_instance (tenant_id, instance_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_history (tenant_id VARCHAR(160) NOT NULL, instance_id VARCHAR(64) NOT NULL, sequence BIGINT NOT NULL, step_id VARCHAR(160) NOT NULL, kind VARCHAR(32) NOT NULL, status VARCHAR(32) NOT NULL, actor_id VARCHAR(160), detail TEXT NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (instance_id, sequence)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_preview (id VARCHAR(64) NOT NULL PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, definition_id VARCHAR(64) NOT NULL, definition_version BIGINT NOT NULL, definition_digest CHAR(64) NOT NULL, status VARCHAR(24) NOT NULL, outcome VARCHAR(160) NOT NULL, failure TEXT NOT NULL, steps_json JSON NOT NULL, created_at DATETIME(6) NOT NULL, INDEX idx_kora_workflow_preview_recent (tenant_id, created_at, id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_start_receipt (tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_digest CHAR(64) NOT NULL, instance_id VARCHAR(64) NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_reconcile_receipt (tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_digest CHAR(64) NOT NULL, result_json JSON NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_workflow_cancel_receipt (tenant_id VARCHAR(160) NOT NULL, actor_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_digest CHAR(64) NOT NULL, result_json JSON NOT NULL, created_at DATETIME(6) NOT NULL, PRIMARY KEY (tenant_id, actor_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported workflow execution dialect %q", dialect.DriverName())
	}
}
