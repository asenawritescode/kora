// Package site manages site configuration and multi-tenancy.
package site

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
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
	DBUser     string `yaml:"db_user"`
	DBPassword string `yaml:"db_password"`
	HTTPPort   int    `yaml:"http_port"`
	Workers   int    `yaml:"workers"`
	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`

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

// DiscoverSites finds all site directories under the given base path.
// Each subdirectory containing a site_config.yaml is considered a site.
func DiscoverSites(basePath string) ([]string, error) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading sites directory: %w", err)
	}

	var sites []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		configPath := filepath.Join(basePath, entry.Name(), "site_config.yaml")
		if _, err := os.Stat(configPath); err == nil {
			sites = append(sites, entry.Name())
		}
	}
	return sites, nil
}

// Connect opens a database connection for the site.
func Connect(cfg *SiteConfig) (*sql.DB, error) {
	dsn := cfg.DSN()
	// DB_DSN env var overrides YAML-based config — used in container deployments.
	if envDSN := os.Getenv("DB_DSN"); envDSN != "" {
		dsn = envDSN
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
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
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
func CreateDatabase(cfg *SiteConfig) error {
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

	// Connect to MySQL without specifying a database.
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?parseTime=true&charset=utf8mb4",
		cfg.DBUser, cfg.DBPassword, cfg.DBHost, cfg.DBPort)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connecting to MySQL server: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("pinging MySQL server: %w", err)
	}

	_, err = db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci", cfg.DBName))
	if err != nil {
		return fmt.Errorf("creating database %s: %w", cfg.DBName, err)
	}
	return nil
}

// Close closes the site's database connection.
func (s *Site) Close() error {
	if s.DB != nil {
		return s.DB.Close()
	}
	return nil
}
