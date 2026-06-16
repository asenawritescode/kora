package site

import (
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

	// ExtraDomains are additional domains this site responds to (e.g. public proxy host).
	// Appended to the Hostname in the site's domains list.
	ExtraDomains []string

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

	domains := []string{input.Hostname}
	domains = append(domains, input.ExtraDomains...)

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
		DomainsList: domains,
	}

	// Step 1: Create database.
	if err := CreateDatabase(siteCfg); err != nil {
		return nil, fmt.Errorf("creating database: %w", err)
	}

	// Step 2: Connect to the new database.
	// For LibSQL, open a fresh connection just like the startup check does —
	// this avoids any connection-pool auth issues with the libsql HTTP driver.
	var db *sql.DB
	var err error
	isOwnedDB := true
	if input.DBType == "libsql" {
		if dsn := os.Getenv("DB_DSN"); dsn != "" {
			db, err = sql.Open("libsql", dsn)
			if err != nil {
				return nil, fmt.Errorf("opening libsql connection: %w", err)
			}
			db.SetMaxOpenConns(1) // single connection avoids pool auth issues with HTTP driver
			if err := db.Ping(); err != nil {
				db.Close()
				return nil, fmt.Errorf("pinging libsql: %w", err)
			}
		} else if input.PlatformDB != nil {
			db = input.PlatformDB
			isOwnedDB = false
		} else {
			db, err = Connect(siteCfg)
			if err != nil {
				return nil, fmt.Errorf("connecting to database: %w", err)
			}
		}
	} else {
		db, err = Connect(siteCfg)
		if err != nil {
			return nil, fmt.Errorf("connecting to database: %w", err)
		}
	}

	// Step 3: Bootstrap system tables.
	if err := BootstrapSystemTables(db, sqlDialect.Resolve(input.DBType)); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}

	// Step 4: Create admin user.
	if err := createAdminUser(db, input.AdminEmail, input.AdminPassword, input.AdminFullName); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("creating admin user: %w", err)
	}

	// Step 5: Create initial config version (used by DiscoverSitesFromDB).
	ensureConfigVersion(db, input.Hostname)

	// Step 6: Build empty registry.
	registry := doctype.NewRegistry()
	registry.LoadFull(nil, nil, nil)

	return &CreateSiteResult{
		Config:   siteCfg,
		DB:       db,
		Registry: registry,
	}, nil
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
