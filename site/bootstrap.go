package site

import (
	"database/sql"
	"fmt"
	"strings"
)

// BootstrapSystemTables creates all _kora_* system tables if they don't exist.
// This is the single canonical implementation used by CLI setup, server startup,
// migration, and the console API.
func BootstrapSystemTables(db *sql.DB) error {
	systemTableSQL := []string{
		`CREATE TABLE IF NOT EXISTS _kora_doctype (
			name VARCHAR(140) PRIMARY KEY,
			module VARCHAR(140) NOT NULL DEFAULT '',
			is_submittable TINYINT(1) NOT NULL DEFAULT 0,
			is_child_table TINYINT(1) NOT NULL DEFAULT 0,
			is_single TINYINT(1) NOT NULL DEFAULT 0,
			track_changes TINYINT(1) NOT NULL DEFAULT 0,
			title_field VARCHAR(140) NOT NULL DEFAULT 'name',
			search_fields VARCHAR(255) NOT NULL DEFAULT '',
			sort_field VARCHAR(140) NOT NULL DEFAULT 'modified',
			sort_order VARCHAR(4) NOT NULL DEFAULT 'DESC',
			description TEXT,
			config_json JSON,
			version INT NOT NULL DEFAULT 1,
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add version column to existing _kora_doctype tables (backwards compat).
		`ALTER TABLE _kora_doctype ADD COLUMN version INT NOT NULL DEFAULT 1`,

		`CREATE TABLE IF NOT EXISTS _kora_field (
			name VARCHAR(140) PRIMARY KEY,
			parent VARCHAR(140) NOT NULL,
			fieldname VARCHAR(140) NOT NULL,
			fieldtype VARCHAR(50) NOT NULL,
			label VARCHAR(255) NOT NULL DEFAULT '',
			options TEXT,
			reqd TINYINT(1) NOT NULL DEFAULT 0,
			unique_constraint TINYINT(1) NOT NULL DEFAULT 0,
			default_value VARCHAR(255),
			hidden TINYINT(1) NOT NULL DEFAULT 0,
			read_only TINYINT(1) NOT NULL DEFAULT 0,
			bold TINYINT(1) NOT NULL DEFAULT 0,
			in_list_view TINYINT(1) NOT NULL DEFAULT 0,
			in_standard_filter TINYINT(1) NOT NULL DEFAULT 0,
			search_index TINYINT(1) NOT NULL DEFAULT 0,
			description TEXT,
			depends_on TEXT,
			mandatory_depends_on TEXT,
			constraints_json JSON,
			renamed_from VARCHAR(140),
			linked_field VARCHAR(255) NOT NULL DEFAULT '',
			computed TEXT,
			idx INT NOT NULL DEFAULT 0,
			INDEX idx_parent (parent),
			INDEX idx_parent_fieldname (parent, fieldname)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add columns for backwards compat (idempotent — error ignored if already exists).
		`ALTER TABLE _kora_field ADD COLUMN linked_field VARCHAR(255) NOT NULL DEFAULT ''`,
		`ALTER TABLE _kora_field ADD COLUMN computed TEXT`,

		`CREATE TABLE IF NOT EXISTS _kora_role (
			name VARCHAR(140) PRIMARY KEY,
			workspace_access TINYINT(1) NOT NULL DEFAULT 1,
			description TEXT
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_permission (
			name VARCHAR(140) PRIMARY KEY,
			doctype VARCHAR(140) NOT NULL,
			role VARCHAR(140) NOT NULL,
			can_read TINYINT(1) NOT NULL DEFAULT 0,
			can_write TINYINT(1) NOT NULL DEFAULT 0,
			can_create TINYINT(1) NOT NULL DEFAULT 0,
			can_delete TINYINT(1) NOT NULL DEFAULT 0,
			can_submit TINYINT(1) NOT NULL DEFAULT 0,
			can_cancel TINYINT(1) NOT NULL DEFAULT 0,
			can_amend TINYINT(1) NOT NULL DEFAULT 0,
			can_export TINYINT(1) NOT NULL DEFAULT 0,
			can_import TINYINT(1) NOT NULL DEFAULT 0,
			can_report TINYINT(1) NOT NULL DEFAULT 0,
			if_owner TINYINT(1) NOT NULL DEFAULT 0,
			UNIQUE KEY idx_doctype_role (doctype, role)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_config_version (
			id VARCHAR(36) PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			version INT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			created_by VARCHAR(140) NOT NULL DEFAULT 'system',
			label VARCHAR(255) NOT NULL DEFAULT '',
			changelog JSON,
			status VARCHAR(20) NOT NULL DEFAULT 'Draft',
			config JSON,
			INDEX idx_site_status (site, status),
			INDEX idx_site_version (site, version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		// Add status and is_active columns for backwards compat.
		`ALTER TABLE _kora_config_version ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'Superseded'`,
		`ALTER TABLE _kora_config_version ADD COLUMN is_active TINYINT(1) NOT NULL DEFAULT 0`,
		`UPDATE _kora_config_version SET status = 'Active' WHERE is_active = 1 AND status = 'Superseded'`,

		`CREATE TABLE IF NOT EXISTS _kora_user (
			name VARCHAR(140) PRIMARY KEY,
			email VARCHAR(255) NOT NULL DEFAULT '',
			password_hash VARCHAR(255) NOT NULL,
			full_name VARCHAR(255) NOT NULL DEFAULT '',
			enabled TINYINT(1) NOT NULL DEFAULT 1,
			roles TEXT,
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			UNIQUE KEY idx_email (email)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_session (
			sid VARCHAR(255) PRIMARY KEY,
			user VARCHAR(140) NOT NULL,
			data JSON,
			expires_at DATETIME(6) NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			INDEX idx_user (user),
			INDEX idx_expires (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow (
			name VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow_state (
			name VARCHAR(255) NOT NULL,
			workflow VARCHAR(140) NOT NULL,
			label VARCHAR(140) NOT NULL,
			is_initial TINYINT(1) NOT NULL DEFAULT 0,
			doc_status TINYINT(1) NOT NULL DEFAULT 0,
			color VARCHAR(20) NOT NULL DEFAULT '',
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow_transition (
			name VARCHAR(255) NOT NULL,
			workflow VARCHAR(140) NOT NULL,
			from_state VARCHAR(255) NOT NULL,
			to_state VARCHAR(255) NOT NULL,
			label VARCHAR(140) NOT NULL,
			allowed_role VARCHAR(255) NOT NULL DEFAULT '',
			condition_expr TEXT,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_secret (
			site VARCHAR(140) NOT NULL,
			key_name VARCHAR(140) NOT NULL,
			encrypted_value BLOB NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, key_name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}

	for _, ddl := range systemTableSQL {
		if _, err := db.Exec(ddl); err != nil {
			// Ignore duplicate column errors and unknown column errors
			// (for idempotent ALTER TABLE and migration UPDATE statements).
			errStr := err.Error()
			if strings.Contains(errStr, "Duplicate column") ||
				strings.Contains(errStr, "Unknown column") {
				continue
			}
			return fmt.Errorf("creating system table: %w\nSQL: %s", err, ddl)
		}
	}

	// Insert Administrator role if not exists.
	db.Exec(`INSERT IGNORE INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`)

	return nil
}
