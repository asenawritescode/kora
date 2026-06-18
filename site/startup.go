package site

import (
	"fmt"
	"os"
	"strconv"
)

// StartupConfig holds all configuration loaded from env vars at boot.
// Replaces scattered os.Getenv calls across multiple files.
type StartupConfig struct {
	// Required when DB_DSN is set.
	DBType string // KORA_DB_TYPE — "mysql" or "libsql"
	DBDSN  string // DB_DSN — full connection string

	// Console (super-admin).
	ConsoleEmail    string // CONSOLE_EMAIL — defaults to admin@kora.local
	ConsolePassword string // CONSOLE_PASSWORD — defaults to kora123

	// Server.
	HTTPPort  int    // KORA_HTTP_PORT — defaults to 8000
	ConfigDir string // KORA_CONFIG_DIR — defaults to "."
	LogLevel  string // KORA_LOG_LEVEL — defaults to "info"
	LogFormat string // KORA_LOG_FORMAT — defaults to "json"
}

// LoadStartupConfig reads all config from environment variables.
func LoadStartupConfig() *StartupConfig {
	c := &StartupConfig{
		DBType:          os.Getenv("KORA_DB_TYPE"),
		DBDSN:           os.Getenv("DB_DSN"),
		ConsoleEmail:    envOrDefault("CONSOLE_EMAIL", "admin@kora.local"),
		ConsolePassword: envOrDefault("CONSOLE_PASSWORD", "kora123"),
		HTTPPort:        envIntOrDefault("KORA_HTTP_PORT", 8000),
		ConfigDir:       envOrDefault("KORA_CONFIG_DIR", "."),
		LogLevel:        envOrDefault("KORA_LOG_LEVEL", "info"),
		LogFormat:       envOrDefault("KORA_LOG_FORMAT", "json"),
	}
	return c
}

// Validate checks required fields and returns all errors at once.
// If DB_DSN is set, KORA_DB_TYPE must be one of "mysql" or "libsql".
func (c *StartupConfig) Validate() error {
	var errs []string

	if c.DBDSN != "" {
		if c.DBType == "" {
			errs = append(errs, "KORA_DB_TYPE is required when DB_DSN is set (must be 'mysql' or 'libsql')")
		} else if c.DBType != "mysql" && c.DBType != "libsql" {
			errs = append(errs, fmt.Sprintf("KORA_DB_TYPE must be 'mysql' or 'libsql', got '%s'", c.DBType))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("configuration errors:\n  - %s", joinErr(errs, "\n  - "))
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func joinErr(errs []string, sep string) string {
	r := ""
	for i, e := range errs {
		if i > 0 {
			r += sep
		}
		r += e
	}
	return r
}
