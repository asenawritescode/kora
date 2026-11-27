package site

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// migrateScriptOwners adds _kora_script.owner_pack to databases created before
// pack script ownership, then backfills owners of existing unowned rows.
func migrateScriptOwners(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if _, err := database.ExecContext(ctx, scriptOwnerMigration(dialect)); err != nil && !isIdempotentSQLError(err) {
		return fmt.Errorf("migrating script owner: %w", err)
	}
	sites, err := unownedScriptSites(ctx, database)
	if err != nil {
		return err
	}
	for _, site := range sites {
		owners, err := installedScriptOwners(ctx, database, dialect, site)
		if err != nil {
			return err
		}
		for name, pack := range owners {
			if _, err := database.ExecContext(ctx, db.Rebind(dialect, `UPDATE _kora_script SET owner_pack = ? WHERE site = ? AND name = ? AND owner_pack = ''`), pack, site, name); err != nil {
				return fmt.Errorf("backfilling owner of script %s: %w", name, err)
			}
		}
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

// installedScriptOwners maps script row names to the pack that installed them,
// read from the site's config versions, newest first. A version attributes its
// scripts only when it names exactly one pack, so an ambiguous version proves nothing.
func installedScriptOwners(ctx context.Context, database *sql.DB, dialect db.Dialect, site string) (map[string]string, error) {
	rows, err := database.QueryContext(ctx, db.Rebind(dialect, `SELECT config FROM _kora_config_version WHERE site = ? ORDER BY version DESC`), site)
	if err != nil {
		return nil, fmt.Errorf("reading config versions for script owners: %w", err)
	}
	defer rows.Close()
	owners := map[string]string{}
	for rows.Next() {
		var config sql.NullString
		if err := rows.Scan(&config); err != nil {
			return nil, err
		}
		if !config.Valid || config.String == "" {
			continue
		}
		snapshot, err := doctype.FromSExpr(config.String)
		if err != nil {
			slog.Warn("config version unreadable for script owner backfill; skipped", "site", site, "error", err)
			continue
		}
		pack, ok := singleInstalledPack(snapshot)
		if !ok {
			continue
		}
		addSnapshotScriptOwners(owners, snapshot, pack)
	}
	return owners, rows.Err()
}

// addSnapshotScriptOwners records pack as the owner of each script row name in snapshot
// that no newer version has already claimed.
func addSnapshotScriptOwners(owners map[string]string, snapshot *doctype.ConfigSnapshot, pack string) {
	for _, ss := range snapshot.Scripts {
		if ss == nil {
			continue
		}
		for _, name := range []string{ss.Name, legacyScriptName(ss)} {
			if _, seen := owners[name]; name != "" && !seen {
				owners[name] = pack
			}
		}
	}
}

// singleInstalledPack returns the one pack a config snapshot installed, if it installed exactly one.
func singleInstalledPack(snapshot *doctype.ConfigSnapshot) (string, bool) {
	packs := map[string]bool{}
	for _, m := range snapshot.Modules {
		if pack, ok := strings.CutSuffix(string(m.ID), ".core"); ok && pack != "" {
			packs[pack] = true
		}
	}
	if len(packs) != 1 {
		return "", false
	}
	for pack := range packs {
		return pack, true
	}
	return "", false
}

// legacyScriptName is the unnamespaced row name a pre-ownership import gave a bound script.
func legacyScriptName(ss *doctype.ScriptSnapshot) string {
	if ss.DocType == "" {
		return ""
	}
	return strings.ReplaceAll(ss.DocType, " ", "_") + "_" + ss.Event
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
		slog.Warn("script row has no installed owner; left unowned and inactive for owned re-imports to refuse", "site", site, "script", name)
	}
	return rows.Err()
}
