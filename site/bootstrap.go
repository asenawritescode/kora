package site

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
)

// BootstrapSystemTables creates all _kora_* system tables if they don't exist.
// This is the single canonical implementation used by CLI setup, server startup,
// migration, and the console API.
func BootstrapSystemTables(database *sql.DB, dialect db.Dialect) error {
	for _, ddl := range dialect.SystemTableSQL() {
		if _, err := database.Exec(ddl); err != nil {
			// Ignore duplicate column errors and unknown column errors
			// (for idempotent ALTER TABLE and migration UPDATE statements).
			errStr := err.Error()
			if strings.Contains(errStr, "Duplicate") ||
				strings.Contains(errStr, "Unknown column") ||
				strings.Contains(errStr, "duplicate column") ||
				strings.Contains(errStr, "already exists") ||
				strings.Contains(errStr, "doesn't exist") ||
				strings.Contains(errStr, "Can't DROP") ||
				strings.Contains(errStr, "check that column") {
				continue
			}
			return fmt.Errorf("creating system table: %w\nSQL: %s", err, ddl)
		}
	}

	// Extensibility tables (scripts, extensions, webhooks).
	var extDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		extDDL = db.ExtensibilityTablesLibSQL()
	default:
		extDDL = db.ExtensibilityTablesMySQL()
	}
	for _, ddl := range extDDL {
		if _, err := database.Exec(ddl); err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "already exists") {
				continue
			}
			return fmt.Errorf("creating extensibility table: %w\nSQL: %s", err, ddl)
		}
	}

	// Insert Administrator role if not exists.
	database.Exec(dialect.InsertOrIgnorePrefix() + ` INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`)

	return nil
}
