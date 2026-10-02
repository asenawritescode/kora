package db

// ConversationTablesLibSQL returns the durable, tenant-scoped interview tables.
func ConversationTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_conversation (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, channel TEXT NOT NULL DEFAULT 'web', external_contact_id TEXT NOT NULL DEFAULT '', user_id TEXT NOT NULL DEFAULT '', current_topic TEXT NOT NULL DEFAULT 'business', status TEXT NOT NULL DEFAULT 'active', correlation_id TEXT NOT NULL, last_message_at TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kora_conversation_correlation ON _kora_conversation (tenant_id, correlation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_conversation_tenant ON _kora_conversation (tenant_id, last_message_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_conversation_message (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, actor TEXT NOT NULL, text TEXT NOT NULL, question_key TEXT NOT NULL DEFAULT '', interpretation TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_conversation_message ON _kora_conversation_message (conversation_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_interview_answer (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, question_key TEXT NOT NULL, original_text TEXT NOT NULL, structured_value TEXT NOT NULL DEFAULT '{}', confidence REAL NOT NULL DEFAULT 0, source_message_id TEXT NOT NULL DEFAULT '', confirmation_state TEXT NOT NULL DEFAULT 'pending', feedback_shown TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kora_interview_answer_key ON _kora_interview_answer (conversation_id, question_key)`,
		`CREATE TABLE IF NOT EXISTS _kora_support_ticket (id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, tenant_id TEXT NOT NULL, reason TEXT NOT NULL, safe_summary TEXT NOT NULL, correlation_id TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'created', created_at TEXT NOT NULL)`,
	}
}

func ConversationTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_conversation (id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(140) NOT NULL, channel VARCHAR(30) NOT NULL DEFAULT 'web', external_contact_id VARCHAR(255) NOT NULL DEFAULT '', user_id VARCHAR(255) NOT NULL DEFAULT '', current_topic VARCHAR(30) NOT NULL DEFAULT 'business', status VARCHAR(30) NOT NULL DEFAULT 'active', correlation_id VARCHAR(64) NOT NULL, last_message_at DATETIME(6) NOT NULL, created_at DATETIME(6) NOT NULL, UNIQUE KEY idx_kora_conversation_correlation (tenant_id, correlation_id), KEY idx_kora_conversation_tenant (tenant_id, last_message_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_conversation_message (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, actor VARCHAR(30) NOT NULL, text TEXT NOT NULL, question_key VARCHAR(100) NOT NULL DEFAULT '', interpretation TEXT NOT NULL, created_at DATETIME(6) NOT NULL, KEY idx_kora_conversation_message (conversation_id, created_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_interview_answer (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, question_key VARCHAR(100) NOT NULL, original_text TEXT NOT NULL, structured_value JSON NOT NULL, confidence DOUBLE NOT NULL DEFAULT 0, source_message_id VARCHAR(64) NOT NULL DEFAULT '', confirmation_state VARCHAR(30) NOT NULL DEFAULT 'pending', feedback_shown TEXT NOT NULL, created_at DATETIME(6) NOT NULL, UNIQUE KEY idx_kora_interview_answer_key (conversation_id, question_key)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS _kora_support_ticket (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, tenant_id VARCHAR(140) NOT NULL, reason VARCHAR(255) NOT NULL, safe_summary TEXT NOT NULL, correlation_id VARCHAR(64) NOT NULL, status VARCHAR(30) NOT NULL DEFAULT 'created', created_at DATETIME(6) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
}

func ConversationTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_conversation (id VARCHAR(64) PRIMARY KEY, tenant_id VARCHAR(140) NOT NULL, channel VARCHAR(30) NOT NULL DEFAULT 'web', external_contact_id VARCHAR(255) NOT NULL DEFAULT '', user_id VARCHAR(255) NOT NULL DEFAULT '', current_topic VARCHAR(30) NOT NULL DEFAULT 'business', status VARCHAR(30) NOT NULL DEFAULT 'active', correlation_id VARCHAR(64) NOT NULL, last_message_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kora_conversation_correlation ON _kora_conversation (tenant_id, correlation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_conversation_tenant ON _kora_conversation (tenant_id, last_message_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_conversation_message (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, actor VARCHAR(30) NOT NULL, text TEXT NOT NULL, question_key VARCHAR(100) NOT NULL DEFAULT '', interpretation TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_kora_conversation_message ON _kora_conversation_message (conversation_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_interview_answer (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, question_key VARCHAR(100) NOT NULL, original_text TEXT NOT NULL, structured_value JSONB NOT NULL DEFAULT '{}'::jsonb, confidence DOUBLE PRECISION NOT NULL DEFAULT 0, source_message_id VARCHAR(64) NOT NULL DEFAULT '', confirmation_state VARCHAR(30) NOT NULL DEFAULT 'pending', feedback_shown TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kora_interview_answer_key ON _kora_interview_answer (conversation_id, question_key)`,
		`CREATE TABLE IF NOT EXISTS _kora_support_ticket (id VARCHAR(64) PRIMARY KEY, conversation_id VARCHAR(64) NOT NULL, tenant_id VARCHAR(140) NOT NULL, reason VARCHAR(255) NOT NULL, safe_summary TEXT NOT NULL, correlation_id VARCHAR(64) NOT NULL, status VARCHAR(30) NOT NULL DEFAULT 'created', created_at TIMESTAMPTZ NOT NULL)`,
	}
}
