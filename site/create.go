package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/contract"
	sqlDialect "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

// CreateSiteInput holds all parameters needed to create a new site.
// DB fields are optional — when empty, platform defaults are applied.
type CreateSiteInput struct {
	// Site identity (required).
	Hostname string
	// SiteID, when supplied by a trusted management command, preserves the
	// Engine tenant identity across provisioning retries. Empty keeps the
	// existing self-hosted generated-ID behavior.
	SiteID string
	// RuntimeCellID is the v1 storage adapter for the deployment assignment.
	// It is written only when creating an unassigned provisioning row; retries
	// cannot silently move an already active tenant.
	RuntimeCellID string

	// DB connection (all optional — defaults from platform config when empty).
	DBType     string // "mysql" or "libsql"
	DBHost     string
	DBPort     int
	DBName     string
	DBUser     string
	DBPassword string

	// Admin account (required).
	AdminEmail     string
	AdminFullName  string // defaults to "Administrator"
	CloudSubjectID string

	// DisplayName optionally seeds the tenant's branding.app_name (the name
	// shown on sign-in, in the sidebar and as the page title). Empty keeps the
	// hostname-derived label. It never affects the hostname or database name.
	DisplayName string

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

// ResolvedTenantConfig returns the tenant database configuration that
// CreateSite will use after applying its defaults. Callers that need to admit
// work before provisioning can use this to reserve the same physical endpoint.
func (in CreateSiteInput) ResolvedTenantConfig() *SiteConfig {
	in.applyDefaults()
	return &SiteConfig{
		DBType: in.DBType, DBHost: in.DBHost, DBPort: in.DBPort, DBName: in.DBName,
		DBUser: in.DBUser, DBPassword: in.DBPassword,
		Hostname: in.Hostname,
	}
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

// CreateSite creates a complete Kigae site: database, system tables, admin user, config version.
// This is the single canonical site creation codepath used by both CLI setup and the console API.
func CreateSite(input CreateSiteInput) (*CreateSiteResult, error) {
	return CreateSiteContext(context.Background(), input)
}

// CreateSiteContext provisions a tenant while respecting the lifecycle owner's
// cancellation between and during database operations. Existing self-hosted
// callers can continue to use CreateSite.
func CreateSiteContext(ctx context.Context, input CreateSiteInput) (*CreateSiteResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("site creation requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input.applyDefaults()
	if len(strings.TrimSpace(input.CloudSubjectID)) < 3 || len(strings.TrimSpace(input.CloudSubjectID)) > 160 {
		return nil, errors.New("Cloud subject ID must be between 3 and 160 bytes")
	}
	if err := ValidateTenantDatabaseName(input.DBType, input.DBName); err != nil {
		return nil, fmt.Errorf("invalid tenant database name: %w", err)
	}
	if input.SiteID != "" {
		if _, err := contract.ParseTenantID(input.SiteID); err != nil {
			return nil, fmt.Errorf("invalid canonical tenant ID: %w", err)
		}
	}
	runtimeCellID, err := normalizeRuntimeCellID(input.RuntimeCellID)
	if err != nil {
		return nil, fmt.Errorf("invalid tenant deployment assignment: %w", err)
	}
	input.RuntimeCellID = runtimeCellID
	displayName, err := contract.NormalizeTenantDisplayName(input.DisplayName)
	if err != nil {
		return nil, fmt.Errorf("invalid tenant display name: %w", err)
	}
	input.DisplayName = displayName

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
		discoveredPlatformDB, err = dbaccess.Open(cfg.DBType, input.PlatformDBDSN)
		if err == nil {
			err = discoveredPlatformDB.PingContext(ctx)
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
	if err := BootstrapPlatformRegistryContext(ctx, input.PlatformDB, sqlDialect.Resolve(input.PlatformDBType)); err != nil {
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
	if err := ensurePlatformSiteRegistrationAssignmentContext(ctx, input.PlatformDB, input.PlatformDBType, siteCfg, "provisioning", input.RuntimeCellID, input.SiteID); err != nil {
		return nil, fmt.Errorf("persisting platform site provisioning intent: %w", err)
	}
	siteID, err := registeredSiteIDContext(ctx, input.PlatformDB, input.PlatformDBType, input.Hostname)
	if err != nil {
		return nil, fmt.Errorf("reading canonical site id: %w", err)
	}
	if siteID == "" {
		return nil, fmt.Errorf("platform site directory returned an empty canonical site id")
	}

	// Step 1: Create database.
	if err := CreateDatabaseContext(ctx, input, siteCfg); err != nil {
		return nil, fmt.Errorf("creating database: %w", err)
	}

	// Step 2: Connect to the new database.
	// For LibSQL, open a fresh connection just like the startup check does —
	// this avoids any connection-pool auth issues with the libsql HTTP driver.
	var db *sql.DB
	isOwnedDB := true
	if input.DBType == "libsql" {
		if dsn := os.Getenv("DB_DSN"); dsn != "" {
			db, err = dbaccess.Open("libsql", dsn)
			if err != nil {
				return nil, fmt.Errorf("opening libsql connection: %w", err)
			}
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(0)
			db.SetConnMaxLifetime(25 * time.Second)
			if err := db.PingContext(ctx); err != nil {
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
		db, err = ConnectContext(ctx, siteCfg)
		if err != nil {
			return nil, fmt.Errorf("connecting to database: %w", err)
		}
	}

	// Step 3: Bootstrap system tables.
	if err := BootstrapSystemTablesContext(ctx, db, sqlDialect.Resolve(input.DBType)); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("bootstrapping system tables: %w", err)
	}

	// Step 4: Create admin user.
	dialect := sqlDialect.Resolve(input.DBType)
	if err := createPasswordlessAdminUserContext(ctx, db, dialect, input.AdminEmail, input.AdminFullName, input.Hostname, input.CloudSubjectID); err != nil {
		if isOwnedDB {
			db.Close()
		}
		return nil, fmt.Errorf("creating admin user: %w", err)
	}

	// Step 5: Create the initial config version for site history and review.
	if err := ensureConfigVersionContext(ctx, db, dialect, input.Hostname, domains); err != nil {
		if isOwnedDB {
			_ = db.Close()
		}
		return nil, fmt.Errorf("creating initial site config version: %w", err)
	}

	// Step 5b: Seed the workspace display name. A retry keeps whatever value
	// is already stored so a later administrator rename is never reverted.
	if err := seedTenantDisplayNameContext(ctx, db, dialect, input.Hostname, input.DisplayName); err != nil {
		if isOwnedDB {
			_ = db.Close()
		}
		return nil, fmt.Errorf("seeding tenant display name: %w", err)
	}

	// Step 6: Publish the tenant only after its database and bootstrap are ready.
	if _, err := NewSQLSiteRegistry(input.PlatformDB, input.PlatformDBType).SetStatusContext(ctx, siteID, "active"); err != nil {
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
	return registeredSiteIDContext(context.Background(), platformDB, platformDBType, hostname)
}

func registeredSiteIDContext(ctx context.Context, platformDB *sql.DB, platformDBType, hostname string) (string, error) {
	if platformDB == nil {
		return "", nil
	}
	query := sqlDialect.Rebind(sqlDialect.Resolve(platformDBType), `SELECT site_id FROM _kora_site_registry WHERE site = ?`)
	var id string
	if err := platformDB.QueryRowContext(ctx, query, hostname).Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

// PasswordlessPasswordHash marks a tenant user that signs in only through the
// Cloud handoff or Cloud OIDC. It is not a valid bcrypt hash, so no password
// can ever match it.
const PasswordlessPasswordHash = "$kora$passwordless$disabled"

func createPasswordlessAdminUserContext(ctx context.Context, db *sql.DB, dialect sqlDialect.QueryDialect, email, fullName, site, cloudSubject string) error {
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
	// Cloud owner membership maps to the Engine tenant Administrator role.
	// Cloud retains organization and billing authority; Engine enforces tenant permissions.
	_, err := db.ExecContext(ctx,
		sqlDialect.Rebind(dialect, query),
		strings.TrimSpace(cloudSubject), site, email, PasswordlessPasswordHash, fullName, contract.CloudOwnerEngineRole,
	)
	if err != nil {
		return fmt.Errorf("inserting passwordless Cloud administrator: %w", err)
	}
	return nil
}

// seedTenantDisplayNameContext stores branding.app_name only when no value
// exists yet. An empty name leaves the hostname-derived label in place.
func seedTenantDisplayNameContext(ctx context.Context, db *sql.DB, dialect sqlDialect.QueryDialect, site, displayName string) error {
	if displayName == "" {
		return nil
	}
	query := `INSERT INTO _kora_site_setting (site, setting_key, setting_value) VALUES (?, ?, ?)`
	driver, _ := dialect.(interface{ DriverName() string })
	if driver == nil {
		return errors.New("tenant display name seeding requires a known SQL dialect")
	}
	switch driver.DriverName() {
	case "mysql":
		query += ` ON DUPLICATE KEY UPDATE setting_value = setting_value`
	case "postgres", "libsql":
		query += ` ON CONFLICT (site, setting_key) DO NOTHING`
	default:
		return fmt.Errorf("tenant display name seeding does not support dialect %q", driver.DriverName())
	}
	if _, err := db.ExecContext(ctx, sqlDialect.Rebind(dialect, query), site, "branding.app_name", displayName); err != nil {
		return fmt.Errorf("inserting branding.app_name: %w", err)
	}
	return nil
}

// ensureConfigVersion creates an initial config version if none exists for the site.
// domains are persisted in the config JSON so they survive container redeploys.
func ensureConfigVersion(db *sql.DB, dialect sqlDialect.QueryDialect, hostname string, domains []string) error {
	return ensureConfigVersionContext(context.Background(), db, dialect, hostname, domains)
}

func ensureConfigVersionContext(ctx context.Context, db *sql.DB, dialect sqlDialect.QueryDialect, hostname string, domains []string) error {
	var count int
	if err := db.QueryRowContext(ctx, sqlDialect.Rebind(dialect, "SELECT COUNT(*) FROM _kora_config_version WHERE site = ?"), hostname).Scan(&count); err != nil {
		return fmt.Errorf("checking initial config version: %w", err)
	}
	if count > 0 {
		var existingID string
		if err := db.QueryRowContext(ctx, sqlDialect.Rebind(dialect, "SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"), hostname).Scan(&existingID); err != nil {
			return fmt.Errorf("recovering existing initial config version: %w", err)
		}
		if err := activateConfigVersionContext(ctx, db, dialect, existingID); err != nil {
			return fmt.Errorf("recovering initial config version activation: %w", err)
		}
		return nil
	}

	domainsJSON, err := json.Marshal(domains)
	if err != nil {
		return fmt.Errorf("encoding initial config domains: %w", err)
	}
	configJSON := fmt.Sprintf(`{"domains": %s}`, string(domainsJSON))
	versionID := ulid.Make().String()
	_, err = db.ExecContext(ctx,
		sqlDialect.Rebind(dialect, `INSERT INTO _kora_config_version (id, site, version, created_by, label, status, config)
		 VALUES (?, ?, 1, 'setup', 'Initial setup', 'Active', ?)`),
		versionID, hostname, configJSON,
	)
	if err != nil {
		return fmt.Errorf("inserting initial config version: %w", err)
	}
	// Mark as active.
	activeFlag := any(true)
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "postgres" {
		activeFlag = 1
	}
	return activateConfigVersionWithFlagContext(ctx, db, dialect, versionID, activeFlag)
}

func activateConfigVersionContext(ctx context.Context, db *sql.DB, dialect sqlDialect.QueryDialect, versionID string) error {
	activeFlag := any(true)
	if driver, ok := dialect.(interface{ DriverName() string }); ok && driver.DriverName() == "postgres" {
		activeFlag = 1
	}
	return activateConfigVersionWithFlagContext(ctx, db, dialect, versionID, activeFlag)
}

func activateConfigVersionWithFlagContext(ctx context.Context, db *sql.DB, dialect sqlDialect.QueryDialect, versionID string, activeFlag any) error {
	if ctx == nil {
		return fmt.Errorf("activating config version requires context")
	}
	if _, err := db.ExecContext(ctx, sqlDialect.Rebind(dialect, "UPDATE _kora_config_version SET is_active = ? WHERE id = ?"), activeFlag, versionID); err != nil {
		return fmt.Errorf("updating active flag: %w", err)
	}
	var active int
	if err := db.QueryRowContext(ctx, sqlDialect.Rebind(dialect, "SELECT is_active FROM _kora_config_version WHERE id = ?"), versionID).Scan(&active); err != nil {
		return fmt.Errorf("verifying active flag: %w", err)
	}
	if active != 1 {
		return fmt.Errorf("active flag persisted as %d", active)
	}
	return nil
}
