package db

// aiPendingWriteSQL is the PostgreSQL form of the pending-write table. See the MySQL
// form for the row meaning and state machine.
func (d *PostgresDialect) aiPendingWriteSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_ai_pending_write" (
			"id" VARCHAR(64) PRIMARY KEY,
			"site" VARCHAR(140) NOT NULL,
			"conversation_id" VARCHAR(255) NOT NULL DEFAULT '',
			"run_id" VARCHAR(255) NOT NULL,
			"actor_id" VARCHAR(255) NOT NULL,
			"tool_name" VARCHAR(140) NOT NULL,
			"args_hash" VARCHAR(64) NOT NULL,
			"args_json" TEXT NOT NULL,
			"preview_json" TEXT NOT NULL,
			"state" VARCHAR(20) NOT NULL,
			"attempts" INTEGER NOT NULL DEFAULT 0,
			"created_at" TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			"claimed_at" TIMESTAMP(6),
			"completed_at" TIMESTAMP(6),
			"outcome" TEXT,
			"error_message" VARCHAR(255) NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS "idx_ai_pending_site_state" ON "_kora_ai_pending_write" ("site", "state")`,
	}
}
