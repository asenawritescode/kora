package site

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
)

// Increment this when BootstrapSystemTables gains or changes system DDL.
const systemBootstrapVersion = 5

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
	// System DDL is immutable between bootstrap versions. Record successful
	// completion per physical database so routine restarts don't replay every
	// CREATE/ALTER statement for every registered site. Write the marker only
	// after all bootstrap steps succeed.
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_system_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating system bootstrap marker: %w", err)
	}
	var currentVersion int
	err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&currentVersion)
	if err == nil {
		if currentVersion > systemBootstrapVersion {
			return fmt.Errorf("system schema version %d is newer than supported version %d", currentVersion, systemBootstrapVersion)
		}
		if currentVersion == systemBootstrapVersion {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading system bootstrap marker: %w", err)
	}

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
	case *db.PostgresDialect:
		extDDL = db.ExtensibilityTablesPostgres()
	default:
		extDDL = db.ExtensibilityTablesMySQL()
	}
	for _, ddl := range extDDL {
		if err := execDDL(ddl, "creating extensibility table"); err != nil {
			return err
		}
	}
	var conversationDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		conversationDDL = db.ConversationTablesLibSQL()
	case *db.PostgresDialect:
		conversationDDL = db.ConversationTablesPostgres()
	default:
		conversationDDL = db.ConversationTablesMySQL()
	}
	for _, ddl := range conversationDDL {
		if err := execDDL(ddl, "creating conversation table"); err != nil {
			return err
		}
	}

	// Transactional outbox (RFC §8.1).
	var outboxDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		outboxDDL = db.OutboxTablesLibSQL()
	case *db.PostgresDialect:
		outboxDDL = db.OutboxTablesPostgres()
	default:
		outboxDDL = db.OutboxTablesMySQL()
	}
	for _, ddl := range outboxDDL {
		if err := execDDL(ddl, "creating outbox table"); err != nil {
			return err
		}
	}

	// Operation kernel tables: idempotency receipts + operation audit
	// (KERNEL-006, KERNEL-007). PostgreSQL is the reference dialect.
	var kernelDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		kernelDDL = db.KernelTablesLibSQL()
	case *db.PostgresDialect:
		kernelDDL = db.KernelTablesPostgres()
	default:
		kernelDDL = db.KernelTablesMySQL()
	}
	for _, ddl := range kernelDDL {
		if err := execDDL(ddl, "creating kernel table"); err != nil {
			return err
		}
	}
	var kernelMigrationDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		kernelMigrationDDL = []string{`ALTER TABLE _kora_idempotency_receipt ADD COLUMN result_json TEXT`}
	case *db.PostgresDialect:
		kernelMigrationDDL = []string{`ALTER TABLE "_kora_idempotency_receipt" ADD COLUMN "result_json" TEXT`}
	default:
		kernelMigrationDDL = []string{`ALTER TABLE _kora_idempotency_receipt ADD COLUMN result_json MEDIUMTEXT NULL`}
	}
	for _, ddl := range kernelMigrationDDL {
		if err := execDDL(ddl, "migrating kernel tables"); err != nil {
			return err
		}
	}
	var agentDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		agentDDL = db.AgentTablesLibSQL()
	case *db.PostgresDialect:
		agentDDL = db.AgentTablesPostgres()
	default:
		agentDDL = db.AgentTablesMySQL()
	}
	for _, ddl := range agentDDL {
		if err := execDDL(ddl, "creating agent table"); err != nil {
			return err
		}
	}

	database.Exec(dialect.InsertOrIgnorePrefix() + ` INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`)

	update := fmt.Sprintf(`UPDATE _kora_system_bootstrap SET version = %s WHERE id = 1`, dialect.Placeholder(1))
	result, err := database.Exec(update, systemBootstrapVersion)
	if err != nil {
		return fmt.Errorf("updating system bootstrap marker: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking system bootstrap marker update: %w", err)
	}
	if updated == 0 {
		insert := fmt.Sprintf(`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, %s)`, dialect.Placeholder(1))
		if _, err := database.Exec(insert, systemBootstrapVersion); err != nil {
			return fmt.Errorf("writing system bootstrap marker: %w", err)
		}
	}

	return nil
}

// BootstrapPlatformRegistry creates the _kora_site_registry table on the
// platform database. This is separate from BootstrapSystemTables because the
// platform DB (used for site discovery) is distinct from per-site databases.
func BootstrapPlatformRegistry(database *sql.DB, dialect db.Dialect) error {
	return bootstrapPlatformRegistryWithLock(database, dialect)
}

func bootstrapPlatformRegistryUnlocked(database *sql.DB, dialect db.Dialect) error {
	// Directory changes live beside the canonical registry so a mutation and
	// its propagation record can commit atomically. This is intentionally a
	// separate table from each tenant's business-event outbox.
	for _, ddl := range platformDirectoryOutboxDDL(dialect) {
		if _, err := database.Exec(ddl); err != nil && !isIdempotentSQLError(err) {
			return fmt.Errorf("create site directory outbox: %w\nSQL: %s", err, ddl)
		}
	}
	for _, ddl := range dialect.SystemTableSQL() {
		if !strings.Contains(ddl, "_kora_site_registry") {
			continue
		}
		if _, err := database.Exec(ddl); err != nil {
			if isIdempotentSQLError(err) {
				continue
			}
			return fmt.Errorf("create _kora_site_registry: %w\nSQL: %s", err, ddl)
		}
	}

	// Canonical IDs are additive so existing Engine installs keep routing by
	// their current hostname while consumers migrate to site_id.
	if _, err := database.Exec(`ALTER TABLE _kora_site_registry ADD COLUMN site_id VARCHAR(64) NOT NULL DEFAULT ''`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add canonical site id: %w", err)
	}
	if _, err := database.Exec(`ALTER TABLE _kora_site_registry ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 1`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add site config revision: %w", err)
	}
	if _, err := database.Exec(`ALTER TABLE _kora_site_registry ADD COLUMN runtime_cell_id VARCHAR(80) NOT NULL DEFAULT ''`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add site runtime cell assignment: %w", err)
	}
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_site_identity_v1_snapshot (
		site VARCHAR(140) PRIMARY KEY,
		prior_site_id VARCHAR(64) NOT NULL DEFAULT '',
		domains_json TEXT,
		captured_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create site identity rollback snapshot: %w", err)
	}
	snapshotInsert := `INSERT INTO _kora_site_identity_v1_snapshot (site, prior_site_id, domains_json, captured_at)
		SELECT r.site, r.site_id, COALESCE(r.domains_json, '[]'), CURRENT_TIMESTAMP
		FROM _kora_site_registry r
		WHERE NOT EXISTS (SELECT 1 FROM _kora_site_identity_v1_snapshot b WHERE b.site = r.site)
		ON CONFLICT (site) DO NOTHING`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		snapshotInsert = `INSERT IGNORE INTO _kora_site_identity_v1_snapshot (site, prior_site_id, domains_json, captured_at)
			SELECT r.site, r.site_id, COALESCE(r.domains_json, '[]'), CURRENT_TIMESTAMP
			FROM _kora_site_registry r
			WHERE NOT EXISTS (SELECT 1 FROM _kora_site_identity_v1_snapshot b WHERE BINARY b.site = BINARY r.site)`
	}
	if _, err := database.Exec(snapshotInsert); err != nil {
		return fmt.Errorf("snapshot site identity before backfill: %w", err)
	}
	rows, err := database.Query(`SELECT site FROM _kora_site_registry WHERE site_id IS NULL OR site_id = '' ORDER BY site`)
	if err != nil {
		return fmt.Errorf("find sites missing canonical ids: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("read site missing canonical id: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate sites missing canonical ids: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close canonical id scan: %w", err)
	}
	for _, name := range names {
		id, err := newCanonicalSiteID()
		if err != nil {
			return fmt.Errorf("generate canonical site id for %s: %w", name, err)
		}
		query := fmt.Sprintf(`UPDATE _kora_site_registry SET site_id = %s WHERE site = %s AND (site_id IS NULL OR site_id = '')`, dialect.Placeholder(1), dialect.Placeholder(2))
		if _, err := database.Exec(query, id, name); err != nil {
			return fmt.Errorf("backfill canonical site id for %s: %w", name, err)
		}
	}
	indexDDL := `CREATE UNIQUE INDEX IF NOT EXISTS idx_site_registry_site_id ON _kora_site_registry (site_id)`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		indexDDL = `CREATE UNIQUE INDEX idx_site_registry_site_id ON _kora_site_registry (site_id)`
	}
	if _, err := database.Exec(indexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("create canonical site id index: %w", err)
	}
	// Eager cell startup filters by lifecycle state and assignment, then emits
	// sites in stable hostname order. This composite index keeps that query
	// scoped to the cell instead of scanning every registry row at fleet scale.
	cellIndexDDL := `CREATE INDEX IF NOT EXISTS idx_site_registry_cell_startup ON _kora_site_registry (status, runtime_cell_id, site)`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		cellIndexDDL = `CREATE INDEX idx_site_registry_cell_startup ON _kora_site_registry (status, runtime_cell_id, site)`
	}
	if _, err := database.Exec(cellIndexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("create cell startup index: %w", err)
	}
	if err := bootstrapSiteAliasIndex(database, dialect); err != nil {
		return err
	}
	return nil
}

func isAlreadyAppliedMigration(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "already exists") ||
		strings.Contains(message, "duplicate column") ||
		strings.Contains(message, "duplicate key name") ||
		strings.Contains(message, "error 1060") || // MySQL duplicate column
		strings.Contains(message, "error 1061") // MySQL duplicate index
}
