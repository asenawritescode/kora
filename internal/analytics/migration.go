package analytics

import (
	"database/sql"
	"fmt"
	"log/slog"

	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// MigrateRollupMetrics consumes a ConfigDiff and updates _kora_analytics_daily
// and _kora_analytics_monthly to reflect field renames and doctype changes.
// Call after config activation/rollback.
func MigrateRollupMetrics(db *sql.DB, dialect kdb.Dialect, siteName string, oldDoctypes, newDoctypes []*doctype.DocType) error {
	if db == nil {
		return nil
	}

	diff := doctype.DiffConfigs(oldDoctypes, newDoctypes)
	if len(diff.Changes) == 0 {
		return nil
	}

	for _, change := range diff.Changes {
		switch change.Type {
		case doctype.ChangeFieldRenamed:
			if err := migrateFieldRename(db, dialect, siteName, change); err != nil {
				return fmt.Errorf("migrating rollups for %s field rename: %w", change.DocType, err)
			}
			slog.Info("analytics: migrated rollup data after field rename", "doctype", change.DocType, "old_field", change.OldValue, "new_field", change.NewValue)

		case doctype.ChangeDocTypeRemoved:
			// Mark all metrics for this doctype as archived.
			if err := archiveDoctypeMetrics(db, dialect, siteName, change.DocType); err != nil {
				return fmt.Errorf("archiving metrics for removed doctype %s: %w", change.DocType, err)
			}
		}
	}

	return nil
}

// migrateFieldRename updates metric names and dimension prefixes after a field rename.
// Example: status → state → updates "customer_count_by_status" → "customer_count_by_state"
// and dimension "status=Active" → "state=Active".
func migrateFieldRename(db *sql.DB, dialect kdb.Dialect, siteName string, change doctype.ConfigChange) error {
	oldField := change.OldValue
	newField := change.NewValue
	doctypeName := change.DocType

	// Build the old and new metric name suffixes.
	oldSuffix := "_by_" + metricName(oldField)
	newSuffix := "_by_" + metricName(newField)

	// Update _kora_analytics_daily: metric names.
	_, err := db.Exec(
		kdb.Rebind(dialect, `UPDATE _kora_analytics_daily
		 SET metric = REPLACE(metric, ?, ?)
		 WHERE site = ? AND doctype = ? AND metric LIKE ?`),
		oldSuffix, newSuffix, siteName, doctypeName, "%"+oldSuffix,
	)
	if err != nil {
		return fmt.Errorf("updating daily metric names: %w", err)
	}

	// Update _kora_analytics_daily: dimension prefixes.
	oldDimPrefix := oldField + "="
	newDimPrefix := newField + "="
	_, err = db.Exec(
		kdb.Rebind(dialect, `UPDATE _kora_analytics_daily
		 SET dimension = REPLACE(dimension, ?, ?)
		 WHERE site = ? AND doctype = ? AND dimension LIKE ?`),
		oldDimPrefix, newDimPrefix, siteName, doctypeName, oldDimPrefix+"%",
	)
	if err != nil {
		return fmt.Errorf("updating daily dimension prefixes: %w", err)
	}

	// Same for monthly.
	_, err = db.Exec(
		kdb.Rebind(dialect, `UPDATE _kora_analytics_monthly
		 SET metric = REPLACE(metric, ?, ?)
		 WHERE site = ? AND doctype = ? AND metric LIKE ?`),
		oldSuffix, newSuffix, siteName, doctypeName, "%"+oldSuffix,
	)
	if err != nil {
		return fmt.Errorf("updating monthly metric names: %w", err)
	}

	_, err = db.Exec(
		kdb.Rebind(dialect, `UPDATE _kora_analytics_monthly
		 SET dimension = REPLACE(dimension, ?, ?)
		 WHERE site = ? AND doctype = ? AND dimension LIKE ?`),
		oldDimPrefix, newDimPrefix, siteName, doctypeName, oldDimPrefix+"%",
	)
	if err != nil {
		return fmt.Errorf("updating monthly dimension prefixes: %w", err)
	}

	return nil
}

// archiveDoctypeMetrics marks all rollup rows for a deleted doctype.
func archiveDoctypeMetrics(db *sql.DB, dialect kdb.Dialect, siteName, doctypeName string) error {
	appendExpr := "CONCAT(dimension, ' [archived]')"
	if dialect.DriverName() != "mysql" {
		appendExpr = "dimension || ' [archived]'"
	}
	for _, table := range []string{"_kora_analytics_daily", "_kora_analytics_monthly"} {
		_, err := db.Exec(
			kdb.Rebind(dialect, fmt.Sprintf("UPDATE %s SET dimension = %s WHERE site = ? AND doctype = ?", table, appendExpr)),
			siteName, doctypeName,
		)
		if err != nil {
			return fmt.Errorf("archiving %s: %w", table, err)
		}
	}
	return nil
}
