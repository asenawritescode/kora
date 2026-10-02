// Package site manages site configuration and multi-tenancy.
package site

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/url"
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
	DBType     string `yaml:"db_type"` // "mysql", "postgres", or "libsql"
	DBHost     string `yaml:"db_host"`
	DBPort     int    `yaml:"db_port"`
	DBName     string `yaml:"db_name"`
	DBUser     string `yaml:"db_user"`
	DBPassword string `yaml:"db_password"`
	// Per-tenant pool bounds inherited from the platform configuration unless
	// explicitly supplied for this site.
	DBMaxOpenConns int `yaml:"db_max_open_conns,omitempty"`
	DBMaxIdleConns int `yaml:"db_max_idle_conns,omitempty"`

	// Redis connection.
	RedisURL string `yaml:"redis_url"`

	// File storage configuration.
	FileStorage   string `yaml:"file_storage"`   // "local" or "s3"
	StorageBucket string `yaml:"storage_bucket"` // site-specific bucket when FileStorage == "s3"
	FilesPath     string `yaml:"files_path"`

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

	// Trusted network proxies allowed to supply forwarding headers. Empty means
	// the server uses the socket peer address and ignores forwarded client IPs.
	TrustedProxies []string `yaml:"trusted_proxies"`

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

	// SMTP / email delivery.
	SMTPHost     string `yaml:"smtp_host"`
	SMTPPort     int    `yaml:"smtp_port"`
	SMTPUsername string `yaml:"smtp_username"`
	SMTPPassword string `yaml:"smtp_password"`
	SMTPFrom     string `yaml:"smtp_from"`
	SMTPTLSMode  string `yaml:"smtp_tls_mode"`
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
	case "postgres":
		dsn := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(s.DBUser, s.DBPassword),
			Host:   net.JoinHostPort(s.DBHost, strconv.Itoa(s.DBPort)),
			Path:   "/" + s.DBName,
		}
		query := dsn.Query()
		query.Set("sslmode", "disable")
		dsn.RawQuery = query.Encode()
		return dsn.String()
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
		DBMaxIdleConns:       1,
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
	if v := os.Getenv("KORA_TRUSTED_PROXIES"); v != "" {
		c.TrustedProxies = splitCSV(v)
	}
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
	if v := os.Getenv("KORA_SMTP_HOST"); v != "" {
		c.SMTPHost = v
	}
	if v := os.Getenv("KORA_SMTP_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SMTPPort = n
		}
	}
	if v := os.Getenv("KORA_SMTP_USERNAME"); v != "" {
		c.SMTPUsername = v
	}
	if v := os.Getenv("KORA_SMTP_PASSWORD"); v != "" {
		c.SMTPPassword = v
	}
	if v := os.Getenv("KORA_SMTP_FROM"); v != "" {
		c.SMTPFrom = v
	}
	if v := os.Getenv("KORA_SMTP_TLS_MODE"); v != "" {
		c.SMTPTLSMode = v
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
		TrustedProxies:       splitCSV(os.Getenv("KORA_TRUSTED_PROXIES")),
		DBMaxOpenConns:       getEnvInt("KORA_DB_MAX_OPEN", 25),
		DBMaxIdleConns:       getEnvInt("KORA_DB_MAX_IDLE", 1),
		APIDefaultLimit:      getEnvInt("KORA_API_DEFAULT_LIMIT", 50),
		APIMaxLimit:          getEnvInt("KORA_API_MAX_LIMIT", 500),
		ReadTimeout:          getEnvInt("KORA_READ_TIMEOUT", 30),
		WriteTimeout:         getEnvInt("KORA_WRITE_TIMEOUT", 60),
		IdleTimeout:          getEnvInt("KORA_IDLE_TIMEOUT", 120),
		AdminRole:            getEnv("KORA_ADMIN_ROLE", "Administrator"),
		SMTPHost:             getEnv("KORA_SMTP_HOST", ""),
		SMTPPort:             getEnvInt("KORA_SMTP_PORT", 0),
		SMTPUsername:         getEnv("KORA_SMTP_USERNAME", ""),
		SMTPPassword:         getEnv("KORA_SMTP_PASSWORD", ""),
		SMTPFrom:             getEnv("KORA_SMTP_FROM", ""),
		SMTPTLSMode:          getEnv("KORA_SMTP_TLS_MODE", "auto"),
	}
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
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

// DiscoverSitesFromDB reads the canonical platform site directory.
func DiscoverSitesFromDB(db *sql.DB) ([]DBSiteInfo, error) {
	return discoverSitesFromRegistry(db)
}

// ReconstructSiteConfig builds a SiteConfig from platform defaults and persisted domains.
func ReconstructSiteConfig(hostname string, common *CommonConfig, domains []string) *SiteConfig {
	if common == nil {
		common = &CommonConfig{}
	}
	if len(domains) == 0 {
		domains = []string{hostname}
	}
	dbPort := common.DBPort
	if dbPort == 0 {
		dbPort = 3306
	}
	return &SiteConfig{
		DBType:         common.DBType,
		DBHost:         common.DBHost,
		DBPort:         dbPort,
		DBUser:         common.DBUser,
		DBPassword:     common.DBPassword,
		DBMaxOpenConns: common.DBMaxOpenConns,
		DBMaxIdleConns: common.DBMaxIdleConns,
		DBName:         strings.ReplaceAll(hostname, ".", "_"),
		Hostname:       hostname,
		DomainsList:    domains,
		Apps:           []string{"core"},
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
	if info.FileStorage != "" {
		cfg.FileStorage = info.FileStorage
	}
	if info.StorageBucket != "" {
		cfg.StorageBucket = info.StorageBucket
	} else if cfg.FileStorage == "s3" {
		cfg.StorageBucket = BucketNameForSite(cfg.Hostname)
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
	tuneConnectionPool(db, driver, cfg.DBMaxOpenConns, cfg.DBMaxIdleConns)
	return db, nil
}

// tuneConnectionPool keeps driver-specific connection lifetimes shorter than the
// backend's idle timeout so pooled sockets are retired before the server drops them.
func tuneConnectionPool(db *sql.DB, driver string, maxOpen, maxIdle int) {
	useDriverDefaults := maxOpen <= 0 && maxIdle <= 0
	if maxOpen <= 0 {
		maxOpen = 25
	}
	if maxIdle < 0 {
		maxIdle = 0
	}
	if maxIdle == 0 && useDriverDefaults && !strings.EqualFold(driver, "libsql") {
		if strings.EqualFold(driver, "mysql") {
			maxIdle = 1
		} else {
			maxIdle = 5
		}
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	switch driver {
	case "libsql":
		// LibSQL HTTP streams expire server-side after ~30s idle.
		db.SetMaxIdleConns(0)
		db.SetConnMaxLifetime(25 * time.Second)
		db.SetConnMaxIdleTime(20 * time.Second)
	case "mysql":
		// MySQL can leave idle TCP sockets half-open from the application's point of view.
		// Retain one idle socket per tenant. With eager multi-site loading, even a
		// modest per-pool idle cap multiplies across sites and can exhaust MySQL
		// before any request traffic arrives; open concurrency remains separately
		// bounded for active request bursts.
		db.SetConnMaxIdleTime(2 * time.Minute)
		db.SetConnMaxLifetime(10 * time.Minute)
	default:
	}
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
	if driver == "postgres" {
		return createPostgresDatabase(input, cfg)
	}
	// LibSQL creates the remote database through its provider — no CREATE DATABASE needed.
	if driver == "libsql" {
		return nil
	}
	if driver != "mysql" {
		return fmt.Errorf("unsupported database type %q", driver)
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

func createPostgresDatabase(input CreateSiteInput, cfg *SiteConfig) error {
	databaseName := cfg.DBName
	if strings.TrimSpace(databaseName) == "" || len(databaseName) > 63 {
		return fmt.Errorf("invalid PostgreSQL database name length")
	}
	adminDB := input.PlatformDB
	if adminDB != nil && input.PlatformDBType != "" && !strings.EqualFold(input.PlatformDBType, "postgres") {
		// The platform registry and a tenant may live on different database
		// engines; never issue PostgreSQL admin SQL through a MySQL/LibSQL handle.
		adminDB = nil
	}
	owned := false
	if adminDB == nil {
		dsn := ""
		if strings.EqualFold(input.PlatformDBType, "postgres") || input.PlatformDBType == "" {
			dsn = input.PlatformDBDSN
		}
		if dsn == "" {
			adminConfig := *cfg
			adminConfig.DBName = "postgres"
			dsn = adminConfig.DSN()
		}
		var err error
		adminDB, err = sql.Open("postgres", dsn)
		if err != nil {
			return fmt.Errorf("connecting to PostgreSQL admin database: %w", err)
		}
		owned = true
	}
	if owned {
		defer adminDB.Close()
	}
	if err := adminDB.Ping(); err != nil {
		return fmt.Errorf("pinging PostgreSQL admin database: %w", err)
	}
	var exists bool
	if err := adminDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, databaseName).Scan(&exists); err != nil {
		return fmt.Errorf("checking PostgreSQL database %s: %w", databaseName, err)
	}
	if exists {
		return nil
	}
	quotedName := `"` + strings.ReplaceAll(databaseName, `"`, `""`) + `"`
	if _, err := adminDB.Exec(`CREATE DATABASE ` + quotedName); err != nil {
		// Concurrent idempotent provisioning can race between the existence
		// check and CREATE DATABASE. Accept only if the target now exists.
		if checkErr := adminDB.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, databaseName).Scan(&exists); checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("creating PostgreSQL database %s: %w", databaseName, err)
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

func postgresAdminDSN(tenantDSN string, host string, port int, user, password string) (string, error) {
	if tenantDSN != "" {
		parsed, err := url.Parse(tenantDSN)
		if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" {
			return "", fmt.Errorf("invalid PostgreSQL tenant DSN")
		}
		parsed.Path = "/postgres"
		parsed.RawPath = ""
		return parsed.String(), nil
	}
	if host == "" || user == "" {
		return "", fmt.Errorf("PostgreSQL host and user are required")
	}
	if port == 0 {
		port = 5432
	}
	return (&SiteConfig{DBType: "postgres", DBHost: host, DBPort: port, DBName: "postgres", DBUser: user, DBPassword: password}).DSN(), nil
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
	SiteID         string
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
		if err := removePlatformSiteRegistrationByID(input.PlatformDB, input.PlatformDBType, input.SiteID, input.Hostname); err != nil {
			return fmt.Errorf("removing platform site registration: %w", err)
		}
		slog.Info("site database dropped", "hostname", input.Hostname, "db_name", input.DBName)
		return nil

	case "postgres", "postgresql":
		if input.DB != nil {
			if err := input.DB.Close(); err != nil {
				return fmt.Errorf("closing site database before drop: %w", err)
			}
		}
		adminDB := input.PlatformDB
		owned := false
		if adminDB == nil || !strings.EqualFold(input.PlatformDBType, "postgres") {
			dsn, err := postgresAdminDSN(input.DBDSN, input.DBHost, input.DBPort, input.DBUser, input.DBPassword)
			if err != nil {
				return fmt.Errorf("building PostgreSQL admin DSN: %w", err)
			}
			adminDB, err = sql.Open("postgres", dsn)
			if err != nil {
				return fmt.Errorf("connecting for PostgreSQL teardown: %w", err)
			}
			owned = true
		}
		if owned {
			defer adminDB.Close()
		}
		var currentDatabase string
		if err := adminDB.QueryRow(`SELECT current_database()`).Scan(&currentDatabase); err != nil {
			return fmt.Errorf("checking PostgreSQL teardown connection: %w", err)
		}
		if currentDatabase == input.DBName {
			return fmt.Errorf("refusing to drop PostgreSQL database %s using a connection to that same database", input.DBName)
		}
		if _, err := adminDB.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, input.DBName); err != nil {
			return fmt.Errorf("terminating sessions for database %s: %w", input.DBName, err)
		}
		if _, err := adminDB.Exec(fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, input.Dialect.QuoteIdent(input.DBName))); err != nil {
			return fmt.Errorf("dropping database %s: %w", input.DBName, err)
		}
		if err := removePlatformSiteRegistrationByID(input.PlatformDB, input.PlatformDBType, input.SiteID, input.Hostname); err != nil {
			return fmt.Errorf("removing platform site registration: %w", err)
		}
		slog.Info("site database dropped", "hostname", input.Hostname, "db_name", input.DBName, "db_type", "postgres")
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
		if err := removePlatformSiteRegistrationByID(input.PlatformDB, input.PlatformDBType, input.SiteID, input.Hostname); err != nil {
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
