package db

// DashboardOverrideTablesMySQL returns the MySQL DDL for the tenant's dashboard
// layout overrides: one row per dashboard, replaced whole on each save. The
// ordering column is named display_order because ORDER is a reserved word.
func DashboardOverrideTablesMySQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS _kora_dashboard_override (
			name VARCHAR(140) PRIMARY KEY,
			hidden TEXT NOT NULL,
			display_order TEXT NOT NULL,
			default_range VARCHAR(20) NOT NULL DEFAULT '',
			modified DATETIME(6) NOT NULL,
			modified_by VARCHAR(140) NOT NULL DEFAULT ''
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		// Pins arrived after the table; existing sites get the column here, fresh ones see a duplicate-column no-op.
		`ALTER TABLE _kora_dashboard_override ADD COLUMN pinned TEXT NOT NULL`,
	}
}

// DashboardOverrideTablesPostgres returns the PostgreSQL DDL for the tenant's
// dashboard layout overrides.
func DashboardOverrideTablesPostgres() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS "_kora_dashboard_override" (
			"name" VARCHAR(140) PRIMARY KEY,
			"hidden" TEXT NOT NULL,
			"display_order" TEXT NOT NULL,
			"default_range" VARCHAR(20) NOT NULL DEFAULT '',
			"modified" TIMESTAMP NOT NULL,
			"modified_by" VARCHAR(140) NOT NULL DEFAULT ''
		)`,
		`ALTER TABLE "_kora_dashboard_override" ADD COLUMN "pinned" TEXT NOT NULL DEFAULT '[]'`,
	}
}
