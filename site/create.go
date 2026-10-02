package site

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
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
	PlatformDBDSN      string

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
	if in.PlatformDBDSN != "" {
		if dsnCfg, err := mysql.ParseDSN(in.PlatformDBDSN); err == nil {
			host, port := mysqlHostPort(dsnCfg.Addr)
			if in.PlatformDBHost == "" && host != "" {
				in.PlatformDBHost = host
			}
			if in.PlatformDBPort == 0 && port != 0 {
				in.PlatformDBPort = port
			}
			if in.PlatformDBUser == "" && dsnCfg.User != "" {
				in.PlatformDBUser = dsnCfg.User
			}
			if in.PlatformDBPassword == "" && dsnCfg.Passwd != "" {
				in.PlatformDBPassword = dsnCfg.Passwd
			}
		}
	}
	if in.DBType == "" {
		if in.PlatformDBType != "" {
			in.DBType = in.PlatformDBType
		} else {
			in.DBType = "mysql"
		}
	}
	if in.PlatformDBType == "" {
		in.PlatformDBType = in.DBType
	}
	if in.DBHost == "" {
		if in.PlatformDBHost != "" {
			in.DBHost = in.PlatformDBHost
		} else {
			in.DBHost = "127.0.0.1"
		}
	}
	if in.DBPort == 0 {
		platformTypeMatches := in.PlatformDBType == "" || strings.EqualFold(in.PlatformDBType, in.DBType)
		if in.PlatformDBPort != 0 && platformTypeMatches {
			in.DBPort = in.PlatformDBPort
		} else {
			in.DBPort = 3306
			if strings.EqualFold(in.DBType, "postgres") {
				in.DBPort = 5432
			}
		}
	}
	if in.DBName == "" {
		// Derive from hostname: dots become underscores.
		in.DBName = strings.ReplaceAll(in.Hostname, ".", "_")
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

func mysqlHostPort(addr string) (string, int) {
	if addr == "" {
		return "", 0
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, 0
	}
	return host, port
}

// CreateSiteResult holds the result of a successful site creation.
type CreateSiteResult struct {
	SiteID   string
	Config   *SiteConfig
	DB       *sql.DB
	Registry *doctype.Registry
}

// CreateSite creates a complete Kora site: database, system tables, admin user, config version.
// This is the single canonical site creation codepath used by both CLI setup and the console API.
func CreateSite(input CreateSiteInput) (*CreateSiteResult, error) {
	input.applyDefaults()

	// CLI setup commonly supplies only PlatformDBDSN. Open a short-lived
	// platform connection for durable site discovery when no shared handle was
	// provided, so sites created outside the console are loaded after restart.
	var discoveredPlatformDB *sql.DB
	if input.PlatformDB == nil && input.PlatformDBDSN != "" {
		cfg := &SiteConfig{
			DBType:     input.PlatformDBType,
			DBHost:     input.PlatformDBHost,
			DBPort:     input.PlatformDBPort,
			DBUser:     input.PlatformDBUser,
			DBPassword: input.PlatformDBPassword,
		}
		if cfg.DBType == "" {
			cfg.DBType = input.DBType
		}
		if cfg.DBType == "" {
			cfg.DBType = "mysql"
		}
		var err error
		discoveredPlatformDB, err = sql.Open(cfg.DBType, input.PlatformDBDSN)
		if err == nil {
			err = discoveredPlatformDB.Ping()
		}
		if err != nil {
			if discoveredPlatformDB != nil {
				discoveredPlatformDB.Close()
			}
			return nil, fmt.Errorf("connecting to platform database: %w", err)
		}
		input.PlatformDB = discoveredPlatformDB
		defer discoveredPlatformDB.Close()
	}
	if input.PlatformDB == nil {
		return nil, fmt.Errorf("durable platform site directory is required to create a site")
	}
	if err := BootstrapPlatformRegistry(input.PlatformDB, sqlDialect.Resolve(input.PlatformDBType)); err != nil {
		return nil, fmt.Errorf("initializing platform site directory: %w", err)
	}

	domains := []string{input.Hostname}
	domains = append(domains, input.ExtraDomains...)

	siteCfg := &SiteConfig{
		DBType:        input.DBType,
		DBHost:        input.DBHost,
		DBPort:        input.DBPort,
		DBName:        input.DBName,
		DBUser:        input.DBUser,
		DBPassword:    input.DBPassword,
		Hostname:      input.Hostname,
		FileStorage:   DefaultFileStorageFromEnv(),
		StorageBucket: "",
		FilesPath:     fmt.Sprintf("sites/%s/files", input.Hostname),
		Apps:          []string{"core"},
		DomainsList:   domains,
	}
	if siteCfg.FileStorage == "s3" {
		siteCfg.StorageBucket = BucketNameForSite(input.Hostname)
	}

	// Publish durable intent before creating tenant data. A retry can reuse this
	// canonical identity after a crash between database creation and activation.
	if err := ensurePlatformSiteRegistrationStatus(input.PlatformDB, input.PlatformDBType, siteCfg, "provisioning"); err != nil {
		return nil, fmt.Errorf("persisting platform site provisioning intent: %w", err)
	}
	siteID, err := registeredSiteID(input.PlatformDB, input.PlatformDBType, input.Hostname)
	if err != nil {
		return nil, fmt.Errorf("reading canonical site id: %w", err)
	}
	if siteID == "" {
		return nil, fmt.Errorf("platform site directory returned an empty canonical site id")
	}

	// Step 1: Create database.
	if err := CreateDatabase(input, siteCfg); err != nil {
		return nil, fmt.Errorf("creating database: %w", err)
	}

	// Step 2: Connect to the new database.
	// For LibSQL, open a fresh connection just like the startup check does —
	// this avoids any connection-pool auth issues with the libsql HTTP driver.
	var db *sql.DB
	isOwnedDB := true
	if input.DBType == "libsql" {
		if dsn := os.Getenv("DB_DSN"); dsn != "" {
			db, err = sql.Open("libsql", dsn)
			if err != nil {
				return nil, fmt.Errorf("opening libsql connection: %w", err)
			}
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(0)
			db.SetConnMaxLifetime(25 * time.Second)
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
	dialect := sqlDialect.Resolve(input.DBType)
	if err := createAdminUser(db, dialect, input.AdminEmail, input.AdminPassword, input.AdminFullName, input.Hostname); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("creating admin user: %w", err)
	}

	// Step 5: Create the initial config version for site history and review.
	ensureConfigVersion(db, dialect, input.Hostname, domains)

	// Step 6: Publish the tenant only after its database and bootstrap are ready.
	if err := NewSQLSiteRegistry(input.PlatformDB, input.PlatformDBType).SetStatus(siteID, "active"); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("activating site in platform directory: %w", err)
	}

	// Step 7: Build empty registry.
	registry := doctype.NewRegistry()
	registry.LoadFull(nil, nil, nil)

	return &CreateSiteResult{
		SiteID:   siteID,
		Config:   siteCfg,
		DB:       db,
		Registry: registry,
	}, nil
}

func registeredSiteID(platformDB *sql.DB, platformDBType, hostname string) (string, error) {
	if platformDB == nil {
		return "", nil
	}
	query := sqlDialect.Rebind(sqlDialect.Resolve(platformDBType), `SELECT site_id FROM _kora_site_registry WHERE site = ?`)
	var id string
	if err := platformDB.QueryRow(query, hostname).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// createAdminUser hashes the password and inserts a user into _kora_user.
// An empty password intentionally creates a passwordless bootstrap account:
// password login is disabled, but magic-link auth can still find the user.
func createAdminUser(db *sql.DB, dialect sqlDialect.QueryDialect, email, password, fullName, site string) error {
	passwordHash := "$kora$passwordless$disabled"
	if strings.TrimSpace(password) != "" {
		var err error
		passwordHash, err = auth.HashPassword(password)
		if err != nil {
			return fmt.Errorf("hashing password: %w", err)
		}
	}

	query := `INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, email_verified_at, roles)
		 VALUES (?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP, ?)`
	if driver, ok := dialect.(interface{ DriverName() string }); ok {
		switch driver.DriverName() {
		case "mysql":
			query += ` ON DUPLICATE KEY UPDATE email = VALUES(email)`
		case "postgres", "libsql":
			query += ` ON CONFLICT (site, email) DO NOTHING`
		}
	}
	_, err := db.Exec(
		sqlDialect.Rebind(dialect, query),
		ulid.Make().String(), site, email, passwordHash, fullName, "Administrator",
	)
	if err != nil {
		return fmt.Errorf("inserting admin user: %w", err)
	}
	return nil
}

// ensureConfigVersion creates an initial config version if none exists for the site.
// domains are persisted in the config JSON so they survive container redeploys.
func ensureConfigVersion(db *sql.DB, dialect sqlDialect.QueryDialect, hostname string, domains []string) {
	var count int
	if err := db.QueryRow(sqlDialect.Rebind(dialect, "SELECT COUNT(*) FROM _kora_config_version WHERE site = ?"), hostname).Scan(&count); err != nil {
		slog.Warn("initial site config-version check failed", "site", hostname, "error", err)
		return
	}
	if count > 0 {
		return
	}

	domainsJSON, _ := json.Marshal(domains)
	configJSON := fmt.Sprintf(`{"domains": %s}`, string(domainsJSON))
	versionID := ulid.Make().String()
	_, err := db.Exec(
		sqlDialect.Rebind(dialect, `INSERT INTO _kora_config_version (id, site, version, created_by, label, status, config)
		 VALUES (?, ?, 1, 'setup', 'Initial setup', 'Active', ?)`),
		versionID, hostname, configJSON,
	)
	if err != nil {
		// Non-fatal — the canonical registry still makes the site routable.
		slog.Warn("initial site config version was not created", "site", hostname, "error", err)
		return
	}
	// Mark as active.
	activeFlag := any(true)
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "postgres" {
		activeFlag = 1
	}
	if _, err := db.Exec(sqlDialect.Rebind(dialect, "UPDATE _kora_config_version SET is_active = ? WHERE id = ?"), activeFlag, versionID); err != nil {
		slog.Warn("initial site config version active flag was not set", "site", hostname, "error", err)
	}
}
