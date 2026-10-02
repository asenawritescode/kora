package site

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/db"
)

// bootstrapSiteAliasIndex maintains a normalized, indexed alias projection.
// The composite key intentionally allows an alias to point at multiple sites
// during report-only rollout; resolution detects and reports that conflict
// instead of choosing an arbitrary site.
func bootstrapSiteAliasIndex(database *sql.DB, dialect db.Dialect) error {
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_site_alias (
		alias VARCHAR(255) NOT NULL,
		site_id VARCHAR(64) NOT NULL,
		PRIMARY KEY (alias, site_id)
	)`); err != nil {
		return fmt.Errorf("create site alias index: %w", err)
	}
	indexDDL := `CREATE INDEX IF NOT EXISTS idx_kora_site_alias_site ON _kora_site_alias (site_id)`
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		indexDDL = `CREATE INDEX idx_kora_site_alias_site ON _kora_site_alias (site_id)`
	}
	if _, err := database.Exec(indexDDL); err != nil && !isAlreadyAppliedMigration(err) {
		return fmt.Errorf("index site aliases by site: %w", err)
	}
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS _kora_site_alias_meta (
		id INTEGER PRIMARY KEY,
		initialized INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("create site alias index metadata: %w", err)
	}
	var initialized int
	err := database.QueryRow(`SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`).Scan(&initialized)
	if err == nil && initialized == 1 {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read site alias index metadata: %w", err)
	}
	if err := backfillSiteAliasIndex(database, dialect); err != nil {
		return err
	}
	if err := setSiteAliasIndexState(database, dialect, true); err != nil {
		return fmt.Errorf("mark site alias index initialized: %w", err)
	}
	return nil
}

func backfillSiteAliasIndex(database *sql.DB, dialect db.Dialect) error {
	rows, err := database.Query(`SELECT site_id, site, COALESCE(domains_json, '[]') FROM _kora_site_registry ORDER BY site`)
	if err != nil {
		return fmt.Errorf("read aliases for site index backfill: %w", err)
	}
	type item struct {
		siteID string
		name   string
		json   string
	}
	var sites []item
	for rows.Next() {
		var value item
		if err := rows.Scan(&value.siteID, &value.name, &value.json); err != nil {
			rows.Close()
			return fmt.Errorf("scan site alias backfill row: %w", err)
		}
		sites = append(sites, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate site alias backfill rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close site alias backfill rows: %w", err)
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("begin site alias index backfill: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM _kora_site_alias`); err != nil {
		return fmt.Errorf("clear site alias projection before resync: %w", err)
	}
	for _, value := range sites {
		var aliases []string
		if err := json.Unmarshal([]byte(value.json), &aliases); err != nil {
			return fmt.Errorf("decode site aliases for %s: %w", value.name, err)
		}
		aliases = append(aliases, value.name)
		if err := insertSiteAliases(tx, dialect, value.siteID, aliases...); err != nil {
			return fmt.Errorf("index aliases for %s: %w", value.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site alias index backfill: %w", err)
	}
	return nil
}

func syncSiteAliases(database *sql.DB, dialect db.Dialect, siteID string, aliases ...string) error {
	if database == nil || siteID == "" {
		return nil
	}
	tx, err := database.Begin()
	if err != nil {
		return fmt.Errorf("begin site alias update: %w", err)
	}
	defer tx.Rollback()
	deleteQuery := `DELETE FROM _kora_site_alias WHERE site_id = ?`
	if strings.EqualFold(dialect.DriverName(), "postgres") {
		deleteQuery = `DELETE FROM _kora_site_alias WHERE site_id = $1`
	}
	if _, err := tx.Exec(deleteQuery, siteID); err != nil {
		return fmt.Errorf("remove previous site aliases: %w", err)
	}
	if err := insertSiteAliases(tx, dialect, siteID, aliases...); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit site alias update: %w", err)
	}
	return nil
}

func invalidateSiteAliasIndex(database *sql.DB, dialect db.Dialect) error {
	return setSiteAliasIndexState(database, dialect, false)
}

func setSiteAliasIndexState(database *sql.DB, dialect db.Dialect, initialized bool) error {
	value := 0
	if initialized {
		value = 1
	}
	query := fmt.Sprintf(`INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, %s) ON CONFLICT (id) DO UPDATE SET initialized = excluded.initialized`, dialect.Placeholder(1))
	if _, isMySQL := dialect.(*db.MySQLDialect); isMySQL {
		query = `INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, ?) ON DUPLICATE KEY UPDATE initialized = VALUES(initialized)`
	}
	_, err := database.Exec(query, value)
	return err
}

type aliasExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertSiteAliases(execer aliasExecer, dialect db.Dialect, siteID string, aliases ...string) error {
	query := fmt.Sprintf(`INSERT INTO _kora_site_alias (alias, site_id) VALUES (%s, %s)`, dialect.Placeholder(1), dialect.Placeholder(2))
	switch strings.ToLower(dialect.DriverName()) {
	case "mysql":
		query = strings.Replace(query, "INSERT INTO", "INSERT IGNORE INTO", 1)
	case "libsql", "postgres":
		query += ` ON CONFLICT (alias, site_id) DO NOTHING`
	}
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		alias = NormalizeSiteAlias(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		if _, err := execer.Exec(query, alias, siteID); err != nil {
			return fmt.Errorf("insert alias %q: %w", alias, err)
		}
	}
	return nil
}

// ResolveSiteAlias returns exactly one site for a normalized alias. Ambiguous
// aliases are surfaced as errors so request routing never depends on row order.
func ResolveSiteAlias(database *sql.DB, platformDBType, alias string) (string, error) {
	if database == nil {
		return "", errors.New("site registry unavailable")
	}
	query := `SELECT site_id FROM _kora_site_alias WHERE alias = ? ORDER BY site_id LIMIT 2`
	if strings.EqualFold(platformDBType, "postgres") {
		query = `SELECT site_id FROM _kora_site_alias WHERE alias = $1 ORDER BY site_id LIMIT 2`
	}
	rows, err := database.Query(query, NormalizeSiteAlias(alias))
	if err != nil {
		return "", fmt.Errorf("resolve site alias: %w", err)
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
