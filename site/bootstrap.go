package site

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
)

// isIdempotentSQLError returns true if the error is expected during
// idempotent DDL execution (CREATE IF NOT EXISTS, ALTER TABLE ADD COLUMN,
// CREATE INDEX IF NOT EXISTS). These errors are safe to ignore — the
// schema is already in the desired state.
func isIdempotentSQLError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "already exists") ||
		strings.Contains(s, "Duplicate") ||
		strings.Contains(s, "duplicate column") ||
		strings.Contains(s, "no such column") ||
		strings.Contains(s, "Unknown column") ||
		strings.Contains(s, "doesn't exist") ||
		strings.Contains(s, "Can't DROP") ||
		strings.Contains(s, "check that column") ||
		strings.Contains(s, "Error 1060") || // MySQL duplicate column
		strings.Contains(s, "Error 1062") || // MySQL duplicate key
		strings.Contains(s, "Error 1050") || // MySQL table already exists
		strings.Contains(s, "Error 1061") || // MySQL duplicate index
		(strings.Contains(s, "Error 1064") && strings.Contains(s, "near ''")) || // MySQL syntax error for empty DEFAULT on existing column
		strings.Contains(s, "Error 1064") && strings.Contains(s, "DEFAULT ''") // Same, alternate message format
}

// BootstrapSystemTables creates all _kora_* system tables if they don't exist.
// This is the single canonical implementation used by CLI setup, server startup,
// migration, and the console API. All DDL is idempotent — errors matching
// isIdempotentSQLError are silently skipped.
func BootstrapSystemTables(database *sql.DB, dialect db.Dialect) error {
	execDDL := func(ddl string, label string) error {
		if _, err := database.Exec(ddl); err != nil {
			if isIdempotentSQLError(err) {
				return nil
			}
			return fmt.Errorf("%s: %w\nSQL: %s", label, err, ddl)
		}
		return nil
	}

	for _, ddl := range dialect.SystemTableSQL() {
		if err := execDDL(ddl, "creating system table"); err != nil {
			return err
		}
	}

	var extDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		extDDL = db.ExtensibilityTablesLibSQL()
	default:
		extDDL = db.ExtensibilityTablesMySQL()
	}
	for _, ddl := range extDDL {
		if err := execDDL(ddl, "creating extensibility table"); err != nil {
			return err
		}
	}

	database.Exec(dialect.InsertOrIgnorePrefix() + ` INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`)

	return nil
}

// BootstrapPlatformRegistry creates the _kora_site_registry table on the
// platform database. This is separate from BootstrapSystemTables because the
// platform DB (used for site discovery) is distinct from per-site databases.
func BootstrapPlatformRegistry(database *sql.DB, dialect db.Dialect) error {
	for _, ddl := range dialect.SystemTableSQL() {
		if strings.Contains(ddl, "_kora_site_registry") {
			if _, err := database.Exec(ddl); err != nil {
				if isIdempotentSQLError(err) {
					return nil
				}
				return fmt.Errorf("create _kora_site_registry: %w\nSQL: %s", err, ddl)
			}
		}
	}
	return nil
}
