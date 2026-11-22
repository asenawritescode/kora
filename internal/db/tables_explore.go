package db

// SavedReportTablesMySQL returns the MySQL DDL for the saved Explore reports of a site. The question is
// JSON text. name_key is the name lower-cased with its ends trimmed, unique per site.
func SavedReportTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_saved_report (
			id VARCHAR(64) PRIMARY KEY,
			name VARCHAR(200) NOT NULL,
			name_key VARCHAR(200) NOT NULL,
			visibility VARCHAR(16) NOT NULL,
			owner VARCHAR(160) NOT NULL DEFAULT '',
			question TEXT NOT NULL,
			record_types TEXT NOT NULL,
			broken_reason VARCHAR(500) NOT NULL DEFAULT '',
			broken_at DATETIME(6) NULL,
			created DATETIME(6) NOT NULL,
			modified DATETIME(6) NOT NULL,
			UNIQUE KEY uq_kora_saved_report_name (name_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

// SavedReportTablesPostgres returns the PostgreSQL DDL for the saved Explore reports of a site.
func SavedReportTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_saved_report (
			id VARCHAR(64) PRIMARY KEY,
			name VARCHAR(200) NOT NULL,
			name_key VARCHAR(200) NOT NULL,
			visibility VARCHAR(16) NOT NULL,
			owner VARCHAR(160) NOT NULL DEFAULT '',
			question TEXT NOT NULL,
			record_types TEXT NOT NULL,
			broken_reason VARCHAR(500) NOT NULL DEFAULT '',
			broken_at TIMESTAMP NULL,
			created TIMESTAMP NOT NULL,
			modified TIMESTAMP NOT NULL,
			UNIQUE (name_key)
		)`,
	}
}
