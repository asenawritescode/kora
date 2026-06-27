package db

// ExtensibilityTablesSQL returns CREATE TABLE statements for extensibility features.
// These are appended to the SystemTableSQL output by each dialect.
func ExtensibilityTablesSQL() []string {
	return []string{
		// _kora_script — user-defined JavaScript hooks.
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

		// _kora_script_execution — audit log.
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

		// _kora_extension — webhook extension registry.
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

		// _kora_webhook_delivery — webhook delivery log.
		`CREATE TABLE IF NOT EXISTS _kora_webhook_delivery (
			id VARCHAR(26) PRIMARY KEY,
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
