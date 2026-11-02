package db

// OutboxTablesMySQL returns the MySQL DDL for the transactional outbox
// (RFC §8.1). The outbox table is written in the same SQL transaction as the
// business write so that no event can be lost between commit and publish.
func OutboxTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_outbox (
			id VARCHAR(26) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			source_sequence BIGINT NOT NULL DEFAULT 0,
			event_type VARCHAR(255) NOT NULL,
			event_version INT NOT NULL DEFAULT 1,
			aggregate_type VARCHAR(140) NOT NULL DEFAULT '',
			aggregate_id VARCHAR(255) NOT NULL DEFAULT '',
			payload JSON,
			status VARCHAR(20) NOT NULL DEFAULT 'pending',
			attempts INT NOT NULL DEFAULT 0,
			next_attempt_at DATETIME(6),
			lease_owner VARCHAR(140) NOT NULL DEFAULT '',
			lease_until DATETIME(6),
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			published_at DATETIME(6),
			last_error TEXT,
			INDEX idx_outbox_pending (status, next_attempt_at),
			INDEX idx_outbox_status_created_at (status, created_at),
			INDEX idx_outbox_site (site),
			INDEX idx_outbox_source_sequence (site, source_sequence),
			INDEX idx_outbox_aggregate (aggregate_type, aggregate_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_event_sequence (
			site VARCHAR(140) PRIMARY KEY,
			last_sequence BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_tenant_event_sequence (
			id TINYINT PRIMARY KEY,
			last_sequence BIGINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_outbox_receipt (
			consumer_name VARCHAR(140) NOT NULL,
			event_id VARCHAR(255) NOT NULL,
			site VARCHAR(140) NOT NULL DEFAULT '',
			source_sequence BIGINT NULL,
			received_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (consumer_name, event_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_outbox_delivery_failure (consumer_name VARCHAR(140) NOT NULL, event_id VARCHAR(255) NOT NULL, site VARCHAR(140) NOT NULL DEFAULT '', attempts INT NOT NULL DEFAULT 0, quarantined BOOLEAN NOT NULL DEFAULT FALSE, last_error VARCHAR(512) NOT NULL DEFAULT '', updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6), PRIMARY KEY (consumer_name, event_id), INDEX idx_outbox_delivery_quarantine (site, quarantined, updated_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_snapshot (token VARCHAR(64) PRIMARY KEY, site VARCHAR(140) NOT NULL, actor VARCHAR(255) NOT NULL, generation BIGINT NOT NULL, boundary_sequence BIGINT NOT NULL, types_json TEXT NOT NULL, expires_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_snapshot_row (token VARCHAR(64) NOT NULL, row_index BIGINT NOT NULL, doctype VARCHAR(140) NOT NULL, document JSON NOT NULL, PRIMARY KEY(token,row_index)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_cursor (token VARCHAR(64) PRIMARY KEY, site VARCHAR(140) NOT NULL, actor VARCHAR(255) NOT NULL, generation BIGINT NOT NULL, source_sequence BIGINT NOT NULL, types_json TEXT NOT NULL, expires_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// OutboxTablesLibSQL returns the LibSQL-compatible outbox DDL.
func OutboxTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_outbox (
			id TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			source_sequence INTEGER NOT NULL DEFAULT 0,
			event_type TEXT NOT NULL,
			event_version INTEGER NOT NULL DEFAULT 1,
			aggregate_type TEXT NOT NULL DEFAULT '',
			aggregate_id TEXT NOT NULL DEFAULT '',
			payload TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until TEXT,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			published_at TEXT,
			last_error TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON _kora_outbox (status, next_attempt_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_status_created_at ON _kora_outbox (status, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_site ON _kora_outbox (site)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_source_sequence ON _kora_outbox (site, source_sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_aggregate ON _kora_outbox (aggregate_type, aggregate_id)`,
		`CREATE TABLE IF NOT EXISTS _kora_event_sequence (
			site TEXT PRIMARY KEY,
			last_sequence INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_tenant_event_sequence (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			last_sequence INTEGER NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE IF NOT EXISTS _kora_outbox_receipt (
			consumer_name TEXT NOT NULL,
			event_id TEXT NOT NULL,
			site TEXT NOT NULL DEFAULT '',
			source_sequence INTEGER,
			received_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (consumer_name, event_id)
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_outbox_delivery_failure (consumer_name TEXT NOT NULL, event_id TEXT NOT NULL, site TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0, quarantined INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT (datetime('now')), PRIMARY KEY (consumer_name, event_id))`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_delivery_quarantine ON _kora_outbox_delivery_failure (site, quarantined, updated_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_snapshot (token TEXT PRIMARY KEY, site TEXT NOT NULL, actor TEXT NOT NULL, generation INTEGER NOT NULL, boundary_sequence INTEGER NOT NULL, types_json TEXT NOT NULL, expires_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_snapshot_row (token TEXT NOT NULL, row_index INTEGER NOT NULL, doctype TEXT NOT NULL, document TEXT NOT NULL, PRIMARY KEY(token,row_index))`,
		`CREATE TABLE IF NOT EXISTS _kora_offline_cursor (token TEXT PRIMARY KEY, site TEXT NOT NULL, actor TEXT NOT NULL, generation INTEGER NOT NULL, source_sequence INTEGER NOT NULL, types_json TEXT NOT NULL, expires_at TEXT NOT NULL)`,
	}
}

// OutboxTablesPostgres returns the PostgreSQL-compatible outbox DDL.
func OutboxTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_outbox" (
			"id" VARCHAR(26) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"source_sequence" BIGINT NOT NULL DEFAULT 0,
			"event_type" VARCHAR(255) NOT NULL,
			"event_version" INTEGER NOT NULL DEFAULT 1,
			"aggregate_type" VARCHAR(140) NOT NULL DEFAULT '',
			"aggregate_id" VARCHAR(255) NOT NULL DEFAULT '',
			"payload" JSONB,
			"status" VARCHAR(20) NOT NULL DEFAULT 'pending',
			"attempts" INTEGER NOT NULL DEFAULT 0,
			"next_attempt_at" TIMESTAMP,
			"lease_owner" VARCHAR(140) NOT NULL DEFAULT '',
			"lease_until" TIMESTAMP,
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			"published_at" TIMESTAMP,
			"last_error" TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_pending ON "_kora_outbox" ("status", "next_attempt_at")`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_status_created_at ON "_kora_outbox" ("status", "created_at")`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_site ON "_kora_outbox" ("site")`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_source_sequence ON "_kora_outbox" ("site", "source_sequence")`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_aggregate ON "_kora_outbox" ("aggregate_type", "aggregate_id")`,
		`CREATE TABLE IF NOT EXISTS "_kora_event_sequence" (
			"site" VARCHAR(140) PRIMARY KEY,
			"last_sequence" BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS "_kora_tenant_event_sequence" (
			"id" INTEGER PRIMARY KEY CHECK ("id" = 1),
			"last_sequence" BIGINT NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE IF NOT EXISTS "_kora_outbox_receipt" (
			"consumer_name" VARCHAR(140) NOT NULL,
			"event_id" VARCHAR(255) NOT NULL,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"source_sequence" BIGINT,
			"received_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			PRIMARY KEY ("consumer_name", "event_id")
		)`,
		`CREATE TABLE IF NOT EXISTS "_kora_outbox_delivery_failure" ("consumer_name" VARCHAR(140) NOT NULL, "event_id" VARCHAR(255) NOT NULL, "site" VARCHAR(140) NOT NULL DEFAULT '', "attempts" INTEGER NOT NULL DEFAULT 0, "quarantined" BOOLEAN NOT NULL DEFAULT FALSE, "last_error" VARCHAR(512) NOT NULL DEFAULT '', "updated_at" TIMESTAMP NOT NULL DEFAULT NOW(), PRIMARY KEY ("consumer_name", "event_id"))`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_delivery_quarantine ON "_kora_outbox_delivery_failure" ("site", "quarantined", "updated_at")`,
		`CREATE TABLE IF NOT EXISTS "_kora_offline_snapshot" ("token" VARCHAR(64) PRIMARY KEY, "site" VARCHAR(140) NOT NULL, "actor" VARCHAR(255) NOT NULL, "generation" BIGINT NOT NULL, "boundary_sequence" BIGINT NOT NULL, "types_json" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS "_kora_offline_snapshot_row" ("token" VARCHAR(64) NOT NULL, "row_index" BIGINT NOT NULL, "doctype" VARCHAR(140) NOT NULL, "document" JSONB NOT NULL, PRIMARY KEY("token","row_index"))`,
		`CREATE TABLE IF NOT EXISTS "_kora_offline_cursor" ("token" VARCHAR(64) PRIMARY KEY, "site" VARCHAR(140) NOT NULL, "actor" VARCHAR(255) NOT NULL, "generation" BIGINT NOT NULL, "source_sequence" BIGINT NOT NULL, "types_json" TEXT NOT NULL, "expires_at" TIMESTAMP NOT NULL)`,
	}
}
