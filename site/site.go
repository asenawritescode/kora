// Package site manages site configuration and multi-tenancy.
package site

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SiteConfig holds the configuration for a single site/tenant.
type SiteConfig struct {
	// Database connection settings.
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
	RedisURL  string `yaml:"redis_url"`
	DBHost    string `yaml:"db_host"`
	HTTPPort  int    `yaml:"http_port"`
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

// DSN returns the MySQL DSN for this site.
func (s *SiteConfig) DSN() string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci",
		s.DBUser, s.DBPassword, s.DBHost, s.DBPort, s.DBName)
}

// LoadCommonConfig reads the common site config from a YAML file.
func LoadCommonConfig(path string) (*CommonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading common config: %w", err)
	}
	cfg := &CommonConfig{
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
	return cfg, nil
}

// LoadSiteConfig reads a site configuration from a YAML file.
func LoadSiteConfig(path string) (*SiteConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading site config: %w", err)
	}
	cfg := &SiteConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing site config: %w", err)
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
	db, err := sql.Open("mysql", dsn)
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
