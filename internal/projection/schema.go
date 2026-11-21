package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// EnsureSchema creates the two tables that published projections need: the
// per-tenant fence and the snapshot table. The DDL matches the analytics
// bootstrap, so both paths can run against the same database.
func EnsureSchema(ctx context.Context, database *sql.DB, dialect kdb.SchemaDialect) error {
	if ctx == nil || database == nil || dialect == nil {
		return errors.New("projection schema requires context, database, and dialect")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	timestampType, dataType := "DATETIME", "LONGTEXT"
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() != "mysql" {
		timestampType, dataType = "TIMESTAMP", "TEXT"
	}
	quote := func(name string) string { return dialect.QuoteIdent(name) }
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			site VARCHAR(140) NOT NULL PRIMARY KEY,
			fence BIGINT NOT NULL DEFAULT 0
		)`, quote("_kora_analytics_rebuild_lock")),
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
		)`, quote("_kora_analytics_module_projection"), timestampType, dataType, timestampType),
	}
	for _, stmt := range statements {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("projection schema: %w", err)
		}
	}
	return nil
}
