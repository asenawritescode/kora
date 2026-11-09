package configstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/asenawritescode/kora/internal/db"
)

// ConfigRevisionSchema creates the tenant-local revision row. It is separate
// from the platform directory revision and changes only with tenant config.
func ConfigRevisionSchema(dialect db.Dialect) []string {
	switch dialect.DriverName() {
	case "postgres":
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1), revision BIGINT NOT NULL CHECK (revision >= 0))`}
	case "libsql":
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_revision (
			id INTEGER PRIMARY KEY CHECK (id = 1), revision BIGINT NOT NULL CHECK (revision >= 0))`}
	default:
		return []string{`CREATE TABLE IF NOT EXISTS _kora_config_revision (
			id INTEGER PRIMARY KEY, revision BIGINT NOT NULL CHECK (revision >= 0)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`}
	}
}

// SeedConfigRevision initializes existing tenant databases at revision 1.
// Bootstrap calls this only after creating the table; retries are idempotent.
func SeedConfigRevision(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if ctx == nil || database == nil || dialect == nil {
		return fmt.Errorf("seed config revision requires context, database, and dialect")
	}
	query := dialect.InsertOrIgnorePrefix() + ` INTO _kora_config_revision (id, revision) VALUES (1, 1)`
	if dialect.DriverName() == "postgres" {
		query += ` ON CONFLICT (id) DO NOTHING`
	}
	if _, err := database.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("initialize tenant config revision: %w", err)
	}
	return nil
}

// CurrentConfigRevision returns the monotonic tenant config revision.
func CurrentConfigRevision(ctx context.Context, database *sql.DB, dialect db.QueryDialect) (uint64, error) {
	if ctx == nil || database == nil || dialect == nil {
		return 0, fmt.Errorf("read config revision requires context, database, and dialect")
	}
	var revision int64
	if err := database.QueryRowContext(ctx, db.Rebind(dialect, `SELECT revision FROM _kora_config_revision WHERE id = 1`)).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read tenant config revision: %w", err)
	}
	if revision < 1 {
		return 0, fmt.Errorf("tenant config revision is invalid: %d", revision)
	}
	return uint64(revision), nil
}

var ErrConfigRevisionConflict = errors.New("tenant config revision does not match the expected revision")

// BumpConfigRevisionTx advances the tenant configuration revision in the same
// transaction as the config mutation. It fails closed on a missing row or
// exhausted signed database integer.
func BumpConfigRevisionTx(tx *sql.Tx, dialect db.QueryDialect) error {
	if tx == nil || dialect == nil {
		return fmt.Errorf("config revision update requires transaction and dialect")
	}
	result, err := tx.Exec(db.Rebind(dialect, `UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807`))
	if err != nil {
		return fmt.Errorf("advance tenant config revision: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check tenant config revision update: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("tenant config revision row is missing or exhausted")
	}
	return nil
}

// BumpConfigRevisionAtTx advances the tenant revision only if it still matches
// expected. The conditional update is the commit-time compare-and-set for
// package/config activation across every supported SQL dialect.
func BumpConfigRevisionAtTx(tx *sql.Tx, dialect db.QueryDialect, expected uint64) error {
	if tx == nil || dialect == nil {
		return fmt.Errorf("conditional config revision update requires transaction and dialect")
	}
	if expected == 0 || expected > math.MaxInt64 {
		return fmt.Errorf("expected tenant config revision is outside the supported range")
	}
	result, err := tx.Exec(db.Rebind(dialect, `UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision = ? AND revision < 9223372036854775807`), int64(expected))
	if err != nil {
		return fmt.Errorf("advance tenant config revision at expected value: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check conditional tenant config revision update: %w", err)
	}
	if count != 1 {
		return ErrConfigRevisionConflict
	}
	return nil
}

// FenceConfigRevisionTx holds the tenant config revision row until the caller's
// transaction ends and verifies that it still matches expected. The no-op
// UPDATE acquires the same row lock used by config writers on MySQL/PostgreSQL
// and a write reservation on LibSQL; reading the value afterward either sees
// the committed newer revision or keeps later writers blocked through commit.
func FenceConfigRevisionTx(ctx context.Context, tx *sql.Tx, dialect db.QueryDialect, expected uint64) error {
	if ctx == nil || tx == nil || dialect == nil || expected == 0 || expected > math.MaxInt64 {
		return fmt.Errorf("config revision fence requires context, transaction, dialect, and positive revision")
	}
	if _, err := tx.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_config_revision SET revision = revision WHERE id = 1`)); err != nil {
		return fmt.Errorf("lock tenant config revision: %w", err)
	}
	var current int64
	if err := tx.QueryRowContext(ctx, db.Rebind(dialect, `SELECT revision FROM _kora_config_revision WHERE id = 1`)).Scan(&current); err != nil {
		return fmt.Errorf("read locked tenant config revision: %w", err)
	}
	if current < 1 || uint64(current) != expected {
		return ErrConfigRevisionConflict
	}
	return nil
}
