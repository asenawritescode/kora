package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

type DBSiteInfo struct {
	SiteID              string
	RuntimeCellID       string
	Name                string
	Domains             []string
	Status              string
	ConfigRevision      uint64
	DBType              string
	DBHost              string
	DBPort              int
	DBName              string
	DBUser              string
	DBPassword          string
	DBPasswordEncrypted bool
	FileStorage         string
	StorageBucket       string
}

func ensurePlatformSiteRegistration(platformDB *sql.DB, platformDBType string, cfg *SiteConfig) error {
	return ensurePlatformSiteRegistrationStatus(platformDB, platformDBType, cfg, "active")
}

func ensurePlatformSiteRegistrationStatus(platformDB *sql.DB, platformDBType string, cfg *SiteConfig, status string, requestedIDs ...string) error {
	return ensurePlatformSiteRegistrationStatusContext(context.Background(), platformDB, platformDBType, cfg, status, requestedIDs...)
}

func ensurePlatformSiteRegistrationStatusContext(ctx context.Context, platformDB *sql.DB, platformDBType string, cfg *SiteConfig, status string, requestedIDs ...string) error {
	return ensurePlatformSiteRegistrationAssignmentContext(ctx, platformDB, platformDBType, cfg, status, "", requestedIDs...)
}

func ensurePlatformSiteRegistrationAssignmentContext(ctx context.Context, platformDB *sql.DB, platformDBType string, cfg *SiteConfig, status, runtimeCellID string, requestedIDs ...string) error {
	if platformDB == nil || cfg == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("site registration requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "provisioning" && status != "active" {
		return fmt.Errorf("unsupported site registration status %q", status)
	}
	runtimeCellID, err := normalizeRuntimeCellID(runtimeCellID)
	if err != nil {
		return fmt.Errorf("invalid site deployment assignment: %w", err)
	}

	aliases := normalizedSiteAliases(cfg.Hostname, cfg.Domains())
	domainsJSON, err := json.Marshal(aliases)
	if err != nil {
		return fmt.Errorf("marshal site domains: %w", err)
	}

	dbPassword := cfg.DBPassword
	encrypted := false
	if dbPassword != "" {
		if cipherText, err := encryptPassword(dbPassword); err == nil {
			dbPassword = cipherText
			encrypted = true
		}
	}

	fileStorage := cfg.FileStorage
	if fileStorage == "" {
		fileStorage = "local"
	}
	storageBucket := cfg.StorageBucket
	if fileStorage == "s3" && storageBucket == "" {
		storageBucket = BucketNameForSite(cfg.Hostname)
	}

	requestedID := ""
	if len(requestedIDs) > 0 {
		requestedID = strings.TrimSpace(requestedIDs[0])
	}
	if requestedID != "" {
		if _, err := contract.ParseTenantID(requestedID); err != nil {
			return fmt.Errorf("invalid requested canonical site id: %w", err)
		}
	}
	tx, err := platformDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin platform site registration: %w", err)
	}
	defer tx.Rollback()
	siteID, err := canonicalSiteIDForRegistration(tx, platformDBType, cfg.Hostname, requestedID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	switch strings.ToLower(platformDBType) {
	case "postgres":
		if _, err := tx.Exec(
			`INSERT INTO _kora_site_registry
				(site, site_id, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, file_storage, storage_bucket, domains_json, status, created_at, updated_at, runtime_cell_id)
			 VALUES
				($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13, $14, $15, $16)
			 ON CONFLICT (site) DO UPDATE SET
				runtime_cell_id = CASE WHEN _kora_site_registry.status = 'provisioning' AND COALESCE(_kora_site_registry.runtime_cell_id, '') = '' AND EXCLUDED.runtime_cell_id <> '' THEN EXCLUDED.runtime_cell_id ELSE _kora_site_registry.runtime_cell_id END,
				db_type = EXCLUDED.db_type,
				db_host = EXCLUDED.db_host,
				db_port = EXCLUDED.db_port,
				db_name = EXCLUDED.db_name,
				db_user = EXCLUDED.db_user,
				db_password = EXCLUDED.db_password,
				db_password_encrypted = EXCLUDED.db_password_encrypted,
				file_storage = EXCLUDED.file_storage,
				storage_bucket = EXCLUDED.storage_bucket,
				domains_json = EXCLUDED.domains_json,
				status = EXCLUDED.status,
				config_revision = _kora_site_registry.config_revision + 1,
				updated_at = EXCLUDED.updated_at`,
			cfg.Hostname, siteID, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), fileStorage, storageBucket, string(domainsJSON), status, now, now, runtimeCellID,
		); err != nil {
			return fmt.Errorf("upsert platform site registry: %w", err)
		}
	default:
		_, upsertErr := tx.Exec(
			`INSERT INTO _kora_site_registry
				(site, site_id, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, file_storage, storage_bucket, domains_json, status, created_at, updated_at, runtime_cell_id)
			 VALUES
				(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
				runtime_cell_id = CASE WHEN status = 'provisioning' AND COALESCE(runtime_cell_id, '') = '' AND VALUES(runtime_cell_id) <> '' THEN VALUES(runtime_cell_id) ELSE runtime_cell_id END,
				db_type = VALUES(db_type),
				db_host = VALUES(db_host),
				db_port = VALUES(db_port),
				db_name = VALUES(db_name),
				db_user = VALUES(db_user),
				db_password = VALUES(db_password),
				db_password_encrypted = VALUES(db_password_encrypted),
				file_storage = VALUES(file_storage),
				storage_bucket = VALUES(storage_bucket),
				domains_json = VALUES(domains_json),
				status = VALUES(status),
				config_revision = config_revision + 1,
				updated_at = VALUES(updated_at)`,
			cfg.Hostname, siteID, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), fileStorage, storageBucket, string(domainsJSON), status, now, now, runtimeCellID,
		)
		if upsertErr != nil {
			if !isDuplicateUpsertUnsupported(upsertErr) {
				return fmt.Errorf("upsert platform site registry: %w", upsertErr)
			}
			res, err := tx.Exec(
				`UPDATE _kora_site_registry
				 SET runtime_cell_id = CASE WHEN status = 'provisioning' AND COALESCE(runtime_cell_id, '') = '' AND ? <> '' THEN ? ELSE runtime_cell_id END,
				 db_type = ?, db_host = ?, db_port = ?, db_name = ?, db_user = ?, db_password = ?, db_password_encrypted = ?, file_storage = ?, storage_bucket = ?, domains_json = ?, status = ?, config_revision = config_revision + 1, updated_at = ?
				 WHERE site = ?`,
				runtimeCellID, runtimeCellID, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), fileStorage, storageBucket, string(domainsJSON), status, now, cfg.Hostname,
			)
			if err != nil {
				return fmt.Errorf("update platform site registry: %w", err)
			}
			rows, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("read site registry rows affected: %w", err)
			}
			if rows == 0 {
				if _, err := tx.Exec(
					`INSERT INTO _kora_site_registry
						(site, site_id, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, file_storage, storage_bucket, domains_json, status, created_at, updated_at, runtime_cell_id)
					 VALUES
						(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					cfg.Hostname, siteID, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), fileStorage, storageBucket, string(domainsJSON), status, now, now, runtimeCellID,
				); err != nil {
					return fmt.Errorf("insert platform site registry: %w", err)
				}
			}
		}
	}
	dialect := db.Resolve(platformDBType)
	if err := replaceSiteAliasesTx(tx, dialect, siteID, aliases...); err != nil {
		return err
	}
	if err := setSiteAliasIndexStateTx(tx, dialect, true); err != nil {
		return fmt.Errorf("mark alias projection synchronized: %w", err)
	}
	if err := appendCurrentSiteDirectoryChange(tx, platformDBType, cfg.Hostname); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit platform site registration: %w", err)
	}
	return nil
}

func canonicalSiteIDForRegistration(tx *sql.Tx, platformDBType, hostname, requested string) (string, error) {
	query := `SELECT site_id, status FROM _kora_site_registry WHERE site = ?`
	dialect := strings.ToLower(strings.TrimSpace(platformDBType))
	if dialect == "postgres" {
		query = `SELECT site_id, status FROM _kora_site_registry WHERE site = $1`
	}
	if dialect == "mysql" || dialect == "postgres" {
		query += ` FOR UPDATE`
	}
	var existing, status string
	err := tx.QueryRow(query, hostname).Scan(&existing, &status)
	if err == nil {
		if status == "deleting" || status == "deleted" {
			return "", fmt.Errorf("site %q is %s and cannot be re-registered", hostname, status)
		}
		if requested != "" && requested != existing {
			return "", fmt.Errorf("hostname %q is already assigned to a different canonical tenant ID", hostname)
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", fmt.Errorf("read canonical tenant identity before registration: %w", err)
	}
	if requested != "" {
		return requested, nil
	}
	siteID, err := newCanonicalSiteID()
	if err != nil {
		return "", fmt.Errorf("generate canonical site id: %w", err)
	}
	return siteID, nil
}

func appendCurrentSiteDirectoryChange(tx *sql.Tx, platformDBType, hostname string) error {
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site = ?`
	if strings.EqualFold(platformDBType, "postgres") {
		query = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site = $1`
	}
	var info DBSiteInfo
	var domainsJSON []byte
	if err := tx.QueryRow(query, hostname).Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
		return fmt.Errorf("read registered site for directory event: %w", err)
	}
	if err := json.Unmarshal(domainsJSON, &info.Domains); err != nil {
		return fmt.Errorf("decode registered site aliases for directory event: %w", err)
	}
	return appendDirectoryChange(tx, platformDBType, info)
}

// UpdatePlatformSiteRegistration updates the persisted registry metadata for an existing site.
func UpdatePlatformSiteRegistration(platformDB *sql.DB, platformDBType string, hostname string, domains []string, fileStorage, storageBucket string) error {
	if platformDB == nil {
		return errors.New("platform site registry unavailable")
	}
	if hostname == "" {
		return nil
	}

	aliases := normalizedSiteAliases(hostname, domains)
	domainsJSON, err := json.Marshal(aliases)
	if err != nil {
		return fmt.Errorf("marshal site domains: %w", err)
	}
	if fileStorage == "" {
		fileStorage = "local"
	}
	if fileStorage == "s3" && storageBucket == "" {
		storageBucket = BucketNameForSite(hostname)
	}

	now := time.Now().UTC()
	dialect := db.Resolve(platformDBType)
	tx, err := platformDB.Begin()
	if err != nil {
		return fmt.Errorf("begin platform site update: %w", err)
	}
	defer tx.Rollback()
	switch strings.ToLower(platformDBType) {
	case "postgres":
		res, err := tx.Exec(
			`UPDATE _kora_site_registry
				SET file_storage = $1,
					storage_bucket = $2,
					domains_json = $3::jsonb,
					status = 'active',
					config_revision = config_revision + 1,
					updated_at = $4
				WHERE site = $5`,
			fileStorage, storageBucket, string(domainsJSON), now, hostname,
		)
		if err != nil {
			return fmt.Errorf("update platform site registry: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read site registry rows affected: %w", err)
		}
		if rows == 0 {
			return sql.ErrNoRows
		}
	default:
		res, err := tx.Exec(
			`UPDATE _kora_site_registry
			 SET file_storage = ?, storage_bucket = ?, domains_json = ?, status = 'active', config_revision = config_revision + 1, updated_at = ?
			 WHERE site = ?`,
			fileStorage, storageBucket, string(domainsJSON), now, hostname,
		)
		if err != nil {
			return fmt.Errorf("update platform site registry: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read site registry rows affected: %w", err)
		}
		if rows == 0 {
			return sql.ErrNoRows
		}
	}
	lookup := `SELECT site_id FROM _kora_site_registry WHERE site = ?`
	if strings.EqualFold(platformDBType, "postgres") {
		lookup = `SELECT site_id FROM _kora_site_registry WHERE site = $1`
	}
	var siteID string
	if err := tx.QueryRow(lookup, hostname).Scan(&siteID); err != nil {
		return fmt.Errorf("read canonical id for alias update: %w", err)
	}
	if err := replaceSiteAliasesTx(tx, dialect, siteID, aliases...); err != nil {
		return err
	}
	if err := setSiteAliasIndexStateTx(tx, dialect, true); err != nil {
		return fmt.Errorf("mark alias projection synchronized: %w", err)
	}
	if err := appendCurrentSiteDirectoryChange(tx, platformDBType, hostname); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit platform site update: %w", err)
	}
	return nil
}

func normalizedSiteAliases(hostname string, aliases []string) []string {
	result := make([]string, 0, len(aliases)+1)
	seen := make(map[string]struct{}, len(aliases)+1)
	for _, alias := range append(append([]string(nil), aliases...), hostname) {
		alias = NormalizeSiteAlias(alias)
		if alias == "" {
			continue
		}
		if _, exists := seen[alias]; exists {
			continue
		}
		seen[alias] = struct{}{}
		result = append(result, alias)
	}
	return result
}

func removePlatformSiteRegistration(platformDB *sql.DB, platformDBType, hostname string) error {
	if platformDB == nil || hostname == "" {
		return nil
	}
	siteID, lookupErr := registeredSiteID(platformDB, platformDBType, hostname)
	if lookupErr != nil && lookupErr != sql.ErrNoRows {
		return fmt.Errorf("read canonical id before removing site: %w", lookupErr)
	}
	return removePlatformSiteRegistrationByID(platformDB, platformDBType, siteID, hostname)
}

func removePlatformSiteRegistrationByID(platformDB *sql.DB, platformDBType, siteID, hostname string) error {
	if platformDB == nil || (siteID == "" && hostname == "") {
		return nil
	}
	dialect := db.Resolve(platformDBType)
	tx, err := platformDB.Begin()
	if err != nil {
		return fmt.Errorf("begin platform site removal: %w", err)
	}
	defer tx.Rollback()
	if siteID == "" && hostname != "" {
		lookup := `SELECT site_id FROM _kora_site_registry WHERE site = ?`
		if strings.EqualFold(platformDBType, "postgres") {
			lookup = `SELECT site_id FROM _kora_site_registry WHERE site = $1`
		}
		if err := tx.QueryRow(lookup, hostname).Scan(&siteID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read canonical id before site removal: %w", err)
		}
	}
	if siteID != "" {
		var info DBSiteInfo
		var domainsJSON []byte
		query := `SELECT site_id, site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
		if strings.EqualFold(platformDBType, "postgres") {
			query = `SELECT site_id, site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = $1`
		}
		if err := tx.QueryRow(query, siteID).Scan(&info.SiteID, &info.Name, &domainsJSON, &info.Status, &info.ConfigRevision); err != nil {
			return fmt.Errorf("read site before removal: %w", err)
		}
		if err := json.Unmarshal(domainsJSON, &info.Domains); err != nil {
			return fmt.Errorf("decode site aliases before removal: %w", err)
		}
		info.Status = "deleted"
		info.ConfigRevision++
		if err := appendDirectoryChange(tx, platformDBType, info); err != nil {
			return err
		}
		if err := replaceSiteAliasesTx(tx, dialect, siteID); err != nil {
			return err
		}
	}
	if siteID != "" {
		query := `DELETE FROM _kora_site_registry WHERE site_id = ?`
		if strings.EqualFold(platformDBType, "postgres") {
			query = `DELETE FROM _kora_site_registry WHERE site_id = $1`
		}
		if _, err := tx.Exec(query, siteID); err != nil {
			return err
		}
	} else if strings.EqualFold(platformDBType, "postgres") {
		if _, err := tx.Exec(`DELETE FROM _kora_site_registry WHERE site = $1`, hostname); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`DELETE FROM _kora_site_registry WHERE site = ?`, hostname); err != nil {
		return err
	}
	if err := setSiteAliasIndexStateTx(tx, dialect, true); err != nil {
		return fmt.Errorf("mark alias projection synchronized: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit platform site removal: %w", err)
	}
	return nil
}

func discoverSitesFromRegistryContext(ctx context.Context, db *sql.DB) ([]DBSiteInfo, error) {
	return discoverRegistrySitesContext(ctx, db, true)
}

func discoverAllSitesFromRegistry(db *sql.DB) ([]DBSiteInfo, error) {
	return discoverRegistrySites(db, false)
}

func discoverRegistrySites(db *sql.DB, activeOnly bool) ([]DBSiteInfo, error) {
	return discoverRegistrySitesContext(context.Background(), db, activeOnly)
}

func discoverRegistrySitesForCell(db *sql.DB, activeOnly bool, cellID, dialect string) ([]DBSiteInfo, error) {
	return discoverRegistrySitesForCellContext(context.Background(), db, activeOnly, cellID, dialect)
}

func discoverRegistrySitesContext(ctx context.Context, db *sql.DB, activeOnly bool) ([]DBSiteInfo, error) {
	return discoverRegistrySitesForCellContext(ctx, db, activeOnly, "", "")
}

func discoverRegistrySitesForCellContext(ctx context.Context, db *sql.DB, activeOnly bool, cellID, dialect string) ([]DBSiteInfo, error) {
	if ctx == nil || db == nil {
		return nil, fmt.Errorf("discover site registry requires context and database")
	}
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry ORDER BY site`
	if activeOnly {
		query = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE status = 'active' ORDER BY site`
	}
	if strings.TrimSpace(cellID) != "" {
		cellID = strings.TrimSpace(cellID)
		predicate := `(runtime_cell_id IS NULL OR runtime_cell_id = '' OR runtime_cell_id = 'default')`
		if cellID != "default" {
			placeholder := `?`
			if strings.EqualFold(dialect, "postgres") {
				placeholder = `$1`
			}
			predicate = `runtime_cell_id = ` + placeholder
		}
		if activeOnly {
			query = strings.Replace(query, `WHERE status = 'active'`, `WHERE status = 'active' AND `+predicate, 1)
		} else {
			query = strings.Replace(query, `ORDER BY site`, `WHERE `+predicate+` ORDER BY site`, 1)
		}
	}
	var rows *sql.Rows
	var err error
	if strings.TrimSpace(cellID) != "" && cellID != "default" {
		rows, err = db.QueryContext(ctx, query, cellID)
	} else {
		rows, err = db.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites []DBSiteInfo
	for rows.Next() {
		var (
			info             DBSiteInfo
			domainsJSON      string
			encryptedNumeric int
		)
		if err := rows.Scan(&info.SiteID, &info.RuntimeCellID, &info.Name, &info.DBType, &info.DBHost, &info.DBPort, &info.DBName, &info.DBUser, &info.DBPassword, &encryptedNumeric, &domainsJSON, &info.FileStorage, &info.StorageBucket, &info.Status, &info.ConfigRevision); err != nil {
			return nil, err
		}
		if info.FileStorage == "s3" && info.StorageBucket == "" {
			info.StorageBucket = BucketNameForSite(info.Name)
		}
		info.DBPasswordEncrypted = encryptedNumeric == 1
		if info.DBPasswordEncrypted && info.DBPassword != "" {
			plain, err := decryptPassword(info.DBPassword)
			if err != nil {
				return nil, fmt.Errorf("decrypting db password for site %s: %w", info.Name, err)
			}
			info.DBPassword = plain
		}
		if domainsJSON != "" && domainsJSON != "null" {
			if err := json.Unmarshal([]byte(domainsJSON), &info.Domains); err != nil {
				return nil, fmt.Errorf("decode site domains for %s: %w", info.Name, err)
			}
		}
		if len(info.Domains) == 0 {
			info.Domains = []string{info.Name}
		}
		sites = append(sites, info)
	}
	return sites, rows.Err()
}

func isDuplicateUpsertUnsupported(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "syntax") || strings.Contains(s, "duplicate key") || strings.Contains(s, "near \"duplicate\"")
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
