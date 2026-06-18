package analytics

import (
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/db"
)

// BootstrapTables creates the analytics rollup tables in the operational DB.
// Idempotent — uses IF NOT EXISTS. Called once per site at startup.
func BootstrapTables(database *sql.DB, dialect db.Dialect) error {
	for _, stmt := range rollupTableDDL(dialect) {
		if _, err := database.Exec(stmt); err != nil {
			return fmt.Errorf("analytics bootstrap: %w", err)
		}
	}
	return nil
}

func rollupTableDDL(dialect db.Dialect) []string {
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
	}
}
