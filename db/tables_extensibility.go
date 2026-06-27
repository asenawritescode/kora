package db

// ExtensibilityTablesMySQL returns MySQL-specific extensibility DDL.
func ExtensibilityTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_script (
			name VARCHAR(140) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			script_type VARCHAR(50) NOT NULL DEFAULT 'doc_event',
			doctype VARCHAR(140) NOT NULL DEFAULT '',
			event VARCHAR(100) NOT NULL DEFAULT '',
			method_path VARCHAR(255) NOT NULL DEFAULT '',
			workflow_action VARCHAR(255) NOT NULL DEFAULT '',
			schedule VARCHAR(100) NOT NULL DEFAULT '',
			priority INT NOT NULL DEFAULT 10,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			run_as VARCHAR(140) NOT NULL DEFAULT '',
			timeout_ms INT NOT NULL DEFAULT 5000,
			script TEXT NOT NULL,
			compiled_at DATETIME(6),
			compile_error TEXT,
			created_by VARCHAR(140) NOT NULL DEFAULT '',
			updated_by VARCHAR(140) NOT NULL DEFAULT '',
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			INDEX idx_script_site (site),
			INDEX idx_script_doctype_event (doctype, event),
			INDEX idx_script_type (script_type),
			INDEX idx_script_active (is_active)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_script_execution (
			id VARCHAR(26) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			script_name VARCHAR(140) NOT NULL,
			script_type VARCHAR(50) NOT NULL,
			doctype VARCHAR(140) NOT NULL DEFAULT '',
			docname VARCHAR(140) NOT NULL DEFAULT '',
			event VARCHAR(100) NOT NULL DEFAULT '',
			trigger_user VARCHAR(255) NOT NULL DEFAULT '',
			duration_ms INT NOT NULL DEFAULT 0,
			status VARCHAR(20) NOT NULL DEFAULT 'success',
			error_message TEXT,
			logged_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_exec_script_name (script_name),
			INDEX idx_exec_logged_at (logged_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_extension (
			name VARCHAR(140) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			display_name VARCHAR(255) NOT NULL DEFAULT '',
			description TEXT,
			endpoint_url VARCHAR(1024) NOT NULL,
			secret VARCHAR(64) NOT NULL,
			access_token VARCHAR(64) NOT NULL DEFAULT '',
			old_secret VARCHAR(64),
			old_secret_expires_at DATETIME(6),
			secret_count INT NOT NULL DEFAULT 1,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			subscriptions JSON,
			api_permissions JSON,
			retry_schedule JSON,
			timeout_sec INT NOT NULL DEFAULT 10,
			headers JSON,
			delivery_stats JSON,
			consecutive_failures INT NOT NULL DEFAULT 0,
			installed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			last_delivery_at DATETIME(6),
			last_error TEXT,
			INDEX idx_ext_site (site),
			INDEX idx_ext_active (is_active),
			INDEX idx_ext_access_token (access_token)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_webhook_delivery (
			id VARCHAR(26) PRIMARY KEY,
			site VARCHAR(140) NOT NULL DEFAULT '',
			extension_name VARCHAR(140) NOT NULL,
			event_id VARCHAR(255) NOT NULL,
			event_type VARCHAR(255) NOT NULL,
			endpoint_url VARCHAR(1024) NOT NULL,
			status VARCHAR(50) NOT NULL DEFAULT 'pending',
			attempt INT NOT NULL DEFAULT 1,
			response_status INT,
			response_body TEXT,
			error_message TEXT,
			duration_ms INT NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			next_retry_at DATETIME(6),
			INDEX idx_deliv_extension (extension_name),
			INDEX idx_deliv_status (status),
			INDEX idx_deliv_created (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// ExtensibilityTablesLibSQL returns LibSQL-compatible extensibility DDL.
func ExtensibilityTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_script (
			name TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			script_type TEXT NOT NULL DEFAULT 'doc_event',
			doctype TEXT NOT NULL DEFAULT '',
			event TEXT NOT NULL DEFAULT '',
			method_path TEXT NOT NULL DEFAULT '',
			workflow_action TEXT NOT NULL DEFAULT '',
			schedule TEXT NOT NULL DEFAULT '',
			priority INTEGER NOT NULL DEFAULT 10,
			is_active INTEGER NOT NULL DEFAULT 1,
			run_as TEXT NOT NULL DEFAULT '',
			timeout_ms INTEGER NOT NULL DEFAULT 5000,
			script TEXT NOT NULL,
			compiled_at TEXT,
			compile_error TEXT,
			created_by TEXT NOT NULL DEFAULT '',
			updated_by TEXT NOT NULL DEFAULT '',
			creation TEXT NOT NULL DEFAULT (datetime('now')),
			modified TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE INDEX IF NOT EXISTS idx_script_site ON _kora_script (site)`,
		`CREATE INDEX IF NOT EXISTS idx_script_doctype_event ON _kora_script (doctype, event)`,
		`CREATE INDEX IF NOT EXISTS idx_script_type ON _kora_script (script_type)`,
		`CREATE INDEX IF NOT EXISTS idx_script_active ON _kora_script (is_active)`,

		`CREATE TABLE IF NOT EXISTS _kora_script_execution (
			id TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			script_name TEXT NOT NULL,
			script_type TEXT NOT NULL,
			doctype TEXT NOT NULL DEFAULT '',
			docname TEXT NOT NULL DEFAULT '',
			event TEXT NOT NULL DEFAULT '',
			trigger_user TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'success',
			error_message TEXT,
			logged_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE INDEX IF NOT EXISTS idx_exec_script_name ON _kora_script_execution (script_name)`,
		`CREATE INDEX IF NOT EXISTS idx_exec_logged_at ON _kora_script_execution (logged_at)`,

		`CREATE TABLE IF NOT EXISTS _kora_extension (
			name TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			description TEXT,
			endpoint_url TEXT NOT NULL,
			secret TEXT NOT NULL,
			access_token TEXT NOT NULL DEFAULT '',
			old_secret TEXT,
			old_secret_expires_at TEXT,
			secret_count INTEGER NOT NULL DEFAULT 1,
			is_active INTEGER NOT NULL DEFAULT 1,
			subscriptions TEXT,
			api_permissions TEXT,
			retry_schedule TEXT,
			timeout_sec INTEGER NOT NULL DEFAULT 10,
			headers TEXT,
			delivery_stats TEXT,
			consecutive_failures INTEGER NOT NULL DEFAULT 0,
			installed_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now')),
			last_delivery_at TEXT,
			last_error TEXT
		)`,

		`CREATE INDEX IF NOT EXISTS idx_ext_site ON _kora_extension (site)`,
		`CREATE INDEX IF NOT EXISTS idx_ext_active ON _kora_extension (is_active)`,
		`CREATE INDEX IF NOT EXISTS idx_ext_access_token ON _kora_extension (access_token)`,

		`CREATE TABLE IF NOT EXISTS _kora_webhook_delivery (
			id TEXT PRIMARY KEY,
			site TEXT NOT NULL DEFAULT '',
			extension_name TEXT NOT NULL,
			event_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			endpoint_url TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempt INTEGER NOT NULL DEFAULT 1,
			response_status INTEGER,
			response_body TEXT,
			error_message TEXT,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			next_retry_at TEXT
		)`,

		// Add site column BEFORE site index (backwards compat for existing tables).
		`ALTER TABLE _kora_webhook_delivery ADD COLUMN site TEXT NOT NULL DEFAULT ''`,

		`CREATE INDEX IF NOT EXISTS idx_deliv_extension ON _kora_webhook_delivery (extension_name)`,
		`CREATE INDEX IF NOT EXISTS idx_deliv_status ON _kora_webhook_delivery (status)`,
		`CREATE INDEX IF NOT EXISTS idx_deliv_created ON _kora_webhook_delivery (created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_deliv_site ON _kora_webhook_delivery (site)`,
	}
}
