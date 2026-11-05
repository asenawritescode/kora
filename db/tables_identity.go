package db

// IdentityTablesMySQL returns the MySQL DDL for external sign-in identity links.
func IdentityTablesMySQL() []string {
	return []string{
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

// IdentityTablesPostgres returns the PostgreSQL DDL for external sign-in identity links.
func IdentityTablesPostgres() []string {
	return []string{
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

// IdentityTablesLibSQL returns the LibSQL DDL for external sign-in identity links.
func IdentityTablesLibSQL() []string {
	return []string{
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
