package analytics

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/asenawritescode/kora/db"
)

// Increment this when rollupTableDDL gains or changes analytics schema DDL.
const analyticsBootstrapVersion = 1

// BootstrapTables creates the analytics rollup tables in the operational DB.
// Idempotent — uses IF NOT EXISTS. Called once per site at startup.
func BootstrapTables(database *sql.DB, dialect db.SchemaDialect) error {
	// Rollup DDL is immutable between bootstrap versions. Persist completion per
	// physical database so normal Engine starts don't issue six CREATE TABLE
	// statements for every registered site. The marker is written only after
	// all analytics tables have been created successfully.
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_analytics_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("analytics bootstrap marker: %w", err)
	}
	var currentVersion int
	err := database.QueryRow(`SELECT version FROM _kora_analytics_bootstrap WHERE id = 1`).Scan(&currentVersion)
	if err == nil {
		if currentVersion > analyticsBootstrapVersion {
			return fmt.Errorf("analytics schema version %d is newer than supported version %d", currentVersion, analyticsBootstrapVersion)
		}
		if currentVersion == analyticsBootstrapVersion {
			return nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading analytics bootstrap marker: %w", err)
	}

	for _, stmt := range rollupTableDDL(dialect) {
		if _, err := database.Exec(stmt); err != nil {
			return fmt.Errorf("analytics bootstrap: %w", err)
		}
	}
	result, err := database.Exec(`UPDATE _kora_analytics_bootstrap SET version = 1 WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("updating analytics bootstrap marker: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking analytics bootstrap marker update: %w", err)
	}
	if updated == 0 {
		if _, err := database.Exec(`INSERT INTO _kora_analytics_bootstrap (id, version) VALUES (1, 1)`); err != nil {
			return fmt.Errorf("writing analytics bootstrap marker: %w", err)
		}
	}
	return nil
}

func rollupTableDDL(dialect db.SchemaDialect) []string {
	quote := func(name string) string { return dialect.QuoteIdent(name) }

	return []string{
		// Daily aggregated metrics.
		// One row per (site, doctype, metric, dimension, date).
		// The worker UPSERTs increments into this table on every document write.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			metric VARCHAR(140) NOT NULL,
			dimension VARCHAR(255) NOT NULL DEFAULT '',
			date DATE NOT NULL,
			value DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (site, doctype, metric, dimension, date)
		)`, quote("_kora_analytics_daily")),

		// Monthly aggregated metrics.
		// Derived from daily data by the monthly rollup job (runs at midnight).
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			metric VARCHAR(140) NOT NULL,
			dimension VARCHAR(255) NOT NULL DEFAULT '',
			month DATE NOT NULL,
			value DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (site, doctype, metric, dimension, month)
		)`, quote("_kora_analytics_monthly")),

		// Workflow state transitions.
		// Tracks every state change for funnel + duration metrics.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			doc_name VARCHAR(140) NOT NULL,
			from_state VARCHAR(140) NOT NULL DEFAULT '',
			to_state VARCHAR(140) NOT NULL,
			entered_at DATETIME NOT NULL,
			exited_at DATETIME,
			duration_seconds INT,
			actor VARCHAR(140) NOT NULL DEFAULT '',
			INDEX idx_site_doctype_time (site, doctype, entered_at)
		)`, quote("_kora_analytics_workflow")),

		// Custom metric definitions created by users via the admin UI.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			name VARCHAR(140) NOT NULL,
			label VARCHAR(255) NOT NULL DEFAULT '',
			type VARCHAR(50) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			field_name VARCHAR(140) NOT NULL DEFAULT '',
			link_field VARCHAR(140) NOT NULL DEFAULT '',
			group_by_field VARCHAR(140) NOT NULL DEFAULT '',
			UNIQUE KEY uq_site_name (site, name)
		)`, quote("_kora_analytics_metric")),

		// Raw event log. Only populated when a DocType has analytics.track_raw_events: true.
		// Used for per-document audit trails and ad-hoc drill-down.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			site VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			doc_name VARCHAR(140) NOT NULL,
			event_type VARCHAR(20) NOT NULL,
			event_at DATETIME NOT NULL,
			field_name VARCHAR(140) NOT NULL DEFAULT '',
			old_value TEXT,
			new_value TEXT,
			actor VARCHAR(140) NOT NULL DEFAULT '',
			INDEX idx_site_doctype_time (site, doctype, event_at)
			)`, quote("_kora_analytics_events")),

		// Durable administrator-triggered rebuild jobs. The queue transports work;
		// this table remains the source of truth for status and recovery.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
				id VARCHAR(180) NOT NULL PRIMARY KEY,
				site VARCHAR(140) NOT NULL,
				doctype VARCHAR(140) NOT NULL DEFAULT '',
				from_date DATE NOT NULL,
				status VARCHAR(20) NOT NULL,
				started_at DATETIME NOT NULL,
				completed_at DATETIME,
				metrics INT NOT NULL DEFAULT 0,
				error TEXT,
				updated_at DATETIME NOT NULL,
				INDEX idx_rebuild_site_status (site, status, updated_at)
			)`, quote("_kora_analytics_rebuild_job")),
	}
}
