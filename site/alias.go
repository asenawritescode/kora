package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/asenawritescode/kora/db"
	"strings"
)

var ErrSiteAliasIndexUnavailable = errors.New("site alias index is unavailable")

func bootstrapSiteAliasIndexContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	if ctx == nil {
		return errors.New("alias bootstrap context required")
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_site_alias (alias VARCHAR(255) NOT NULL, site_id VARCHAR(64) NOT NULL, PRIMARY KEY (alias, site_id))`); err != nil {
		return fmt.Errorf("alias table: %w", err)
	}
	indexDDL := `CREATE INDEX IF NOT EXISTS idx_kora_site_alias_site ON _kora_site_alias (site_id)`
	if _, ok := dialect.(*db.MySQLDialect); ok {
		indexDDL = `CREATE INDEX idx_kora_site_alias_site ON _kora_site_alias (site_id)`
	}
	if _, err := database.ExecContext(ctx, indexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("alias index: %w", err)
	}
	if _, err := database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _kora_site_alias_meta (id INTEGER PRIMARY KEY, initialized INTEGER NOT NULL DEFAULT 0)`); err != nil {
		return fmt.Errorf("alias table metadata: %w", err)
	}
	var initialized int
	err := database.QueryRowContext(ctx, `SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`).Scan(&initialized)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("alias metadata: %w", err)
	}
	if initialized != 1 {
		if err := backfillSiteAliasIndexContext(ctx, database, dialect); err != nil {
			return err
		}
	}
	if err := ensureUniqueSiteAliasIndex(ctx, database, dialect); err != nil {
		return err
	}
	if err := setSiteAliasIndexStateContext(ctx, database, dialect, true); err != nil {
		return fmt.Errorf("mark alias index: %w", err)
	}
	return nil
}
func ensureUniqueSiteAliasIndex(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	var alias string
	var owners int
	err := database.QueryRowContext(ctx, `SELECT alias, COUNT(DISTINCT site_id) FROM _kora_site_alias GROUP BY alias HAVING COUNT(DISTINCT site_id) > 1 ORDER BY alias LIMIT 1`).Scan(&alias, &owners)
	if err == nil {
		return fmt.Errorf("site alias %q has %d tenant owners; resolve legacy alias ownership conflict before startup", alias, owners)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check ownership: %w", err)
	}
	query := `CREATE UNIQUE INDEX IF NOT EXISTS idx_kora_site_alias_unique ON _kora_site_alias (alias)`
	if _, ok := dialect.(*db.MySQLDialect); ok {
		query = `CREATE UNIQUE INDEX idx_kora_site_alias_unique ON _kora_site_alias (alias)`
	}
	if _, err := database.ExecContext(ctx, query); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("create unique alias index: %w", err)
	}
	err = verifySiteAliasUniqueIndex(ctx, database, dialect)
	if err != nil {
		return err
	}
	return nil
}
func verifySiteAliasUniqueIndex(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	id, err := newCanonicalSiteID()
	if err != nil {
		return err
	}
	alias := "kora-probe-" + strings.ToLower(id)
	if err := insertSiteAliases(tx, dialect, "probe-a", alias); err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	err = insertSiteAliases(tx, dialect, "probe-b", alias)
	if err == nil {
		return errors.New("not unique")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unique") && !strings.Contains(msg, "duplicate") {
		return fmt.Errorf("probe: %w", err)
	}
	return nil
}
func backfillSiteAliasIndexContext(ctx context.Context, database *sql.DB, dialect db.Dialect) error {
	rows, err := database.QueryContext(ctx, `SELECT site_id, site, COALESCE(domains_json, '[]') FROM _kora_site_registry ORDER BY site`)
	if err != nil {
		return err
	}
	type item struct{ id, name, domains string }
	var sites []item
	for rows.Next() {
		var v item
		if err := rows.Scan(&v.id, &v.name, &v.domains); err != nil {
			rows.Close()
			return err
		}
		sites = append(sites, v)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM _kora_site_alias`); err != nil {
		return err
	}
	for _, v := range sites {
		var aliases []string
		if err := json.Unmarshal([]byte(v.domains), &aliases); err != nil {
			return fmt.Errorf("decode %s aliases: %w", v.name, err)
		}
		aliases = append(aliases, v.name)
		if err := insertSiteAliases(tx, dialect, v.id, aliases...); err != nil {
			return fmt.Errorf("index %s aliases: %w", v.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
func replaceSiteAliasesTx(tx *sql.Tx, dialect db.Dialect, siteID string, aliases ...string) error {
	q := `DELETE FROM _kora_site_alias WHERE site_id = ?`
	if strings.EqualFold(dialect.DriverName(), "postgres") {
		q = `DELETE FROM _kora_site_alias WHERE site_id = $1`
	}
	if _, err := tx.Exec(q, siteID); err != nil {
		return fmt.Errorf("remove old aliases: %w", err)
	}
	return insertSiteAliases(tx, dialect, siteID, aliases...)
}
func aliasStateQuery(dialect db.Dialect) string {
	q := fmt.Sprintf(`INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, %s) ON CONFLICT (id) DO UPDATE SET initialized = excluded.initialized`, dialect.Placeholder(1))
	if _, ok := dialect.(*db.MySQLDialect); ok {
		q = `INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, ?) ON DUPLICATE KEY UPDATE initialized = VALUES(initialized)`
	}
	return q
}
func setSiteAliasIndexStateContext(ctx context.Context, database *sql.DB, dialect db.Dialect, initialized bool) error {
	if ctx == nil {
		return errors.New("nil alias-state context")
	}
	v := 0
	if initialized {
		v = 1
	}
	_, err := database.ExecContext(ctx, aliasStateQuery(dialect), v)
	return err
}
func setSiteAliasIndexStateTx(tx *sql.Tx, dialect db.Dialect, initialized bool) error {
	v := 0
	if initialized {
		v = 1
	}
	_, err := tx.Exec(aliasStateQuery(dialect), v)
	return err
}

type aliasExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertSiteAliases(execer aliasExecer, dialect db.Dialect, siteID string, aliases ...string) error {
	q := fmt.Sprintf(`INSERT INTO _kora_site_alias (alias, site_id) VALUES (%s, %s)`, dialect.Placeholder(1), dialect.Placeholder(2))
	if d := strings.ToLower(dialect.DriverName()); d == "libsql" || d == "postgres" {
		q += ` ON CONFLICT (alias, site_id) DO NOTHING`
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, a := range aliases {
		a = NormalizeSiteAlias(a)
		if a == "" {
			continue
		}
		if _, ok := seen[a]; ok {
			continue
		}
		seen[a] = struct{}{}
		if _, err := execer.Exec(q, a, siteID); err != nil {
			return fmt.Errorf("insert alias %q: %w", a, err)
		}
	}
	return nil
}
func ResolveSiteAlias(database *sql.DB, platformDBType, alias string) (string, error) {
	return ResolveSiteAliasContext(context.Background(), database, platformDBType, alias)
}
func ResolveSiteAliasContext(ctx context.Context, database *sql.DB, platformDBType, alias string) (string, error) {
	if database == nil {
		return "", errors.New("site registry unavailable")
	}
	if ctx == nil {
		return "", errors.New("site alias resolution requires context")
	}
	var initialized int
	if err := database.QueryRowContext(ctx, `SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`).Scan(&initialized); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSiteAliasIndexUnavailable
		}
		return "", fmt.Errorf("check alias index state: %w", err)
	}
	if initialized != 1 {
		return "", ErrSiteAliasIndexUnavailable
	}
	q := `SELECT site_id FROM _kora_site_alias WHERE alias = ? ORDER BY site_id LIMIT 2`
	if strings.EqualFold(platformDBType, "postgres") {
		q = `SELECT site_id FROM _kora_site_alias WHERE alias = $1 ORDER BY site_id LIMIT 2`
	}
	rows, err := database.QueryContext(ctx, q, NormalizeSiteAlias(alias))
	if err != nil {
		return "", fmt.Errorf("resolve alias route: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", sql.ErrNoRows
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("site alias %q resolves to multiple canonical sites", NormalizeSiteAlias(alias))
	}
}
