// Package sqlstore owns durable notification event receipts and delivery rows.
package sqlstore

import (
	"fmt"

	"github.com/asenawritescode/kora/db"
)

func SchemaStatements(dialect db.Dialect) ([]string, error) {
	switch dialect.(type) {
	case *db.LibSQLDialect:
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_notification_event (
				tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, created_at TEXT NOT NULL,
				PRIMARY KEY (tenant_id, event_id)
			)`,
			`CREATE TABLE IF NOT EXISTS _kora_notification_delivery (
				id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, recipient_id TEXT NOT NULL,
				request_id TEXT NOT NULL, channel TEXT NOT NULL, event_id TEXT NOT NULL,
				status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, available_at TEXT NOT NULL,
				lease_owner TEXT NOT NULL DEFAULT '', lease_token INTEGER NOT NULL DEFAULT 0, lease_until TEXT,
				provider_reference TEXT NOT NULL DEFAULT '', accepted_at TEXT, last_error TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
				UNIQUE (tenant_id, event_id, recipient_id, channel)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_notification_due ON _kora_notification_delivery (tenant_id, status, available_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_notification_recipient ON _kora_notification_delivery (tenant_id, recipient_id, status, created_at, id)`,
		}, nil
	case *db.PostgresDialect:
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_notification_event (
				tenant_id TEXT NOT NULL, event_id TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
				PRIMARY KEY (tenant_id, event_id)
			)`,
			`CREATE TABLE IF NOT EXISTS _kora_notification_delivery (
				id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, recipient_id TEXT NOT NULL,
				request_id TEXT NOT NULL, channel TEXT NOT NULL, event_id TEXT NOT NULL,
				status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0, available_at TIMESTAMPTZ NOT NULL,
				lease_owner TEXT NOT NULL DEFAULT '', lease_token BIGINT NOT NULL DEFAULT 0, lease_until TIMESTAMPTZ,
				provider_reference TEXT NOT NULL DEFAULT '', accepted_at TIMESTAMPTZ, last_error TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL,
				UNIQUE (tenant_id, event_id, recipient_id, channel)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_notification_due ON _kora_notification_delivery (tenant_id, status, available_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_kora_notification_recipient ON _kora_notification_delivery (tenant_id, recipient_id, status, created_at, id)`,
		}, nil
	case *db.MySQLDialect:
		return []string{
			`CREATE TABLE IF NOT EXISTS _kora_notification_event (
				tenant_id VARCHAR(160) NOT NULL, event_id VARCHAR(160) NOT NULL, created_at DATETIME(6) NOT NULL,
				PRIMARY KEY (tenant_id, event_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
			`CREATE TABLE IF NOT EXISTS _kora_notification_delivery (
				id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(160) NOT NULL, recipient_id VARCHAR(160) NOT NULL,
				request_id VARCHAR(64) NOT NULL, channel VARCHAR(16) NOT NULL, event_id VARCHAR(160) NOT NULL,
				status VARCHAR(16) NOT NULL, attempts INT NOT NULL DEFAULT 0, available_at DATETIME(6) NOT NULL,
				lease_owner VARCHAR(160) NOT NULL DEFAULT '', lease_token BIGINT NOT NULL DEFAULT 0, lease_until DATETIME(6),
				provider_reference VARCHAR(512) NOT NULL DEFAULT '', accepted_at DATETIME(6), last_error VARCHAR(2048) NOT NULL DEFAULT '',
				created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
				UNIQUE KEY uq_kora_notification_delivery (tenant_id, event_id, recipient_id, channel),
				INDEX idx_kora_notification_due (tenant_id, status, available_at, id),
				INDEX idx_kora_notification_recipient (tenant_id, recipient_id, status, created_at, id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported notification database dialect %T", dialect)
	}
}
