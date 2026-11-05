package db

// PaymentTablesMySQL returns the MySQL DDL for the engine-owned payment
// intent store. Idempotency keys are unique per site, and a provider's request
// id is unique per (site, provider) so asynchronous confirmations resolve to
// exactly one intent.
func PaymentTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_payment_intent (
			id VARCHAR(26) PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			provider VARCHAR(40) NOT NULL,
			state VARCHAR(16) NOT NULL,
			amount DECIMAL(20,6) NOT NULL,
			currency VARCHAR(10) NOT NULL DEFAULT '',
			contact VARCHAR(64) NOT NULL DEFAULT '',
			source_doctype VARCHAR(140) NOT NULL DEFAULT '',
			idempotency_key VARCHAR(255) NOT NULL,
			payload_hash VARCHAR(64) NOT NULL,
			provider_request_id VARCHAR(190) NULL,
			provider_reference VARCHAR(190) NULL,
			failure_reason TEXT,
			actor_user VARCHAR(140) NOT NULL DEFAULT '',
			consumed_by VARCHAR(26) NULL,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			UNIQUE KEY uq_payment_idem (site, idempotency_key),
			UNIQUE KEY uq_payment_provider_req (site, provider, provider_request_id),
			INDEX idx_payment_state (site, state, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// PaymentTablesLibSQL returns the LibSQL-compatible payment intent DDL.
func PaymentTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_payment_intent (
			id TEXT PRIMARY KEY,
			site TEXT NOT NULL,
			provider TEXT NOT NULL,
			state TEXT NOT NULL,
			amount TEXT NOT NULL,
			currency TEXT NOT NULL DEFAULT '',
			contact TEXT NOT NULL DEFAULT '',
			source_doctype TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			provider_request_id TEXT,
			provider_reference TEXT,
			failure_reason TEXT,
			actor_user TEXT NOT NULL DEFAULT '',
			consumed_by TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_idem ON _kora_payment_intent (site, idempotency_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_req ON _kora_payment_intent (site, provider, provider_request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_state ON _kora_payment_intent (site, state, created_at)`,
	}
}

// PaymentTablesPostgres returns the PostgreSQL payment intent DDL.
func PaymentTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_payment_intent" (
			"id" VARCHAR(26) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL,
			"provider" VARCHAR(40) NOT NULL,
			"state" VARCHAR(16) NOT NULL,
			"amount" NUMERIC(20,6) NOT NULL,
			"currency" VARCHAR(10) NOT NULL DEFAULT '',
			"contact" VARCHAR(64) NOT NULL DEFAULT '',
			"source_doctype" VARCHAR(140) NOT NULL DEFAULT '',
			"idempotency_key" VARCHAR(255) NOT NULL,
			"payload_hash" VARCHAR(64) NOT NULL,
			"provider_request_id" VARCHAR(190),
			"provider_reference" VARCHAR(190),
			"failure_reason" TEXT,
			"actor_user" VARCHAR(140) NOT NULL DEFAULT '',
			"consumed_by" VARCHAR(26),
			"created_at" TIMESTAMP NOT NULL,
			"updated_at" TIMESTAMP NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_idem ON "_kora_payment_intent" ("site", "idempotency_key")`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_provider_req ON "_kora_payment_intent" ("site", "provider", "provider_request_id")`,
		`CREATE INDEX IF NOT EXISTS idx_payment_state ON "_kora_payment_intent" ("site", "state", "created_at")`,
	}
}
