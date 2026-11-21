package facts

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/asenawritescode/kora/internal/db"
)

// Unsynced returns the models among names whose fact tables the sync has not created yet. The sync stores a
// model's fingerprint only after its tables exist, so a row with an empty fingerprint is a claim, not a sync.
// A database with no shape table at all has no synced model.
func Unsynced(ctx context.Context, conn *sql.DB, dialect db.Dialect, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if !hasTable(ctx, conn, dialect, shapeTable) {
		return append([]string(nil), names...), nil
	}
	marks, args := inList(names)
	query := fmt.Sprintf("SELECT model FROM %s WHERE model IN (%s) AND fingerprint <> ''", dialect.QuoteIdent(shapeTable), marks)
	rows, err := conn.QueryContext(ctx, db.Rebind(dialect, query), args...)
	if err != nil {
		return nil, fmt.Errorf("facts: read model sync state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	synced := map[string]bool{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("facts: scan model sync state: %w", err)
		}
		synced[model] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("facts: read model sync state: %w", err)
	}
	var out []string
	for _, name := range names {
		if !synced[name] {
			out = append(out, name)
		}
	}
	return out, nil
}

// hasTable reports whether table exists. The probe reads no rows.
func hasTable(ctx context.Context, conn *sql.DB, dialect db.Dialect, table string) bool {
	_, err := conn.ExecContext(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE 1 = 0", dialect.QuoteIdent(table))) //nolint:gosec // identifiers only, quoted by the dialect
	return err == nil
}
