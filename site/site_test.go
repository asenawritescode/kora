package site

import (
	"os"
	"testing"
)

func TestDSN_MySQL(t *testing.T) {
	cfg := &SiteConfig{
		DBType:     "mysql",
		DBHost:     "db.example.com",
		DBPort:     3306,
		DBName:     "test_site",
		DBUser:     "user",
		DBPassword: "pass",
	}
	dsn := cfg.DSN()
	expected := "user:pass@tcp(db.example.com:3306)/test_site?parseTime=true&charset=utf8mb4&collation=utf8mb4_unicode_ci"
	if dsn != expected {
		t.Errorf("DSN() = %q, want %q", dsn, expected)
	}
}

func TestDSN_LibSQL(t *testing.T) {
	tests := []struct {
		name string
		cfg  *SiteConfig
		want string
	}{
		{
			name: "remote url",
			cfg: &SiteConfig{
				DBType: "libsql",
				DBHost: "libsql://test-db.turso.io",
			},
			want: "libsql://test-db.turso.io",
		},
		{
			name: "http with credentials",
			cfg: &SiteConfig{
				DBType:     "libsql",
				DBHost:     "https://db.turso.io",
				DBUser:     "token",
				DBPassword: "secret",
			},
			want: "http://token:secret@db.turso.io",
		},
		{
			name: "no prefix",
			cfg: &SiteConfig{
				DBType: "libsql",
				DBHost: "some-host",
			},
			want: "some-host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.DSN()
			if got != tt.want {
				t.Errorf("DSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReconstructSiteConfig_DBName(t *testing.T) {
	common := &CommonConfig{
		DBType:     "mysql",
		DBHost:     "127.0.0.1",
		DBPort:     3306,
		DBUser:     "root",
		DBPassword: "",
	}

	cfg := ReconstructSiteConfig("airtime.local", common, nil)
	expectedDBName := "airtime_local"
	if cfg.DBName != expectedDBName {
		t.Errorf("DBName = %q, want %q", cfg.DBName, expectedDBName)
	}
	if cfg.Hostname != "airtime.local" {
		t.Errorf("Hostname = %q, want %q", cfg.Hostname, "airtime.local")
	}
	if len(cfg.Apps) == 0 || cfg.Apps[0] != "core" {
		t.Error("should have 'core' app by default")
	}
}

func TestReconstructSiteConfig_Domains(t *testing.T) {
	common := &CommonConfig{
		DBType: "mysql",
		DBHost: "127.0.0.1",
		DBPort: 3306,
	}

	// Test with provided domains.
	providedDomains := []string{"app.example.com", "app-admin.example.com"}
	cfg := ReconstructSiteConfig("app.example.com", common, providedDomains)
	if len(cfg.DomainsList) != 2 {
		t.Fatalf("DomainsList length = %d, want 2", len(cfg.DomainsList))
	}
	if cfg.DomainsList[0] != "app.example.com" {
		t.Errorf("DomainsList[0] = %q, want %q", cfg.DomainsList[0], "app.example.com")
	}

	// Test fallback to [hostname].
	cfg2 := ReconstructSiteConfig("single.example.com", common, nil)
	domains := cfg2.Domains()
	if len(domains) != 1 {
		t.Fatalf("Domains length = %d, want 1", len(domains))
	}
	if domains[0] != "single.example.com" {
		t.Errorf("Domains[0] = %q, want %q", domains[0], "single.example.com")
	}
}

func TestSiteConfig_Domains(t *testing.T) {
	tests := []struct {
		name string
		cfg  *SiteConfig
		want []string
	}{
		{
			name: "uses domains list when set",
			cfg:  &SiteConfig{Hostname: "example.com", DomainsList: []string{"a.com", "b.com"}},
			want: []string{"a.com", "b.com"},
		},
		{
			name: "falls back to hostname",
			cfg:  &SiteConfig{Hostname: "example.com"},
			want: []string{"example.com"},
		},
		{
			name: "falls back to localhost",
			cfg:  &SiteConfig{},
			want: []string{"localhost"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Domains()
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Domains[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCommonConfigFromEnv_Defaults(t *testing.T) {
	// Clear relevant env vars.
	for _, key := range []string{
		"KORA_DB_TYPE", "KORA_DB_HOST", "KORA_DB_PORT",
		"KORA_HTTP_PORT", "KORA_LOG_LEVEL", "KORA_LOG_FORMAT",
		"KORA_APP_NAME", "KORA_VERSION", "KORA_ADMIN_ROLE",
	} {
		os.Unsetenv(key)
	}

	cfg := CommonConfigFromEnv()
	if cfg.DBType != "mysql" {
		t.Errorf("DBType = %q, want %q", cfg.DBType, "mysql")
	}
	if cfg.DBHost != "127.0.0.1" {
		t.Errorf("DBHost = %q, want %q", cfg.DBHost, "127.0.0.1")
	}
	if cfg.HTTPPort != 8000 {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, 8000)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
	if cfg.AppName != "Kora" {
		t.Errorf("AppName = %q, want %q", cfg.AppName, "Kora")
	}
	if cfg.Version != "0.3.0" {
		t.Errorf("Version = %q, want %q", cfg.Version, "0.3.0")
	}
	if cfg.AdminRole != "Administrator" {
		t.Errorf("AdminRole = %q, want %q", cfg.AdminRole, "Administrator")
	}
}

func TestCommonConfigFromEnv_Overrides(t *testing.T) {
	// Set env vars.
	os.Setenv("KORA_DB_TYPE", "libsql")
	os.Setenv("KORA_DB_HOST", "db.remote.turso.io")
	os.Setenv("KORA_DB_PORT", "8080")
	os.Setenv("KORA_HTTP_PORT", "9000")
	os.Setenv("KORA_LOG_LEVEL", "debug")
	os.Setenv("KORA_APP_NAME", "MyApp")
	os.Setenv("KORA_ADMIN_ROLE", "SuperAdmin")
	os.Setenv("KORA_VERSION", "1.0.0")

	defer func() {
		os.Unsetenv("KORA_DB_TYPE")
		os.Unsetenv("KORA_DB_HOST")
		os.Unsetenv("KORA_DB_PORT")
		os.Unsetenv("KORA_HTTP_PORT")
		os.Unsetenv("KORA_LOG_LEVEL")
		os.Unsetenv("KORA_APP_NAME")
		os.Unsetenv("KORA_ADMIN_ROLE")
		os.Unsetenv("KORA_VERSION")
	}()

	cfg := CommonConfigFromEnv()
	if cfg.DBType != "libsql" {
		t.Errorf("DBType = %q, want %q", cfg.DBType, "libsql")
	}
	if cfg.DBHost != "db.remote.turso.io" {
		t.Errorf("DBHost = %q, want %q", cfg.DBHost, "db.remote.turso.io")
	}
	if cfg.HTTPPort != 9000 {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, 9000)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.AppName != "MyApp" {
		t.Errorf("AppName = %q, want %q", cfg.AppName, "MyApp")
	}
	if cfg.AdminRole != "SuperAdmin" {
		t.Errorf("AdminRole = %q, want %q", cfg.AdminRole, "SuperAdmin")
	}
	if cfg.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", cfg.Version, "1.0.0")
	}
}

func TestStartupConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *StartupConfig
		wantErr bool
	}{
		{
			name: "no dsn — valid",
			cfg: &StartupConfig{
				DBDSN:  "",
				DBType: "",
			},
			wantErr: false,
		},
		{
			name: "dsn with mysql type — valid",
			cfg: &StartupConfig{
				DBDSN:  "user:pass@tcp(localhost)/db",
				DBType: "mysql",
			},
			wantErr: false,
		},
		{
			name: "dsn with libsql type — valid",
			cfg: &StartupConfig{
				DBDSN:  "libsql://db.turso.io",
				DBType: "libsql",
			},
			wantErr: false,
		},
		{
			name: "dsn without type — error",
			cfg: &StartupConfig{
				DBDSN:  "user:pass@tcp(localhost)/db",
				DBType: "",
			},
			wantErr: true,
		},
		{
			name: "dsn with invalid type — error",
			cfg: &StartupConfig{
				DBDSN:  "user:pass@tcp(localhost)/db",
				DBType: "postgres",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	os.Setenv("KORA_DB_HOST", "env-host.example.com")
	os.Setenv("KORA_DB_USER", "envuser")
	os.Setenv("KORA_DB_PASSWORD", "envpass")
	os.Setenv("KORA_HTTP_PORT", "9999")
	os.Setenv("KORA_SESSION_HOURS", "48")

	defer func() {
		os.Unsetenv("KORA_DB_HOST")
		os.Unsetenv("KORA_DB_USER")
		os.Unsetenv("KORA_DB_PASSWORD")
		os.Unsetenv("KORA_HTTP_PORT")
		os.Unsetenv("KORA_SESSION_HOURS")
	}()

	cfg := &CommonConfig{
		DBHost:               "default-host",
		DBUser:               "defaultuser",
		DBPassword:           "defaultpass",
		HTTPPort:             8000,
		SessionLifetimeHours: 24,
	}

	cfg.ApplyEnvOverrides()

	if cfg.DBHost != "env-host.example.com" {
		t.Errorf("DBHost = %q, want %q", cfg.DBHost, "env-host.example.com")
	}
	if cfg.DBUser != "envuser" {
		t.Errorf("DBUser = %q, want %q", cfg.DBUser, "envuser")
	}
	if cfg.DBPassword != "envpass" {
		t.Errorf("DBPassword = %q, want %q", cfg.DBPassword, "envpass")
	}
	if cfg.HTTPPort != 9999 {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, 9999)
	}
	if cfg.SessionLifetimeHours != 48 {
		t.Errorf("SessionLifetimeHours = %d, want %d", cfg.SessionLifetimeHours, 48)
	}
}

func TestLoadStartupConfig_Defaults(t *testing.T) {
	for _, key := range []string{
		"DB_DSN", "KORA_DB_TYPE", "CONSOLE_EMAIL", "CONSOLE_PASSWORD",
		"KORA_HTTP_PORT", "KORA_CONFIG_DIR", "KORA_LOG_LEVEL", "KORA_LOG_FORMAT",
	} {
		os.Unsetenv(key)
	}

	cfg := LoadStartupConfig()
	if cfg.DBType != "" {
		t.Errorf("DBType = %q, want empty", cfg.DBType)
	}
	if cfg.DBDSN != "" {
		t.Errorf("DBDSN = %q, want empty", cfg.DBDSN)
	}
	if cfg.ConsoleEmail != "admin@kora.local" {
		t.Errorf("ConsoleEmail = %q, want %q", cfg.ConsoleEmail, "admin@kora.local")
	}
	if cfg.ConsolePassword != "kora123" {
		t.Errorf("ConsolePassword = %q, want %q", cfg.ConsolePassword, "kora123")
	}
	if cfg.HTTPPort != 8000 {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, 8000)
	}
	if cfg.ConfigDir != "." {
		t.Errorf("ConfigDir = %q, want %q", cfg.ConfigDir, ".")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "json")
	}
}

func TestLoadStartupConfig_EnvOverrides(t *testing.T) {
	os.Setenv("DB_DSN", "user:pass@tcp(localhost:3306)/test")
	os.Setenv("KORA_DB_TYPE", "mysql")
	os.Setenv("CONSOLE_EMAIL", "super@admin.com")
	os.Setenv("CONSOLE_PASSWORD", "supersecret")
	os.Setenv("KORA_HTTP_PORT", "9999")
	os.Setenv("KORA_CONFIG_DIR", "/etc/kora")
	os.Setenv("KORA_LOG_LEVEL", "debug")
	os.Setenv("KORA_LOG_FORMAT", "text")

	defer func() {
		os.Unsetenv("DB_DSN")
		os.Unsetenv("KORA_DB_TYPE")
		os.Unsetenv("CONSOLE_EMAIL")
		os.Unsetenv("CONSOLE_PASSWORD")
		os.Unsetenv("KORA_HTTP_PORT")
		os.Unsetenv("KORA_CONFIG_DIR")
		os.Unsetenv("KORA_LOG_LEVEL")
		os.Unsetenv("KORA_LOG_FORMAT")
	}()

	cfg := LoadStartupConfig()
	if cfg.DBType != "mysql" {
		t.Errorf("DBType = %q, want %q", cfg.DBType, "mysql")
	}
	if cfg.DBDSN != "user:pass@tcp(localhost:3306)/test" {
		t.Errorf("DBDSN = %q, want %q", cfg.DBDSN, "user:pass@tcp(localhost:3306)/test")
	}
	if cfg.ConsoleEmail != "super@admin.com" {
		t.Errorf("ConsoleEmail = %q, want %q", cfg.ConsoleEmail, "super@admin.com")
	}
	if cfg.ConsolePassword != "supersecret" {
		t.Errorf("ConsolePassword = %q, want %q", cfg.ConsolePassword, "supersecret")
	}
	if cfg.HTTPPort != 9999 {
		t.Errorf("HTTPPort = %d, want %d", cfg.HTTPPort, 9999)
	}
	if cfg.ConfigDir != "/etc/kora" {
		t.Errorf("ConfigDir = %q, want %q", cfg.ConfigDir, "/etc/kora")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, "text")
	}
}
