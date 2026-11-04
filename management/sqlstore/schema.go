package sqlstore

import (
	"fmt"

	"github.com/asenawritescode/kora/db"
)

func SchemaStatements(dialect db.Dialect) ([]string, error) {
	if dialect == nil {
		return nil, fmt.Errorf("management operation schema requires a dialect")
	}
	switch dialect.DriverName() {
	case "libsql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, caller_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_kind TEXT NOT NULL, command_digest TEXT NOT NULL, command_json TEXT NOT NULL, state TEXT NOT NULL, observed_revision TEXT, error_type TEXT, error_message TEXT, lease_owner TEXT, lease_fence INTEGER NOT NULL DEFAULT 0, lease_until TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE (tenant_id, caller_id, idempotency_key))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_managed_operation_due ON _kora_managed_operation (state, lease_until, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation_recovery (recovery_id TEXT PRIMARY KEY, operation_id TEXT NOT NULL, caller_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest TEXT NOT NULL, reason TEXT NOT NULL, created_at TEXT NOT NULL, UNIQUE (operation_id, caller_id, idempotency_key))`,
		}, nil
	case "postgres":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, caller_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_kind TEXT NOT NULL, command_digest CHAR(64) NOT NULL, command_json JSONB NOT NULL, state TEXT NOT NULL, observed_revision TEXT, error_type TEXT, error_message TEXT, lease_owner TEXT, lease_fence BIGINT NOT NULL DEFAULT 0, lease_until TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, UNIQUE (tenant_id, caller_id, idempotency_key))`,
			`CREATE INDEX IF NOT EXISTS idx_kora_managed_operation_due ON _kora_managed_operation (state, lease_until, created_at, id)`,
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation_recovery (recovery_id TEXT PRIMARY KEY, operation_id TEXT NOT NULL, caller_id TEXT NOT NULL, idempotency_key TEXT NOT NULL, command_digest CHAR(64) NOT NULL, reason TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, UNIQUE (operation_id, caller_id, idempotency_key))`,
		}, nil
	case "mysql":
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation (id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, caller_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_kind VARCHAR(48) NOT NULL, command_digest CHAR(64) NOT NULL, command_json JSON NOT NULL, state VARCHAR(32) NOT NULL, observed_revision VARCHAR(32), error_type VARCHAR(64), error_message TEXT, lease_owner VARCHAR(160), lease_fence BIGINT NOT NULL DEFAULT 0, lease_until DATETIME(6), created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, UNIQUE KEY uq_kora_managed_operation_idem (tenant_id, caller_id, idempotency_key), INDEX idx_kora_managed_operation_due (state, lease_until, created_at, id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
			`CREATE TABLE IF NOT EXISTS _kora_managed_operation_recovery (recovery_id VARCHAR(64) PRIMARY KEY, operation_id VARCHAR(64) NOT NULL, caller_id VARCHAR(160) NOT NULL, idempotency_key VARCHAR(160) NOT NULL, command_digest CHAR(64) NOT NULL, reason TEXT NOT NULL, created_at DATETIME(6) NOT NULL, UNIQUE KEY uq_kora_managed_recovery_idem (operation_id, caller_id, idempotency_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported management operation dialect %q", dialect.DriverName())
	}
}
