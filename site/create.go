package site

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/auth"
	sqlDialect "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// CreateSiteInput holds all parameters needed to create a new site.
// DB fields are optional — when empty, platform defaults are applied.
type CreateSiteInput struct {
	// Site identity (required).
	Hostname string

	// DB connection (all optional — defaults from platform config when empty).
	DBType     string // "mysql" or "libsql"
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string

	// Admin account (required).
	AdminEmail    string
	AdminPassword string
	AdminFullName string // defaults to "Administrator"

	// Platform defaults for DB (filled by caller from env/StartupConfig).
	PlatformDBHost     string
	PlatformDBPort     int
	PlatformDBType     string // "mysql" or "libsql"
	PlatformDBUser     string
	PlatformDBPassword string

	// PlatformDB is an existing, authenticated connection to the platform database.
	// When set (for LibSQL), CreateSite reuses this connection instead of calling Connect.
	// This avoids auth issues with opening a second connection to the same server.
	PlatformDB *sql.DB

	// ConfigDir is where site_config.yaml is written. Defaults to KORA_CONFIG_DIR or ".".
	ConfigDir string
}

// applyDefaults fills empty fields with platform config or hardcoded defaults.
func (in *CreateSiteInput) applyDefaults() {
	if in.DBHost == "" {
		if in.PlatformDBHost != "" {
			in.DBHost = in.PlatformDBHost
		} else {
			in.DBHost = "127.0.0.1"
		}
	}
	if in.DBPort == 0 {
		if in.PlatformDBPort != 0 {
			in.DBPort = in.PlatformDBPort
		} else {
			in.DBPort = 3306
		}
	}
	if in.DBName == "" {
		// Derive from hostname: dots become underscores.
		in.DBName = strings.ReplaceAll(in.Hostname, ".", "_")
	}
	if in.DBType == "" {
		if in.PlatformDBType != "" {
			in.DBType = in.PlatformDBType
		} else {
			in.DBType = "mysql"
		}
	}
	if in.DBUser == "" {
		if in.PlatformDBUser != "" {
			in.DBUser = in.PlatformDBUser
		} else {
			in.DBUser = "root"
		}
	}
	if in.DBPassword == "" && in.PlatformDBPassword != "" {
		in.DBPassword = in.PlatformDBPassword
	}
	if in.AdminFullName == "" {
		in.AdminFullName = "Administrator"
	}
	if in.ConfigDir == "" {
		in.ConfigDir = os.Getenv("KORA_CONFIG_DIR")
		if in.ConfigDir == "" {
			in.ConfigDir = "."
		}
	}
}

// CreateSiteResult holds the result of a successful site creation.
type CreateSiteResult struct {
	Config   *SiteConfig
	DB       *sql.DB
	Registry *doctype.Registry
}

// CreateSite creates a complete Kora site: database, system tables, admin user, config version.
// This is the single canonical site creation codepath used by both CLI setup and the console API.
func CreateSite(input CreateSiteInput) (*CreateSiteResult, error) {
	input.applyDefaults()

	siteCfg := &SiteConfig{
		DBType:      input.DBType,
		DBHost:      input.DBHost,
		DBPort:      input.DBPort,
		DBName:      input.DBName,
		DBUser:      input.DBUser,
		DBPassword:  input.DBPassword,
		Hostname:    input.Hostname,
		FileStorage: "local",
		FilesPath:   fmt.Sprintf("sites/%s/files", input.Hostname),
		Apps:        []string{"core"},
		DomainsList: []string{input.Hostname},
	}

	// Compute and store DB fingerprint.
	siteCfg.DBFingerprint = computeFingerprint(siteCfg)

	// Step 1: Create database.
	if err := CreateDatabase(siteCfg); err != nil {
		return nil, fmt.Errorf("creating database: %w", err)
	}

	// Step 2: Write site config to disk (with fingerprint).
	if err := writeSiteConfig(input.ConfigDir, input.Hostname, siteCfg); err != nil {
		return nil, fmt.Errorf("writing site config: %w", err)
	}

	// Step 3: Connect to the new database.
	// For LibSQL, reuse the platform's already-authenticated connection
	// to avoid auth issues (HTTP Basic auth may not carry to a second sql.Open).
	var db *sql.DB
	isPlatformDB := false
	if input.PlatformDB != nil && input.DBType == "libsql" {
		db = input.PlatformDB
		isPlatformDB = true
	} else {
		var err error
		db, err = Connect(siteCfg)
		if err != nil {
			return nil, fmt.Errorf("connecting to database: %w", err)
		}
	}

	// Step 4: Bootstrap system tables.
	if err := BootstrapSystemTables(db, sqlDialect.Resolve(input.DBType)); err != nil {
		if !isPlatformDB {
			db.Close()
		}
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}

	// Step 5: Create admin user.
	if err := createAdminUser(db, input.AdminEmail, input.AdminPassword, input.AdminFullName); err != nil {
		if !isPlatformDB {
			db.Close()
		}
		return nil, fmt.Errorf("creating admin user: %w", err)
	}

	// Step 6: Create initial config version.
	ensureConfigVersion(db, input.Hostname)

	// Step 7: Build empty registry.
	registry := doctype.NewRegistry()
	registry.LoadFull(nil, nil, nil)

	return &CreateSiteResult{
		Config:   siteCfg,
		DB:       db,
		Registry: registry,
	}, nil
}

// computeFingerprint returns a SHA-256 hash of (host:port:dbname).
// Password is deliberately excluded — passwords legitimately change.
func computeFingerprint(cfg *SiteConfig) string {
	payload := fmt.Sprintf("%s:%d:%s", cfg.DBHost, cfg.DBPort, cfg.DBName)
	sum := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", sum)
}

// ValidateFingerprint checks that the current DB connection details match the
// fingerprint stored at site creation time. Returns an error if they don't match.
func ValidateFingerprint(cfg *SiteConfig) error {
	if cfg.DBFingerprint == "" {
		return nil // No fingerprint — site created before this feature, skip.
	}
	current := computeFingerprint(cfg)
	if current != cfg.DBFingerprint {
		return fmt.Errorf(
			"site %q DB connection changed: fingerprint mismatch.\n"+
				"  Current:  %s\n"+
				"  Expected: %s\n"+
				"If this change is intentional, update db_fingerprint in sites/%s/site_config.yaml to:\n"+
				"  db_fingerprint: %s\n"+
				"Or remove db_fingerprint to disable this check.",
			cfg.Hostname, current, cfg.DBFingerprint, cfg.Hostname, current,
		)
	}
	return nil
}

// writeSiteConfig writes a site_config.yaml with the DB fingerprint to disk.
// If KORA_SECRET_KEY is set, the DB password is encrypted at rest.
func writeSiteConfig(configDir, hostname string, cfg *SiteConfig) error {
	siteDir := fmt.Sprintf("%s/sites/%s", configDir, hostname)
	if err := os.MkdirAll(siteDir, 0755); err != nil {
		return err
	}
	filesDir := fmt.Sprintf("%s/files", siteDir)
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return err
	}

	// Encrypt password if we have a server key.
	dbPassword := cfg.DBPassword
	dbPasswordEncrypted := false
	if dbPassword != "" {
		if encrypted, err := encryptPassword(dbPassword); err == nil {
			dbPassword = encrypted
			dbPasswordEncrypted = true
		}
		// If encryption fails (no KORA_SECRET_KEY), we store plaintext.
	}

	content := fmt.Sprintf(`# Site configuration for %s
db_host: %s
db_port: %d
db_name: %s
db_user: %s
db_password: %s
db_password_encrypted: %v

redis_url: redis://localhost:6379/0

file_storage: local
files_path: sites/%s/files

apps:
  - core

hostname: %s

# Auto-generated — do not edit manually.
# To change DB connection, run: kora site update-db --site %s
db_fingerprint: %s
`, hostname, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, dbPassword, dbPasswordEncrypted,
		hostname, hostname, hostname, cfg.DBFingerprint)

	return os.WriteFile(fmt.Sprintf("%s/site_config.yaml", siteDir), []byte(content), 0644)
}

// createAdminUser hashes the password and inserts a user into _kora_user.
func createAdminUser(db *sql.DB, email, password, fullName string) error {
	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	_, err = db.Exec(
		`INSERT INTO _kora_user (name, email, password_hash, full_name, roles)
		 VALUES (?, ?, ?, ?, ?)`,
		ulid.Make().String(), email, passwordHash, fullName, "Administrator",
	)
	if err != nil {
		return fmt.Errorf("inserting admin user: %w", err)
	}
	return nil
}

// ensureConfigVersion creates an initial config version if none exists for the site.
func ensureConfigVersion(db *sql.DB, hostname string) {
	var count int
	db.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = ?", hostname).Scan(&count)
	if count > 0 {
		return
	}

	versionID := ulid.Make().String()
	_, err := db.Exec(
		`INSERT INTO _kora_config_version (id, site, version, created_by, label, status, config)
		 VALUES (?, ?, 1, 'setup', 'Initial setup', 'Active', '{}')`,
		versionID, hostname,
	)
	if err != nil {
		// Non-fatal — site is still usable.
		return
	}
	// Mark as active.
	db.Exec("UPDATE _kora_config_version SET is_active = 1 WHERE id = ?", versionID)
}
