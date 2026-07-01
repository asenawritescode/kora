package site

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type DBSiteInfo struct {
	Name                string
	Domains             []string
	DBType              string
	DBHost              string
	DBPort              int
	DBName              string
	DBUser              string
	DBPassword          string
	DBPasswordEncrypted bool
}

func ensurePlatformSiteRegistration(platformDB *sql.DB, platformDBType string, cfg *SiteConfig) error {
	if platformDB == nil || cfg == nil {
		return nil
	}

	domainsJSON, err := json.Marshal(cfg.Domains())
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

	now := time.Now().UTC()
	switch strings.ToLower(platformDBType) {
	case "postgres":
		if _, err := platformDB.Exec(
			`INSERT INTO _kora_site_registry
				(site, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, domains_json, status, created_at, updated_at)
			 VALUES
				($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, 'active', $10, $11)
			 ON CONFLICT (site) DO UPDATE SET
				db_type = EXCLUDED.db_type,
				db_host = EXCLUDED.db_host,
				db_port = EXCLUDED.db_port,
				db_name = EXCLUDED.db_name,
				db_user = EXCLUDED.db_user,
				db_password = EXCLUDED.db_password,
				db_password_encrypted = EXCLUDED.db_password_encrypted,
				domains_json = EXCLUDED.domains_json,
				status = 'active',
				updated_at = EXCLUDED.updated_at`,
			cfg.Hostname, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), string(domainsJSON), now, now,
		); err != nil {
			return fmt.Errorf("upsert platform site registry: %w", err)
		}
	default:
		if _, err := platformDB.Exec(
			`INSERT INTO _kora_site_registry
				(site, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, domains_json, status, created_at, updated_at)
			 VALUES
				(?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)
			 ON DUPLICATE KEY UPDATE
				db_type = VALUES(db_type),
				db_host = VALUES(db_host),
				db_port = VALUES(db_port),
				db_name = VALUES(db_name),
				db_user = VALUES(db_user),
				db_password = VALUES(db_password),
				db_password_encrypted = VALUES(db_password_encrypted),
				domains_json = VALUES(domains_json),
				status = 'active',
				updated_at = VALUES(updated_at)`,
			cfg.Hostname, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), string(domainsJSON), now, now,
		); err == nil {
			return nil
		} else if !isDuplicateUpsertUnsupported(err) {
			return fmt.Errorf("upsert platform site registry: %w", err)
		}

		res, err := platformDB.Exec(
			`UPDATE _kora_site_registry
			 SET db_type = ?, db_host = ?, db_port = ?, db_name = ?, db_user = ?, db_password = ?, db_password_encrypted = ?, domains_json = ?, status = 'active', updated_at = ?
			 WHERE site = ?`,
			cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), string(domainsJSON), now, cfg.Hostname,
		)
		if err != nil {
			return fmt.Errorf("update platform site registry: %w", err)
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("read site registry rows affected: %w", err)
		}
		if rows > 0 {
			return nil
		}
		if _, err := platformDB.Exec(
			`INSERT INTO _kora_site_registry
				(site, db_type, db_host, db_port, db_name, db_user, db_password, db_password_encrypted, domains_json, status, created_at, updated_at)
			 VALUES
				(?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
			cfg.Hostname, cfg.DBType, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, boolToInt(encrypted), string(domainsJSON), now, now,
		); err != nil {
			return fmt.Errorf("insert platform site registry: %w", err)
		}
	}
	return nil
}

func removePlatformSiteRegistration(platformDB *sql.DB, platformDBType, hostname string) error {
	if platformDB == nil || hostname == "" {
		return nil
	}
	switch strings.ToLower(platformDBType) {
	case "postgres":
		_, err := platformDB.Exec(`DELETE FROM _kora_site_registry WHERE site = $1`, hostname)
		return err
	default:
		_, err := platformDB.Exec(`DELETE FROM _kora_site_registry WHERE site = ?`, hostname)
		return err
	}
}

func discoverSitesFromRegistry(db *sql.DB) ([]DBSiteInfo, error) {
	rows, err := db.Query(`SELECT site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]') FROM _kora_site_registry WHERE status = 'active' ORDER BY site`)
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
		if err := rows.Scan(&info.Name, &info.DBType, &info.DBHost, &info.DBPort, &info.DBName, &info.DBUser, &info.DBPassword, &encryptedNumeric, &domainsJSON); err != nil {
			return nil, err
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

func isSiteRegistryMissing(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "_kora_site_registry") && (strings.Contains(s, "doesn't exist") || strings.Contains(s, "does not exist") || strings.Contains(s, "no such table"))
}

func isLegacyConfigMissing(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "_kora_config_version") && (strings.Contains(s, "doesn't exist") || strings.Contains(s, "does not exist") || strings.Contains(s, "no such table"))
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
