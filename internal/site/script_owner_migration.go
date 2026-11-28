package site

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/asenawritescode/kora/internal/db"
)

// migrateScriptOwners adds _kora_script.owner_pack to databases created before
// pack script ownership. It does not assign owners to existing rows: the Engine
// keeps no installed pack file content that could prove which pack produced a
// row, so a version snapshot or a single-pack guess is not evidence. Unowned rows
// are logged, and an owned import adopts one only when its content matches.
func migrateScriptOwners(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if _, err := database.ExecContext(ctx, scriptOwnerMigration(dialect)); err != nil && !isIdempotentSQLError(err) {
		return fmt.Errorf("migrating script owner: %w", err)
	}
	sites, err := unownedScriptSites(ctx, database)
	if err != nil {
		return err
	}
	for _, site := range sites {
		if err := warnUnownedScripts(ctx, database, dialect, site); err != nil {
			return err
		}
	}
	return nil
}

func unownedScriptSites(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT DISTINCT site FROM _kora_script WHERE owner_pack = ''`)
	if err != nil {
		return nil, fmt.Errorf("listing unowned scripts: %w", err)
	}
	defer rows.Close()
	var sites []string
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

func warnUnownedScripts(ctx context.Context, database *sql.DB, dialect db.Dialect, site string) error {
	rows, err := database.QueryContext(ctx, db.Rebind(dialect, `SELECT name FROM _kora_script WHERE site = ? AND owner_pack = ''`), site)
	if err != nil {
		return fmt.Errorf("listing unowned scripts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		slog.Warn("script row has no proven installed owner; left unowned (no installed pack content records it); an owned import adopts it only if its content matches", "site", site, "script", name)
	}
	return rows.Err()
}
