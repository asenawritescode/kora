package db

// KernelTablesMySQL returns the MySQL DDL for the operation kernel system
// tables (KERNEL-006, KERNEL-007): the idempotency receipt store (RFC §8.2)
// and the unified operation audit ledger. Both are written inside the same
// SQL transaction as the business mutation so that a commit implies exactly
// one receipt, one audit row, and the outbox events for that operation.
func KernelTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_idempotency_receipt (
			site VARCHAR(140) NOT NULL DEFAULT '',
			idempotency_key VARCHAR(255) NOT NULL,
			operation_id VARCHAR(26) NOT NULL,
			command_name VARCHAR(190) NOT NULL,
			payload_hash VARCHAR(64) NOT NULL,
			result_hash VARCHAR(64) NOT NULL DEFAULT '',
			result_json MEDIUMTEXT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'completed',
			actor_user VARCHAR(140) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, idempotency_key),
			INDEX idx_idem_operation (operation_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_operation_audit (
			id VARCHAR(26) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			operation_id VARCHAR(26) NOT NULL,
			correlation_id VARCHAR(64) NOT NULL DEFAULT '',
			causation_id VARCHAR(64) NOT NULL DEFAULT '',
			source VARCHAR(30) NOT NULL DEFAULT '',
			principal_type VARCHAR(20) NOT NULL DEFAULT '',
			principal_id VARCHAR(190) NOT NULL DEFAULT '',
			actor_user VARCHAR(140) NOT NULL DEFAULT '',
			actor_roles TEXT,
			command_name VARCHAR(190) NOT NULL,
			doctype VARCHAR(140) NOT NULL DEFAULT '',
			doc_name VARCHAR(255) NOT NULL DEFAULT '',
			prior_modified DATETIME(6),
			new_modified DATETIME(6),
			status VARCHAR(20) NOT NULL DEFAULT 'completed',
			error_code VARCHAR(60) NOT NULL DEFAULT '',
			payload_hash VARCHAR(64) NOT NULL DEFAULT '',
			before_hash VARCHAR(64) NOT NULL DEFAULT '',
			after_hash VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_audit_site_time (site, created_at),
			INDEX idx_audit_operation (operation_id),
			INDEX idx_audit_doctype_doc (doctype, doc_name),
			INDEX idx_audit_actor (actor_user)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// KernelTablesLibSQL returns the LibSQL-compatible kernel DDL.
func KernelTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_idempotency_receipt (
			site TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL,
			operation_id TEXT NOT NULL,
			command_name TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			result_hash TEXT NOT NULL DEFAULT '',
			result_json TEXT,
			status TEXT NOT NULL DEFAULT 'completed',
			actor_user TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			PRIMARY KEY (site, idempotency_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_idem_operation ON _kora_idempotency_receipt (operation_id)`,

		`CREATE TABLE IF NOT EXISTS _kora_operation_audit (
			id TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			operation_id TEXT NOT NULL,
			correlation_id TEXT NOT NULL DEFAULT '',
			causation_id TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			principal_type TEXT NOT NULL DEFAULT '',
			principal_id TEXT NOT NULL DEFAULT '',
			actor_user TEXT NOT NULL DEFAULT '',
			actor_roles TEXT,
			command_name TEXT NOT NULL,
			doctype TEXT NOT NULL DEFAULT '',
			doc_name TEXT NOT NULL DEFAULT '',
			prior_modified TEXT,
			new_modified TEXT,
			status TEXT NOT NULL DEFAULT 'completed',
			error_code TEXT NOT NULL DEFAULT '',
			payload_hash TEXT NOT NULL DEFAULT '',
			before_hash TEXT NOT NULL DEFAULT '',
			after_hash TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_site_time ON _kora_operation_audit (site, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_operation ON _kora_operation_audit (operation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_doctype_doc ON _kora_operation_audit (doctype, doc_name)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor ON _kora_operation_audit (actor_user)`,
	}
}

// KernelTablesPostgres returns the PostgreSQL-compatible kernel DDL.
// PostgreSQL is the reference dialect for these tables.
func KernelTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_idempotency_receipt" (
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"idempotency_key" VARCHAR(255) NOT NULL,
			"operation_id" VARCHAR(26) NOT NULL,
			"command_name" VARCHAR(190) NOT NULL,
			"payload_hash" VARCHAR(64) NOT NULL,
			"result_hash" VARCHAR(64) NOT NULL DEFAULT '',
			"result_json" TEXT,
			"status" VARCHAR(20) NOT NULL DEFAULT 'completed',
			"actor_user" VARCHAR(140) NOT NULL DEFAULT '',
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW(),
			PRIMARY KEY ("site", "idempotency_key")
		)`,
		`CREATE INDEX IF NOT EXISTS idx_idem_operation ON "_kora_idempotency_receipt" ("operation_id")`,

		`CREATE TABLE IF NOT EXISTS "_kora_operation_audit" (
			"id" VARCHAR(26) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL DEFAULT '',
			"operation_id" VARCHAR(26) NOT NULL,
			"correlation_id" VARCHAR(64) NOT NULL DEFAULT '',
			"causation_id" VARCHAR(64) NOT NULL DEFAULT '',
			"source" VARCHAR(30) NOT NULL DEFAULT '',
			"principal_type" VARCHAR(20) NOT NULL DEFAULT '',
			"principal_id" VARCHAR(190) NOT NULL DEFAULT '',
			"actor_user" VARCHAR(140) NOT NULL DEFAULT '',
			"actor_roles" TEXT,
			"command_name" VARCHAR(190) NOT NULL,
			"doctype" VARCHAR(140) NOT NULL DEFAULT '',
			"doc_name" VARCHAR(255) NOT NULL DEFAULT '',
			"prior_modified" TIMESTAMP,
			"new_modified" TIMESTAMP,
			"status" VARCHAR(20) NOT NULL DEFAULT 'completed',
			"error_code" VARCHAR(60) NOT NULL DEFAULT '',
			"payload_hash" VARCHAR(64) NOT NULL DEFAULT '',
			"before_hash" VARCHAR(64) NOT NULL DEFAULT '',
			"after_hash" VARCHAR(64) NOT NULL DEFAULT '',
			"created_at" TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_site_time ON "_kora_operation_audit" ("site", "created_at")`,
		`CREATE INDEX IF NOT EXISTS idx_audit_operation ON "_kora_operation_audit" ("operation_id")`,
		`CREATE INDEX IF NOT EXISTS idx_audit_doctype_doc ON "_kora_operation_audit" ("doctype", "doc_name")`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor ON "_kora_operation_audit" ("actor_user")`,
	}
}
