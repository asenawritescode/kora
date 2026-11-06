//go:build integration

package site

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/go-sql-driver/mysql"
)

// TestLiveCreateSiteSeedsDisplayName proves the provisioning display name is
// persisted as the tenant's branding.app_name, that legacy callers without one
// keep the hostname-derived label, and that a provisioning retry never
// overwrites a name the tenant administrator has since changed.
func TestLiveCreateSiteSeedsDisplayName(t *testing.T) {
	for _, dialect := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN"},
	} {
		t.Run(dialect.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(dialect.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", dialect.env, dialect.driver)
			}
			for _, test := range []struct {
				name, displayName, wantSeed string
				edit                        string
			}{
				{name: "typed business name", displayName: "Dbg Duka", wantSeed: "Dbg Duka"},
				{name: "legacy caller without a name", displayName: "", wantSeed: ""},
				{name: "retry keeps administrator edit", displayName: "Mama Njeri Duka", wantSeed: "Mama Njeri Duka", edit: "Mama Njeri Supermarket"},
			} {
				t.Run(test.name, func(t *testing.T) {
					testLiveCreateSiteDisplayName(t, dialect.driver, dsn, test.displayName, test.wantSeed, test.edit)
				})
			}
		})
	}
}

func testLiveCreateSiteDisplayName(t *testing.T, driver, dsn, displayName, wantSeed, edit string) {
	t.Helper()
	host, port, user, password, err := liveCreateSiteTarget(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	platformDB, err := sql.Open(driver, dsn)
	if err != nil {
		t.Fatal("open disposable platform database:", err)
	}
	// Registered first so it closes after the cleanup below has dropped the
	// tenant database (a deferred Close would run before t.Cleanup).
	t.Cleanup(func() { _ = platformDB.Close() })
	dialect := db.Resolve(driver)
	if err := BootstrapPlatformRegistryContext(t.Context(), platformDB, dialect); err != nil {
		t.Fatal("bootstrap platform registry:", err)
	}
	stamp := time.Now().UnixNano()
	hostname := fmt.Sprintf("dbg-duka-%d.example.invalid", stamp)
	databaseName := fmt.Sprintf("kora_display_%d", stamp)
	tenantID := fmt.Sprintf("tenant-display-%d", stamp)
	var result *CreateSiteResult
	t.Cleanup(func() {
		if result != nil && result.DB != nil {
			_ = result.DB.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = removePlatformSiteRegistration(platformDB, driver, hostname)
		quoted := "`" + databaseName + "`"
		if driver == "postgres" {
			quoted = `"` + databaseName + `" WITH (FORCE)`
		}
		if _, err := platformDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
			t.Logf("drop disposable tenant database %s: %v", databaseName, err)
		}
	})
	input := CreateSiteInput{
		Hostname: hostname, SiteID: tenantID, DBType: driver, DBHost: host, DBPort: port,
		DBName: databaseName, DBUser: user, DBPassword: password, DisplayName: displayName,
		AdminEmail: "owner@" + hostname, CloudSubjectID: "cloud-user-" + tenantID, PlatformDBType: driver, PlatformDB: platformDB,
	}
	result, err = CreateSiteContext(t.Context(), input)
	if err != nil {
		t.Fatal("create tenant:", err)
	}
	readName := func() string {
		t.Helper()
		var value string
		err := result.DB.QueryRowContext(t.Context(), db.Rebind(dialect, "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"), hostname, "branding.app_name").Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return ""
		}
		if err != nil {
			t.Fatal("read seeded display name:", err)
		}
		return value
	}
	if got := readName(); got != wantSeed {
		t.Fatalf("seeded branding.app_name = %q, want %q", got, wantSeed)
	}
	if edit == "" {
		return
	}
	if _, err := result.DB.ExecContext(t.Context(), db.Rebind(dialect, "UPDATE _kora_site_setting SET setting_value = ? WHERE site = ? AND setting_key = ?"), edit, hostname, "branding.app_name"); err != nil {
		t.Fatal("simulate administrator rename:", err)
	}
	if err := result.DB.Close(); err != nil {
		t.Fatal("close tenant before retry:", err)
	}
	result.DB = nil
	result, err = CreateSiteContext(t.Context(), input)
	if err != nil {
		t.Fatal("retry tenant creation:", err)
	}
	if got := readName(); got != edit {
		t.Fatalf("retry rewrote branding.app_name to %q, want administrator value %q", got, edit)
	}
}

func liveCreateSiteTarget(driver, dsn string) (string, int, string, string, error) {
	if driver == "mysql" {
		parsed, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", 0, "", "", fmt.Errorf("parse MySQL DSN: %w", err)
		}
		host, port := mysqlHostPort(parsed.Addr)
		if host == "" || port == 0 {
			return "", 0, "", "", fmt.Errorf("MySQL DSN must include host and port")
		}
		return host, port, parsed.User, parsed.Passwd, nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", 0, "", "", fmt.Errorf("PostgreSQL acceptance requires a URL DSN")
	}
	port := 5432
	if parsed.Port() != "" {
		if port, err = strconv.Atoi(parsed.Port()); err != nil {
			return "", 0, "", "", fmt.Errorf("parse PostgreSQL port: %w", err)
		}
	}
	password, _ := parsed.User.Password()
	return parsed.Hostname(), port, parsed.User.Username(), password, nil
}
