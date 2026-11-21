package projection

import (
	"context"
	"database/sql"
	"fmt"

	kdb "github.com/asenawritescode/kora/internal/db"
)

// lockFenceTx takes the tenant's projection fence inside tx and returns the new
// value. Each snapshot write bumps it, so a snapshot's generation orders writes
// for the tenant.
func lockFenceTx(ctx context.Context, tx *sql.Tx, dialect kdb.QueryDialect, site string) (int64, error) {
	insert := `INSERT INTO _kora_analytics_rebuild_lock (site, fence) VALUES (?, 0)`
	if driverName(dialect) == "mysql" {
		insert += ` ON DUPLICATE KEY UPDATE site = VALUES(site)`
	} else {
		insert += ` ON CONFLICT (site) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, kdb.Rebind(dialect, insert), site); err != nil {
		return 0, fmt.Errorf("ensure projection fence: %w", err)
	}
	result, err := tx.ExecContext(ctx, kdb.Rebind(dialect,
		`UPDATE _kora_analytics_rebuild_lock SET fence = fence + 1 WHERE site = ?`), site)
	if err != nil {
		return 0, fmt.Errorf("lock projection fence: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return 0, fmt.Errorf("verify projection fence: %w", err)
	} else if rows != 1 {
		return 0, fmt.Errorf("projection fence row missing for tenant %q", site)
	}
	var fence int64
	if err := tx.QueryRowContext(ctx, kdb.Rebind(dialect, `SELECT fence FROM _kora_analytics_rebuild_lock WHERE site = ?`), site).Scan(&fence); err != nil {
		return 0, fmt.Errorf("read projection fence: %w", err)
	}
	return fence, nil
}

func driverName(dialect kdb.QueryDialect) string {
	if named, ok := dialect.(interface{ DriverName() string }); ok {
		return named.DriverName()
	}
	return ""
}
