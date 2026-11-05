package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/db"
	notificationstore "github.com/asenawritescode/kora/notifications/sqlstore"
	"github.com/asenawritescode/kora/requests/sqlstore"
	workflowsql "github.com/asenawritescode/kora/workflow/sqlstore"
)

// Increment this when BootstrapSystemTables gains or changes system DDL.
const systemBootstrapVersion = 31

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
	return BootstrapSystemTablesContext(context.Background(), database, dialect)
}

// BootstrapSystemTablesContext creates the tenant's system schema while
// observing cancellation from its lifecycle owner.
func BootstrapSystemTablesContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if ctx == nil {
		return errors.New("system bootstrap requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if database == nil || dialect == nil {
		return errors.New("system bootstrap requires database and dialect")
	}
	// System DDL is immutable between bootstrap versions. Record successful
	// completion per physical database so routine restarts don't replay every
	// CREATE/ALTER statement for every registered site. Write the marker only
	// after all bootstrap steps succeed.
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_system_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating system bootstrap marker: %w", err)
	}
	var currentVersion int
	err := database.QueryRowContext(ctx, `SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&currentVersion)
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
		if _, err := database.ExecContext(ctx, ddl); err != nil {
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

	var identityDDL []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		identityDDL = db.IdentityTablesLibSQL()
	case *db.PostgresDialect:
		identityDDL = db.IdentityTablesPostgres()
	default:
		identityDDL = db.IdentityTablesMySQL()
	}
	for _, ddl := range identityDDL {
		if err := execDDL(ddl, "creating identity table"); err != nil {
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
	if len(outboxDDL) > 0 {
		if err := execDDL(outboxDDL[0], "creating outbox table"); err != nil {
			return err
		}
	}
	var outboxSequenceMigration []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		outboxSequenceMigration = []string{
			`ALTER TABLE _kora_outbox ADD COLUMN source_sequence INTEGER NOT NULL DEFAULT 0`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_source_sequence ON _kora_outbox (site, source_sequence)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_status_created_at ON _kora_outbox (status, created_at)`,
		}
	case *db.PostgresDialect:
		outboxSequenceMigration = []string{
			`ALTER TABLE "_kora_outbox" ADD COLUMN IF NOT EXISTS "source_sequence" BIGINT NOT NULL DEFAULT 0`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_source_sequence ON "_kora_outbox" ("site", "source_sequence")`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_status_created_at ON "_kora_outbox" ("status", "created_at")`,
		}
	default:
		outboxSequenceMigration = []string{
			`ALTER TABLE _kora_outbox ADD COLUMN source_sequence BIGINT NOT NULL DEFAULT 0`,
			`CREATE INDEX idx_outbox_source_sequence ON _kora_outbox (site, source_sequence)`,
			`CREATE INDEX idx_outbox_status_created_at ON _kora_outbox (status, created_at)`,
		}
	}
	for _, ddl := range outboxSequenceMigration {
		if err := execDDL(ddl, "migrating outbox tenant source sequence"); err != nil {
			return err
		}
	}
	// Existing outbox tables may predate source_sequence. Add that column
	// before dialect DDL creates the source-sequence index. The first statement
	// above is only CREATE TABLE IF NOT EXISTS, so it cannot upgrade old tables.
	for _, ddl := range outboxDDL[1:] {
		if err := execDDL(ddl, "creating outbox index or supporting table"); err != nil {
			return err
		}
	}
	var outboxReceiptSequenceMigration string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		outboxReceiptSequenceMigration = `ALTER TABLE _kora_outbox_receipt ADD COLUMN source_sequence INTEGER NULL`
	case *db.PostgresDialect:
		outboxReceiptSequenceMigration = `ALTER TABLE "_kora_outbox_receipt" ADD COLUMN IF NOT EXISTS "source_sequence" BIGINT NULL`
	default:
		outboxReceiptSequenceMigration = `ALTER TABLE _kora_outbox_receipt ADD COLUMN source_sequence BIGINT NULL`
	}
	if _, err := database.ExecContext(ctx, outboxReceiptSequenceMigration); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("migrate outbox consumer receipt source sequence: %w", err)
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
	var kernelReceiptLookupIndex string
	switch dialect.(type) {
	case *db.MySQLDialect:
		kernelReceiptLookupIndex = `CREATE INDEX idx_idem_key ON _kora_idempotency_receipt (idempotency_key)`
	default:
		kernelReceiptLookupIndex = `CREATE INDEX IF NOT EXISTS idx_idem_key ON _kora_idempotency_receipt (idempotency_key)`
	}
	if err := execDDL(kernelReceiptLookupIndex, "migrating stable-tenant idempotency receipt lookup index"); err != nil {
		return err
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
	requestDDL, err := sqlstore.SchemaStatements(dialect)
	if err != nil {
		return fmt.Errorf("building request schema: %w", err)
	}
	for _, ddl := range requestDDL {
		if err := execDDL(ddl, "creating request table or index"); err != nil {
			return err
		}
	}
	// MySQL only creates inline indexes with CREATE TABLE. This explicit
	// additive migration also upgrades existing request tables whose CREATE
	// TABLE IF NOT EXISTS statement is necessarily a no-op.
	var requestWorkflowSourceIndex string
	switch dialect.(type) {
	case *db.MySQLDialect:
		requestWorkflowSourceIndex = `CREATE INDEX idx_kora_request_workflow_source ON _kora_request (tenant_id, source_instance_id, source_step_id, status, updated_at)`
	default:
		requestWorkflowSourceIndex = `CREATE INDEX IF NOT EXISTS idx_kora_request_workflow_source ON _kora_request (tenant_id, source_instance_id, source_step_id, status, updated_at)`
	}
	if err := execDDL(requestWorkflowSourceIndex, "migrating workflow request source index"); err != nil {
		return err
	}
	workflowDefinitionDDL, err := workflowsql.DefinitionSchemaStatements(dialect)
	if err != nil {
		return fmt.Errorf("building workflow definition schema: %w", err)
	}
	workflowExecutionDDL, err := workflowsql.ExecutionSchemaStatements(dialect)
	if err != nil {
		return fmt.Errorf("building workflow execution schema: %w", err)
	}
	for _, ddl := range append(workflowDefinitionDDL, workflowExecutionDDL...) {
		if err := execDDL(ddl, "creating durable workflow table or index"); err != nil {
			return err
		}
	}
	var requestAssignmentModeMigration string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		requestAssignmentModeMigration = `ALTER TABLE _kora_request ADD COLUMN assignment_mode TEXT NOT NULL DEFAULT 'any'`
	case *db.PostgresDialect:
		requestAssignmentModeMigration = `ALTER TABLE _kora_request ADD COLUMN assignment_mode TEXT NOT NULL DEFAULT 'any'`
	default:
		requestAssignmentModeMigration = `ALTER TABLE _kora_request ADD COLUMN assignment_mode VARCHAR(24) NOT NULL DEFAULT 'any'`
	}
	if err := execDDL(requestAssignmentModeMigration, "migrating request assignment mode"); err != nil {
		return err
	}
	var requestHistoryAssigneeMigration []string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		requestHistoryAssigneeMigration = []string{
			`ALTER TABLE _kora_request_history ADD COLUMN previous_assignee_json TEXT`,
			`ALTER TABLE _kora_request_history ADD COLUMN new_assignee_json TEXT`,
		}
	case *db.PostgresDialect:
		requestHistoryAssigneeMigration = []string{
			`ALTER TABLE _kora_request_history ADD COLUMN previous_assignee_json JSONB`,
			`ALTER TABLE _kora_request_history ADD COLUMN new_assignee_json JSONB`,
		}
	default:
		requestHistoryAssigneeMigration = []string{
			`ALTER TABLE _kora_request_history ADD COLUMN previous_assignee_json JSON NULL`,
			`ALTER TABLE _kora_request_history ADD COLUMN new_assignee_json JSON NULL`,
		}
	}
	for _, ddl := range requestHistoryAssigneeMigration {
		if err := execDDL(ddl, "migrating reassignment history"); err != nil {
			return err
		}
	}
	notificationDDL, err := notificationstore.SchemaStatements(dialect)
	if err != nil {
		return fmt.Errorf("building notification schema: %w", err)
	}
	for _, ddl := range notificationDDL {
		if err := execDDL(ddl, "creating notification table or index"); err != nil {
			return err
		}
	}
	for _, ddl := range configstore.ActivationJournalSchema(dialect) {
		if err := execDDL(ddl, "creating config activation recovery journal"); err != nil {
			return err
		}
	}
	for _, ddl := range configstore.ManagementActivationFenceSchema(dialect) {
		if err := execDDL(ddl, "creating management activation fencing state"); err != nil {
			return err
		}
	}
	// Policy and membership epochs let transactional consumers reject
	// authorization snapshots that changed after the policy check. The row is
	// tenant-database scoped and initialized idempotently for existing installs.
	var authRevisionDDL string
	switch dialect.(type) {
	case *db.LibSQLDialect:
		authRevisionDDL = `CREATE TABLE IF NOT EXISTS _kora_auth_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1), policy_revision BIGINT NOT NULL,
			membership_revision BIGINT NOT NULL)`
	case *db.PostgresDialect:
		authRevisionDDL = `CREATE TABLE IF NOT EXISTS _kora_auth_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1), policy_revision BIGINT NOT NULL,
			membership_revision BIGINT NOT NULL)`
	default:
		authRevisionDDL = `CREATE TABLE IF NOT EXISTS _kora_auth_revision (
			id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL,
			membership_revision BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`
	}
	if err := execDDL(authRevisionDDL, "creating authorization revision state"); err != nil {
		return err
	}
	seedAuthRevision := dialect.InsertOrIgnorePrefix() + ` INTO _kora_auth_revision (id, policy_revision, membership_revision) VALUES (1, 1, 1)`
	if err := execDDL(seedAuthRevision, "initializing authorization revision state"); err != nil {
		return err
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dialect) {
		if err := execDDL(ddl, "creating tenant config revision state"); err != nil {
			return err
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dialect); err != nil {
		return err
	}

	if err := seedDefaultAdministratorRoleContext(ctx, database, dialect); err != nil {
		return err
	}
	if err := auth.BackfillExtensionTokenHashes(ctx, database, dialect); err != nil {
		return err
	}

	update := fmt.Sprintf(`UPDATE _kora_system_bootstrap SET version = %s WHERE id = 1`, dialect.Placeholder(1))
	result, err := database.ExecContext(ctx, update, systemBootstrapVersion)
	if err != nil {
		return fmt.Errorf("updating system bootstrap marker: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking system bootstrap marker update: %w", err)
	}
	if updated == 0 {
		insert := fmt.Sprintf(`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, %s)`, dialect.Placeholder(1))
		if _, err := database.ExecContext(ctx, insert, systemBootstrapVersion); err != nil {
			return fmt.Errorf("writing system bootstrap marker: %w", err)
		}
	}

	return nil
}

func seedDefaultAdministratorRole(database *sql.DB, dialect db.Dialect) error {
	return seedDefaultAdministratorRoleContext(context.Background(), database, dialect)
}

func seedDefaultAdministratorRoleContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if database == nil || dialect == nil {
		return fmt.Errorf("seeding default administrator role requires database and dialect")
	}
	if ctx == nil {
		return errors.New("seeding default administrator role requires context")
	}
	query := dialect.InsertOrIgnorePrefix() + ` INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`
	if _, err := database.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("seeding default administrator role: %w", err)
	}
	return nil
}

// BootstrapPlatformRegistry creates the _kora_site_registry table on the
// platform database. This is separate from BootstrapSystemTables because the
// platform DB (used for site discovery) is distinct from per-site databases.
func BootstrapPlatformRegistry(database *sql.DB, dialect db.Dialect) error {
	return BootstrapPlatformRegistryContext(context.Background(), database, dialect)
}

// BootstrapPlatformRegistryContext initializes the Engine directory with a
// caller-owned deadline, including the session lock used by MySQL/PostgreSQL.
func BootstrapPlatformRegistryContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	return bootstrapPlatformRegistryWithLockContext(ctx, database, dialect)
}

func bootstrapPlatformRegistryUnlockedContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	// Directory changes live beside the canonical registry so a mutation and
	// its propagation record can commit atomically. This is intentionally a
	// separate table from each tenant's business-event outbox.
	for _, ddl := range platformDirectoryOutboxDDL(dialect) {
		if _, err := database.ExecContext(ctx, ddl); err != nil && !isIdempotentSQLError(err) {
			return fmt.Errorf("create site directory outbox: %w\nSQL: %s", err, ddl)
		}
	}
	for _, ddl := range dialect.SystemTableSQL() {
		if !strings.Contains(ddl, "_kora_site_registry") {
			continue
		}
		if _, err := database.ExecContext(ctx, ddl); err != nil {
			if isIdempotentSQLError(err) {
				continue
			}
			return fmt.Errorf("create _kora_site_registry: %w\nSQL: %s", err, ddl)
		}
	}

	// Canonical IDs are additive so existing Engine installs keep routing by
	// their current hostname while consumers migrate to site_id.
	if _, err := database.ExecContext(ctx, `ALTER TABLE _kora_site_registry ADD COLUMN site_id VARCHAR(64) NOT NULL DEFAULT ''`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add canonical site id: %w", err)
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE _kora_site_registry ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 1`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add site config revision: %w", err)
	}
	if _, err := database.ExecContext(ctx, `ALTER TABLE _kora_site_registry ADD COLUMN runtime_cell_id VARCHAR(80) NOT NULL DEFAULT ''`); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("add site runtime cell assignment: %w", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_site_identity_v1_snapshot (
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
	if _, err := database.ExecContext(ctx, snapshotInsert); err != nil {
		return fmt.Errorf("snapshot site identity before backfill: %w", err)
	}
	rows, err := database.QueryContext(ctx, `SELECT site FROM _kora_site_registry WHERE site_id IS NULL OR site_id = '' ORDER BY site`)
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
		if _, err := database.ExecContext(ctx, query, id, name); err != nil {
			return fmt.Errorf("backfill canonical site id for %s: %w", name, err)
		}
	}
	indexDDL := `CREATE UNIQUE INDEX IF NOT EXISTS idx_site_registry_site_id ON _kora_site_registry (site_id)`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		indexDDL = `CREATE UNIQUE INDEX idx_site_registry_site_id ON _kora_site_registry (site_id)`
	}
	if _, err := database.ExecContext(ctx, indexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("create canonical site id index: %w", err)
	}
	// Eager cell startup filters by lifecycle state and assignment, then emits
	// sites in stable hostname order. This composite index keeps that query
	// scoped to the cell instead of scanning every registry row at fleet scale.
	cellIndexDDL := `CREATE INDEX IF NOT EXISTS idx_site_registry_cell_startup ON _kora_site_registry (status, runtime_cell_id, site)`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		cellIndexDDL = `CREATE INDEX idx_site_registry_cell_startup ON _kora_site_registry (status, runtime_cell_id, site)`
	}
	if _, err := database.ExecContext(ctx, cellIndexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("create cell startup index: %w", err)
	}
	if err := bootstrapSiteAliasIndexContext(ctx, database, dialect); err != nil {
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
