// Package site manages site configuration and multi-tenancy.
package site

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	sqlDialect "github.com/asenawritescode/kora/db"
)

// SiteConfig holds the configuration for a single site/tenant.
type SiteConfig struct {
	// Database connection settings.
	DBType     string `yaml:"db_type"` // "mysql" or "libsql"
	DBHost     string `yaml:"db_host"`
	DBPort     int    `yaml:"db_port"`
	DBName     string `yaml:"db_name"`
	DBUser     string `yaml:"db_user"`
	DBPassword string `yaml:"db_password"`

	// Redis connection.
	RedisURL string `yaml:"redis_url"`

	// File storage configuration.
	FileStorage string `yaml:"file_storage"` // "local" or "s3"
	FilesPath   string `yaml:"files_path"`

	// Apps loaded for this site.
	Apps []string `yaml:"apps"`

	// Hostname used for site resolution by Host header.
	Hostname string `yaml:"hostname"`

	// Domains lists all domains this site responds to (including Hostname).
	// If empty, defaults to [hostname].
	DomainsList []string `yaml:"domains"`

	// DBFingerprint is a SHA-256 hash of (host:port:dbname) computed at site creation.
	// Validated on every site load to detect accidental DB connection changes.
	DBFingerprint string `yaml:"db_fingerprint"`

	// DBPasswordEncrypted is set to true when db_password is encrypted with the server key.
	// When false or absent, db_password is treated as plaintext (backwards compat).
	DBPasswordEncrypted bool `yaml:"db_password_encrypted"`
}

// Domains returns all domains for this site. Falls back to [Hostname] if not configured.
func (s *SiteConfig) Domains() []string {
	if len(s.DomainsList) > 0 {
		return s.DomainsList
	}
	if s.Hostname != "" {
		return []string{s.Hostname}
	}
	return []string{"localhost"}
}

// CommonConfig holds configuration shared across all sites.
type CommonConfig struct {
	RedisURL   string `yaml:"redis_url"`
	DBType     string `yaml:"db_type"` // "mysql" or "libsql"
	DBHost     string `yaml:"db_host"`
	DBPort     int    `yaml:"db_port"`
	DBUser     string `yaml:"db_user"`
	DBPassword string `yaml:"db_password"`
	HTTPPort   int    `yaml:"http_port"`
	Workers    int    `yaml:"workers"`
	LogLevel   string `yaml:"log_level"`
	LogFormat  string `yaml:"log_format"`

	// App branding.
	AppName      string `yaml:"app_name"`
	Version      string `yaml:"version"`
	PrimaryColor string `yaml:"primary_color"`

	// Session & security.
	SessionLifetimeHours int  `yaml:"session_lifetime_hours"`
	CSRFSecure           bool `yaml:"csrf_secure"`

	// Rate limiting.
	RateLimitRPS   int `yaml:"rate_limit_rps"`
	RateLimitBurst int `yaml:"rate_limit_burst"`

	// Database pool.
	DBMaxOpenConns int `yaml:"db_max_open_conns"`
	DBMaxIdleConns int `yaml:"db_max_idle_conns"`

	// API pagination.
	APIDefaultLimit int `yaml:"api_default_limit"`
	APIMaxLimit     int `yaml:"api_max_limit"`

	// Server timeouts (seconds).
	ReadTimeout  int `yaml:"read_timeout_secs"`
	WriteTimeout int `yaml:"write_timeout_secs"`
	IdleTimeout  int `yaml:"idle_timeout_secs"`

	// Admin role name (defaults to "Administrator").
	AdminRole string `yaml:"admin_role"`

	// TLS.
	TLSMode  string `yaml:"tls_mode"`
	TLSEmail string `yaml:"tls_email"`
}

// Site represents a running site with its database connection and config.
type Site struct {
	Config   *SiteConfig
	DB       *sql.DB
	Hostname string
}

// DSN returns the connection string for this site, based on its DBType.
func (s *SiteConfig) DSN() string {
	dbType := s.DBType
	if dbType == "" {
		dbType = "mysql"
	}
	switch dbType {
	case "libsql":
		// LibSQL remote-only — no embedded/file fallback.
		// Accepts: libsql://host or http(s)://host
		if strings.HasPrefix(s.DBHost, "libsql://") {
			return s.DBHost
		}
		if strings.HasPrefix(s.DBHost, "http") {
			// Embed credentials in URL if provided and not already present.
			if s.DBUser != "" && s.DBPassword != "" && !strings.Contains(s.DBHost, "@") {
				rest := strings.TrimPrefix(s.DBHost, "https://")
				rest = strings.TrimPrefix(rest, "http://")
				return fmt.Sprintf("http://%s:%s@%s", s.DBUser, s.DBPassword, rest)
			}
			return s.DBHost
		}
		// No valid remote URL — this will fail at connect time with a clear error.
		return s.DBHost
	default:
		// MySQL / MariaDB.
		return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci",
			s.DBUser, s.DBPassword, s.DBHost, s.DBPort, s.DBName)
	}
}

// LoadCommonConfig reads the common site config from a YAML file.
func LoadCommonConfig(path string) (*CommonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading common config: %w", err)
	}
	cfg := &CommonConfig{
		DBHost:               "127.0.0.1",
		DBUser:               "root",
		HTTPPort:             8000,
		Workers:              4,
		LogLevel:             "info",
		LogFormat:            "json",
		AppName:              "Kora",
		Version:              "0.1.0",
		PrimaryColor:         "#2563eb",
		SessionLifetimeHours: 24,
		RateLimitRPS:         100,
		RateLimitBurst:       20,
		DBMaxOpenConns:       25,
		DBMaxIdleConns:       5,
		APIDefaultLimit:      50,
		APIMaxLimit:          500,
		ReadTimeout:          30,
		WriteTimeout:         30,
		IdleTimeout:          120,
		AdminRole:            "Administrator",
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing common config: %w", err)
	}
	cfg.ApplyEnvOverrides()
	return cfg, nil
}

// ApplyEnvOverrides overlays KORA_* environment variables on top of YAML values.
// Env vars take precedence — this lets secrets like DB passwords stay out of YAML.
func (c *CommonConfig) ApplyEnvOverrides() {
	if v := os.Getenv("KORA_DB_TYPE"); v != "" {
		c.DBType = v
	}
	if v := os.Getenv("KORA_DB_HOST"); v != "" {
		c.DBHost = v
	}
	if v := os.Getenv("KORA_DB_USER"); v != "" {
		c.DBUser = v
	}
	if v := os.Getenv("KORA_DB_PASSWORD"); v != "" {
		c.DBPassword = v
	}
	if v := os.Getenv("KORA_HTTP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.HTTPPort = n
		}
	}
	if v := os.Getenv("KORA_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("KORA_LOG_FORMAT"); v != "" {
		c.LogFormat = v
	}
	if v := os.Getenv("KORA_APP_NAME"); v != "" {
		c.AppName = v
	}
	if v := os.Getenv("KORA_ADMIN_ROLE"); v != "" {
		c.AdminRole = v
	}
	if v := os.Getenv("KORA_SESSION_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SessionLifetimeHours = n
		}
	}
}

// CommonConfigFromEnv builds a CommonConfig from environment variables with sensible defaults.
// Used when common_site_config.yaml is missing (container/console-first deployments).
func CommonConfigFromEnv() *CommonConfig {
	return &CommonConfig{
		DBType:               getEnv("KORA_DB_TYPE", "mysql"),
		DBHost:               getEnv("KORA_DB_HOST", "127.0.0.1"),
		DBPort:               getEnvInt("KORA_DB_PORT", 3306),
		DBUser:               getEnv("KORA_DB_USER", ""),
		DBPassword:           getEnv("KORA_DB_PASSWORD", ""),
		HTTPPort:             getEnvInt("KORA_HTTP_PORT", 8000),
		LogLevel:             getEnv("KORA_LOG_LEVEL", "info"),
		LogFormat:            getEnv("KORA_LOG_FORMAT", "json"),
		AppName:              getEnv("KORA_APP_NAME", "Kora"),
		Version:              getEnv("KORA_VERSION", "0.3.0"),
		PrimaryColor:         getEnv("KORA_PRIMARY_COLOR", "#000000"),
		SessionLifetimeHours: getEnvInt("KORA_SESSION_HOURS", 72),
		RateLimitRPS:         getEnvInt("KORA_RATE_LIMIT", 100),
		RateLimitBurst:       getEnvInt("KORA_RATE_BURST", 20),
		DBMaxOpenConns:       getEnvInt("KORA_DB_MAX_OPEN", 25),
		DBMaxIdleConns:       getEnvInt("KORA_DB_MAX_IDLE", 5),
		APIDefaultLimit:      getEnvInt("KORA_API_DEFAULT_LIMIT", 50),
		APIMaxLimit:          getEnvInt("KORA_API_MAX_LIMIT", 500),
		ReadTimeout:          getEnvInt("KORA_READ_TIMEOUT", 30),
		WriteTimeout:         getEnvInt("KORA_WRITE_TIMEOUT", 60),
		IdleTimeout:          getEnvInt("KORA_IDLE_TIMEOUT", 120),
		AdminRole:            getEnv("KORA_ADMIN_ROLE", "Administrator"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// LoadSiteConfig reads a site configuration from a YAML file.
// If db_password_encrypted is true, the password is decrypted using KORA_SECRET_KEY.
func LoadSiteConfig(path string) (*SiteConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading site config: %w", err)
	}
	cfg := &SiteConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing site config: %w", err)
	}

	// Decrypt password if it was stored encrypted.
	if cfg.DBPasswordEncrypted && cfg.DBPassword != "" {
		plain, err := decryptPassword(cfg.DBPassword)
		if err != nil {
			return nil, fmt.Errorf("decrypting db password for site %s: %w (is KORA_SECRET_KEY set?)", cfg.Hostname, err)
		}
		cfg.DBPassword = plain
	}

	// If no password in config, check platform default.
	if cfg.DBPassword == "" {
		if p := os.Getenv("KORA_DB_PASSWORD"); p != "" {
			cfg.DBPassword = p
		}
	}

	return cfg, nil
}

// DiscoverSitesFromDB finds sites from both the durable platform registry and,
// for backwards compatibility, the legacy _kora_config_version table. Registry
// entries take precedence over legacy entries with the same site name.
func DiscoverSitesFromDB(db *sql.DB) ([]DBSiteInfo, error) {
	registrySites, regErr := discoverSitesFromRegistry(db)
	if regErr != nil && !isSiteRegistryMissing(regErr) {
		return nil, regErr
	}

	// Always also discover from legacy config version to catch sites that
	// predate the registry table. Registry entries override legacy ones.
	legacySites, legErr := discoverSitesFromLegacyConfig(db)
	if legErr != nil && !isLegacyConfigMissing(legErr) {
		return nil, legErr
	}

	// Merge: registry takes precedence.
	seen := make(map[string]int, len(registrySites))
	for i, s := range registrySites {
		seen[s.Name] = i
	}
	for _, s := range legacySites {
		if _, exists := seen[s.Name]; !exists {
			registrySites = append(registrySites, s)
		}
	}
	return registrySites, nil
}

// discoverSitesFromLegacyConfig reads site names from _kora_config_version.
func discoverSitesFromLegacyConfig(db *sql.DB) ([]DBSiteInfo, error) {
	rows, err := db.Query("SELECT DISTINCT site, config FROM _kora_config_version WHERE status = 'Active'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites []DBSiteInfo
	for rows.Next() {
		var site, configJSON string
		if err := rows.Scan(&site, &configJSON); err != nil {
			return nil, err
		}
		info := DBSiteInfo{Name: site, Domains: []string{site}}
		if configJSON != "" && configJSON != "{}" {
			var cfg struct {
				Domains []string `json:"domains"`
			}
			if json.Unmarshal([]byte(configJSON), &cfg) == nil && len(cfg.Domains) > 0 {
				info.Domains = cfg.Domains
			}
		}
		sites = append(sites, info)
	}
	return sites, rows.Err()
}

// ReconstructSiteConfig builds a SiteConfig from platform defaults and persisted domains.
func ReconstructSiteConfig(hostname string, common *CommonConfig, domains []string) *SiteConfig {
	if len(domains) == 0 {
		domains = []string{hostname}
	}
	dbPort := common.DBPort
	if dbPort == 0 {
		dbPort = 3306
	}
	return &SiteConfig{
		DBType:      common.DBType,
		DBHost:      common.DBHost,
		DBPort:      dbPort,
		DBUser:      common.DBUser,
		DBPassword:  common.DBPassword,
		DBName:      strings.ReplaceAll(hostname, ".", "_"),
		Hostname:    hostname,
		DomainsList: domains,
		Apps:        []string{"core"},
	}
}

// ReconstructSiteConfigFromDBInfo builds a SiteConfig from discovered site metadata
// and fills any missing values from platform defaults.
func ReconstructSiteConfigFromDBInfo(info DBSiteInfo, common *CommonConfig) *SiteConfig {
	cfg := ReconstructSiteConfig(info.Name, common, info.Domains)
	if info.DBType != "" {
		cfg.DBType = info.DBType
	}
	if info.DBHost != "" {
		cfg.DBHost = info.DBHost
	}
	if info.DBPort != 0 {
		cfg.DBPort = info.DBPort
	}
	if info.DBName != "" {
		cfg.DBName = info.DBName
	}
	if info.DBUser != "" {
		cfg.DBUser = info.DBUser
	}
	if info.DBPassword != "" {
		cfg.DBPassword = info.DBPassword
	}
	return cfg
}

// Connect opens a database connection for the site.
func Connect(cfg *SiteConfig) (*sql.DB, error) {
	dsn := cfg.DSN()
	// DB_DSN env var overrides per-site DSN only for LibSQL (shared-DB mode)
	// or when the site config has no explicit DB credentials.
	// In MySQL multi-database mode with KORA_DB_USER/KORA_DB_PASSWORD set,
	// the site connects to its own database using the site-specific DSN.
	if envDSN := os.Getenv("DB_DSN"); envDSN != "" {
		if cfg.DBType == "libsql" || cfg.DBUser == "" {
			dsn = envDSN
		}
	}
	driver := cfg.DBType
	if driver == "" {
		driver = os.Getenv("KORA_DB_TYPE")
	}
	if driver == "" {
		driver = "mysql" // Backwards compat: existing site configs may not have db_type.
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database connection: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	// Connection pool tuning.
	if driver == "libsql" {
		// LibSQL HTTP streams expire server-side after ~30s idle.
		// Don't pool idle connections — open fresh each time.
		// Set a max open limit to prevent unbounded concurrent connections
		// from overwhelming the LibSQL server (e.g. during sweep thundering herds).
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(0)
		db.SetConnMaxLifetime(25 * time.Second)
	} else {
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
	}
	return db, nil
}

// NewSite creates a new Site with a database connection.
func NewSite(cfg *SiteConfig) (*Site, error) {
	db, err := Connect(cfg)
	if err != nil {
		return nil, err
	}
	return &Site{
		Config:   cfg,
		DB:       db,
		Hostname: cfg.Hostname,
	}, nil
}

// CreateDatabase creates the site's database if it doesn't exist.
// Connects without a database name, issues CREATE DATABASE IF NOT EXISTS.
func CreateDatabase(input CreateSiteInput, cfg *SiteConfig) error {
	driver := cfg.DBType
	if driver == "" {
		driver = os.Getenv("KORA_DB_TYPE")
	}
	if driver == "" {
		driver = "mysql" // Backwards compat: existing site configs may not have db_type.
	}
	// LibSQL/SQLite creates the database file on first connection — no CREATE DATABASE needed.
	if driver != "mysql" {
		return nil
	}

	// Prefer the already-open platform connection when available. This keeps
	// DB_DSN-based startup working without requiring separate host/user env vars.
	if input.PlatformDB != nil {
		if _, err := input.PlatformDB.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", cfg.DBName)); err != nil {
			return fmt.Errorf("creating database %s: %w", cfg.DBName, err)
		}
		return nil
	}

	// Otherwise fall back to an explicit MySQL DSN or the legacy host/user/password fields.
	dsn := input.PlatformDBDSN
	if dsn == "" {
		dsn = mysqlServerDSN(cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword)
	}
	if dsn == "" {
		return fmt.Errorf("no MySQL connection details available for creating database %s", cfg.DBName)
	}

	baseDSN, err := mysqlBaseDSN(dsn)
	if err != nil {
		return fmt.Errorf("parsing MySQL DSN: %w", err)
	}

	db, err := sql.Open("mysql", baseDSN)
	if err != nil {
		return fmt.Errorf("connecting to MySQL server: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("pinging MySQL server: %w", err)
	}

	if _, err := db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", cfg.DBName)); err != nil {
		return fmt.Errorf("creating database %s: %w", cfg.DBName, err)
	}
	return nil
}

func mysqlServerDSN(host string, port int, user, password string) string {
	if host == "" || user == "" {
		return ""
	}
	if port == 0 {
		port = 3306
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/?parseTime=true&charset=utf8mb4", user, password, host, port)
}

func mysqlBaseDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	cfg.DBName = ""
	return cfg.FormatDSN(), nil
}

// DeleteSiteInput holds the parameters needed to tear down a site.
type DeleteSiteInput struct {
	DB             *sql.DB
	Dialect        sqlDialect.Dialect
	Hostname       string
	PlatformDB     *sql.DB
	PlatformDBType string

	// MySQL-specific (for DROP DATABASE).
	DBType     string
	DBName     string
	DBHost     string
	DBPort     int
	DBUser     string
	DBPassword string
	DBDSN      string
}

// DeleteSite tears down a site completely, removing all data and closing connections.
// For MySQL: drops the entire database via a temporary connection.
// For LibSQL: drops all application tables and cleans shared system-table rows.
func DeleteSite(input DeleteSiteInput) error {
	dbType := input.DBType
	if dbType == "" {
		dbType = "mysql"
	}

	switch dbType {
	case "mysql":
		// Close the site's DB connection before dropping the database.
		if input.DB != nil {
			input.DB.Close()
		}

		// Open a temporary connection without a database name.
		dsn := input.DBDSN
		if dsn == "" {
			dsn = mysqlServerDSN(input.DBHost, input.DBPort, input.DBUser, input.DBPassword)
		}
		if dsn == "" {
			return fmt.Errorf("no MySQL connection details available for dropping database %s", input.DBName)
		}
		dsn, err := mysqlBaseDSN(dsn)
		if err != nil {
			return fmt.Errorf("parsing MySQL DSN: %w", err)
		}
		tempDB, err := sql.Open("mysql", dsn)
		if err != nil {
			return fmt.Errorf("connecting for teardown: %w", err)
		}
		defer tempDB.Close()

		if _, err := tempDB.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS %s", input.Dialect.QuoteIdent(input.DBName))); err != nil {
			return fmt.Errorf("dropping database %s: %w", input.DBName, err)
		}
		if err := removePlatformSiteRegistration(input.PlatformDB, input.PlatformDBType, input.Hostname); err != nil {
			return fmt.Errorf("removing platform site registration: %w", err)
		}
		slog.Info("site database dropped", "hostname", input.Hostname, "db_name", input.DBName)
		return nil

	case "libsql":
		if input.DB == nil {
			return nil
		}

		// Drop all application tables (tab%).
		rows, err := input.DB.Query("SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'tab%'")
		if err != nil {
			return fmt.Errorf("listing tables: %w", err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return fmt.Errorf("scanning table name: %w", err)
			}
			tables = append(tables, name)
		}
		rows.Close()

		for _, t := range tables {
			if _, err := input.DB.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s", input.Dialect.QuoteIdent(t))); err != nil {
				return fmt.Errorf("dropping table %s: %w", t, err)
			}
		}

		// Clean up shared system-table rows for this site.
		if _, err := input.DB.Exec("DELETE FROM _kora_config_version WHERE site = ?", input.Hostname); err != nil {
			return fmt.Errorf("cleaning config versions: %w", err)
		}
		if _, err := input.DB.Exec("DELETE FROM _kora_secret WHERE site = ?", input.Hostname); err != nil {
			return fmt.Errorf("cleaning secrets: %w", err)
		}
		if err := removePlatformSiteRegistration(input.PlatformDB, input.PlatformDBType, input.Hostname); err != nil {
			return fmt.Errorf("removing platform site registration: %w", err)
		}

		input.DB.Close()
		slog.Info("site data deleted", "hostname", input.Hostname, "tables_dropped", len(tables))
		return nil
	}

	return fmt.Errorf("unsupported db type: %s", dbType)
}

// Close closes the site's database connection.
func (s *Site) Close() error {
	if s.DB != nil {
		return s.DB.Close()
	}
	return nil
}
