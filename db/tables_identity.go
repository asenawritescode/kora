package db

// IdentityTablesMySQL returns the MySQL DDL for sign-in providers and identity links.
func IdentityTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_oidc_login (
			state_hash CHAR(64) PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			provider_id VARCHAR(64) NOT NULL,
			nonce VARCHAR(128) NOT NULL,
			code_verifier VARCHAR(128) NOT NULL,
			return_to VARCHAR(1024) NOT NULL DEFAULT '',
			binding_hash CHAR(64) NOT NULL,
			expires_at DATETIME(6) NOT NULL,
			INDEX idx_oidc_login_expires (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_auth_provider (
			site VARCHAR(140) NOT NULL,
			id VARCHAR(64) NOT NULL,
			display_name VARCHAR(140) NOT NULL DEFAULT '',
			issuer VARCHAR(255) NOT NULL,
			client_id VARCHAR(255) NOT NULL,
			client_secret_key VARCHAR(140) NOT NULL,
			scopes VARCHAR(512) NOT NULL DEFAULT 'openid email profile',
			enabled TINYINT(1) NOT NULL DEFAULT 0,
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS _kora_user_identity (
			site VARCHAR(140) NOT NULL,
			user_name VARCHAR(140) NOT NULL,
			issuer VARCHAR(255) NOT NULL,
			subject VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL DEFAULT '',
			linked_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, issuer, subject),
			INDEX idx_user_identity_user (site, user_name, issuer)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// IdentityTablesPostgres returns the PostgreSQL DDL for sign-in providers and identity links.
func IdentityTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_oidc_login (
			state_hash CHAR(64) PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			provider_id VARCHAR(64) NOT NULL,
			nonce VARCHAR(128) NOT NULL,
			code_verifier VARCHAR(128) NOT NULL,
			return_to VARCHAR(1024) NOT NULL DEFAULT '',
			binding_hash CHAR(64) NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_oidc_login_expires ON _kora_oidc_login (expires_at)`,
		`CREATE TABLE IF NOT EXISTS _kora_auth_provider (
			site VARCHAR(140) NOT NULL,
			id VARCHAR(64) NOT NULL,
			display_name VARCHAR(140) NOT NULL DEFAULT '',
			issuer VARCHAR(255) NOT NULL,
			client_id VARCHAR(255) NOT NULL,
			client_secret_key VARCHAR(140) NOT NULL,
			scopes VARCHAR(512) NOT NULL DEFAULT 'openid email profile',
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, id)
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_user_identity (
			site VARCHAR(140) NOT NULL,
			user_name VARCHAR(140) NOT NULL,
			issuer VARCHAR(255) NOT NULL,
			subject VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL DEFAULT '',
			linked_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, issuer, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_identity_user ON _kora_user_identity (site, user_name, issuer)`,
	}
}

// IdentityTablesLibSQL returns the LibSQL DDL for sign-in providers and identity links.
func IdentityTablesLibSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_oidc_login (
			state_hash TEXT PRIMARY KEY,
			site TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			nonce TEXT NOT NULL,
			code_verifier TEXT NOT NULL,
			return_to TEXT NOT NULL DEFAULT '',
			binding_hash TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_auth_provider (
			site TEXT NOT NULL,
			id TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			issuer TEXT NOT NULL,
			client_id TEXT NOT NULL,
			client_secret_key TEXT NOT NULL,
			scopes TEXT NOT NULL DEFAULT 'openid email profile',
			enabled INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, id)
		)`,
		`CREATE TABLE IF NOT EXISTS _kora_user_identity (
			site TEXT NOT NULL,
			user_name TEXT NOT NULL,
			issuer TEXT NOT NULL,
			subject TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			linked_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (site, issuer, subject)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_user_identity_user ON _kora_user_identity (site, user_name, issuer)`,
	}
}
