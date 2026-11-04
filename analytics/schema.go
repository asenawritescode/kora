package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/db"
)

// Increment this when rollupTableDDL gains or changes analytics schema DDL.
const analyticsBootstrapVersion = 7

// BootstrapTables creates the analytics rollup tables in the operational DB.
// Idempotent — uses IF NOT EXISTS. Called once per site at startup.
func BootstrapTables(database *sql.DB, dialect db.SchemaDialect) error {
	return BootstrapTablesContext(context.Background(), database, dialect)
}

// BootstrapTablesContext creates the analytics schema using the caller's
// cancellation and deadline. Engine startup passes its tenant work
// admission context here so schema work participates in the same bounded
// database-work budget as requests and workers.
func BootstrapTablesContext(ctx context.Context, database *sql.DB, dialect db.SchemaDialect) error {
	if ctx == nil {
		return errors.New("analytics bootstrap requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if database == nil || dialect == nil {
		return errors.New("analytics bootstrap requires database and dialect")
	}
	// Analytics storage uses one fresh schema. Older layouts are intentionally
	// rejected: the module architecture does not migrate preexisting records.
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("analytics bootstrap marker: %w", err)
	}
	var currentVersion int
	err := database.QueryRowContext(ctx, `SELECT version FROM _kora_analytics_bootstrap WHERE id = 1`).Scan(&currentVersion)
	if err == nil {
		if currentVersion > analyticsBootstrapVersion {
			return fmt.Errorf("analytics schema version %d is newer than supported version %d", currentVersion, analyticsBootstrapVersion)
		}
		if currentVersion == analyticsBootstrapVersion {
			return nil
		}
		if currentVersion != 0 {
			return fmt.Errorf("analytics schema version %d is incompatible with required version %d; create a fresh tenant database", currentVersion, analyticsBootstrapVersion)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading analytics bootstrap marker: %w", err)
	}

	for _, stmt := range rollupTableDDL(dialect) {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("analytics bootstrap: %w", err)
		}
	}
	result, err := database.ExecContext(ctx, fmt.Sprintf(`UPDATE _kora_analytics_bootstrap SET version = %d WHERE id = 1`, analyticsBootstrapVersion))
	if err != nil {
		return fmt.Errorf("updating analytics bootstrap marker: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking analytics bootstrap marker update: %w", err)
	}
	if updated == 0 {
		if _, err := database.ExecContext(ctx, fmt.Sprintf(`INSERT INTO _kora_analytics_bootstrap (id, version) VALUES (1, %d)`, analyticsBootstrapVersion)); err != nil {
			return fmt.Errorf("writing analytics bootstrap marker: %w", err)
		}
	}
	return nil
}

func rollupTableDDL(dialect db.SchemaDialect) []string {
	quote := func(name string) string { return dialect.QuoteIdent(name) }
	driver := "mysql"
	if d, ok := dialect.(interface{ DriverName() string }); ok {
		driver = d.DriverName()
	}
	identity, timestampType, doubleType := "BIGINT AUTO_INCREMENT PRIMARY KEY", "DATETIME", "DOUBLE"
	projectionDataType := "LONGTEXT"
	indexClause := func(name, columns string) string { return fmt.Sprintf(", INDEX %s (%s)", name, columns) }
	var indexStatements []string
	switch driver {
	case "postgres":
		identity, timestampType, doubleType = "BIGSERIAL PRIMARY KEY", "TIMESTAMP", "DOUBLE PRECISION"
		projectionDataType = "TEXT"
		indexClause = func(name, columns string) string {
			table := analyticsIndexTable(name)
			indexStatements = append(indexStatements, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", quote(name), quote(table), columns))
			return ""
		}
	case "libsql":
		identity, timestampType, doubleType = "INTEGER PRIMARY KEY AUTOINCREMENT", "DATETIME", "REAL"
		projectionDataType = "TEXT"
		indexClause = func(name, columns string) string {
			table := analyticsIndexTable(name)
			indexStatements = append(indexStatements, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (%s)", quote(name), quote(table), columns))
			return ""
		}
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL PRIMARY KEY,
			fence BIGINT NOT NULL DEFAULT 0
		)`, quote("_kora_analytics_rebuild_lock")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL DEFAULT '',
			scope_kind VARCHAR(32) NOT NULL DEFAULT 'tenant',
			legal_entity_id VARCHAR(140) NOT NULL DEFAULT '',
			from_date VARCHAR(10) NOT NULL,
			last_source_sequence BIGINT NOT NULL DEFAULT 0,
			updated_at %s NOT NULL,
			PRIMARY KEY (site, doctype, from_date, scope_kind, legal_entity_id)
		)`, quote("_kora_analytics_rebuild_checkpoint"), timestampType),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			job_id VARCHAR(140) NOT NULL,
			owner VARCHAR(140) NOT NULL,
			fencing_token BIGINT NOT NULL,
			lease_until %s NOT NULL,
			PRIMARY KEY (site, job_id)%s
		)`, quote("_kora_analytics_rebuild_lease"), timestampType, indexClause("idx_analytics_rebuild_lease_expiry", "site, lease_until")),
		// Each event receipt is committed in the same transaction as its analytics
		// effects so replay can skip already-applied effects after a crash.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			event_id VARCHAR(140) NOT NULL,
			source_sequence BIGINT NULL,
			processed_at %s NOT NULL,
			PRIMARY KEY (site, event_id)
		)`, quote("_kora_analytics_event_receipt"), timestampType),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			snapshot_key CHAR(64) NOT NULL PRIMARY KEY,
			generation BIGINT NOT NULL,
			site VARCHAR(140) NOT NULL,
			projection_id VARCHAR(140) NOT NULL,
			owner_module_id VARCHAR(140) NOT NULL,
			scope_kind VARCHAR(32) NOT NULL,
			legal_entity_id VARCHAR(140) NOT NULL DEFAULT '',
			source_id VARCHAR(280) NOT NULL,
			source_revision VARCHAR(256) NOT NULL,
			source_cursor VARCHAR(2048) NOT NULL DEFAULT '',
			output_schema_id VARCHAR(140) NOT NULL,
			output_schema_version INTEGER NOT NULL,
			as_of %s NOT NULL,
			freshness VARCHAR(16) NOT NULL,
			data %s NOT NULL,
			updated_at %s NOT NULL
		)`, quote("_kora_analytics_module_projection"), timestampType, projectionDataType, timestampType),

		// Daily aggregated metrics.
		// One row per (site, doctype, metric, dimension, date).
		// The worker UPSERTs increments into this table on every document write.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			metric VARCHAR(140) NOT NULL,
			dimension VARCHAR(160) NOT NULL DEFAULT '',
			date DATE NOT NULL,
			scope_kind VARCHAR(32) NOT NULL DEFAULT 'tenant',
			legal_entity_id VARCHAR(140) NOT NULL DEFAULT '',
			value %s NOT NULL DEFAULT 0,
			PRIMARY KEY (site, doctype, metric, dimension, date, scope_kind, legal_entity_id)%s
		)`, quote("_kora_analytics_daily"), doubleType, indexClause("idx_analytics_daily_scope", "site, scope_kind, legal_entity_id, doctype")),

		// Monthly aggregated metrics.
		// Derived from daily data by the monthly rollup job (runs at midnight).
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			metric VARCHAR(140) NOT NULL,
			dimension VARCHAR(160) NOT NULL DEFAULT '',
			month DATE NOT NULL,
			scope_kind VARCHAR(32) NOT NULL DEFAULT 'tenant',
			legal_entity_id VARCHAR(140) NOT NULL DEFAULT '',
			value %s NOT NULL DEFAULT 0,
			PRIMARY KEY (site, doctype, metric, dimension, month, scope_kind, legal_entity_id)%s
		)`, quote("_kora_analytics_monthly"), doubleType, indexClause("idx_analytics_monthly_scope", "site, scope_kind, legal_entity_id, doctype")),

		// Workflow state transitions.
		// Tracks every state change for funnel + duration metrics.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id %s,
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			doc_name VARCHAR(140) NOT NULL,
			from_state VARCHAR(140) NOT NULL DEFAULT '',
			to_state VARCHAR(140) NOT NULL,
			entered_at %s NOT NULL,
			exited_at %s,
			duration_seconds INT,
			actor VARCHAR(140) NOT NULL DEFAULT ''%s
		)`, quote("_kora_analytics_workflow"), identity, timestampType, timestampType, indexClause("idx_analytics_workflow_site_doctype_time", "site, doctype, entered_at")),

		// Custom metric definitions created by users via the admin UI.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id %s,
			site VARCHAR(140) NOT NULL,
			name VARCHAR(140) NOT NULL,
			label VARCHAR(255) NOT NULL DEFAULT '',
			type VARCHAR(50) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			field_name VARCHAR(140) NOT NULL DEFAULT '',
			link_field VARCHAR(140) NOT NULL DEFAULT '',
			group_by_field VARCHAR(140) NOT NULL DEFAULT '',
			CONSTRAINT uq_analytics_metric_site_name UNIQUE (site, name)
		)`, quote("_kora_analytics_metric"), identity),

		// Raw event log. Only populated when a DocType has analytics.track_raw_events: true.
		// Used for per-document audit trails and ad-hoc drill-down.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id %s,
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			doc_name VARCHAR(140) NOT NULL,
			event_type VARCHAR(20) NOT NULL,
			event_at %s NOT NULL,
			field_name VARCHAR(140) NOT NULL DEFAULT '',
			old_value TEXT,
			new_value TEXT,
			actor VARCHAR(140) NOT NULL DEFAULT ''%s
			)`, quote("_kora_analytics_events"), identity, timestampType, indexClause("idx_analytics_events_site_doctype_time", "site, doctype, event_at")),

		// Durable administrator-triggered rebuild jobs. The queue transports work;
		// this table remains the source of truth for status and recovery.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				id VARCHAR(180) NOT NULL PRIMARY KEY,
				site VARCHAR(140) NOT NULL,
				doctype VARCHAR(140) NOT NULL DEFAULT '',
				from_date DATE NOT NULL,
				status VARCHAR(20) NOT NULL,
			started_at %s NOT NULL,
			completed_at %s,
				metrics INT NOT NULL DEFAULT 0,
				error TEXT,
			updated_at %s NOT NULL%s
			)`, quote("_kora_analytics_rebuild_job"), timestampType, timestampType, timestampType, indexClause("idx_analytics_rebuild_site_status", "site, status, updated_at")),
	}
	statements = append(statements, indexStatements...)
	return statements
}

func analyticsIndexTable(name string) string {
	return map[string]string{
		"idx_analytics_workflow_site_doctype_time": "_kora_analytics_workflow",
		"idx_analytics_events_site_doctype_time":   "_kora_analytics_events",
		"idx_analytics_rebuild_site_status":        "_kora_analytics_rebuild_job",
		"idx_analytics_rebuild_lease_expiry":       "_kora_analytics_rebuild_lease",
		"idx_analytics_daily_scope":                "_kora_analytics_daily",
		"idx_analytics_monthly_scope":              "_kora_analytics_monthly",
	}[name]
}
