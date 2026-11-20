//go:build integration

package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
)

// TestLiveCreateSiteStoresAdminFullName proves the administrator full name
// passed to CreateSite is the name on the tenant's bootstrap user row.
func TestLiveCreateSiteStoresAdminFullName(t *testing.T) {
	for _, dialect := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DIRECTORY_LIVE_POSTGRES_DSN"},
	} {
		t.Run(dialect.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(dialect.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", dialect.env, dialect.driver)
			}
			host, port, user, password, err := liveCreateSiteTarget(dialect.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			platformDB, err := sql.Open(dialect.driver, dsn)
			if err != nil {
				t.Fatal("open disposable platform database:", err)
			}
			t.Cleanup(func() { _ = platformDB.Close() })
			dbDialect := db.Resolve(dialect.driver)
			if err = BootstrapPlatformRegistry(t.Context(), platformDB, dbDialect); err != nil {
				t.Fatal("bootstrap platform registry:", err)
			}
			stamp := time.Now().UnixNano()
			hostname := fmt.Sprintf("owner-name-%d.example.invalid", stamp)
			databaseName := fmt.Sprintf("kora_owner_name_%d", stamp)
			const ownerName = "Retail Owner"
			ownerEmail := "owner@" + hostname
			ownerSubject := fmt.Sprintf("cloud-user-%d", stamp)
			var result *CreateSiteResult
			t.Cleanup(func() {
				if result != nil && result.DB != nil {
					_ = result.DB.Close()
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = removePlatformSiteRegistration(platformDB, dialect.driver, hostname)
				quoted := "`" + databaseName + "`"
				if dialect.driver == "postgres" {
					quoted = `"` + databaseName + `" WITH (FORCE)`
				}
				if _, err := platformDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
					t.Logf("drop disposable tenant database %s: %v", databaseName, err)
				}
			})
			result, err = CreateSite(t.Context(), CreateSiteInput{
				Hostname: hostname, SiteID: "tenant-owner-name-" + fmt.Sprint(stamp), DBType: dialect.driver, DBHost: host, DBPort: port,
				DBName: databaseName, DBUser: user, DBPassword: password, AdminEmail: ownerEmail, AdminFullName: ownerName,
				CloudSubjectID: ownerSubject, PlatformDBType: dialect.driver, PlatformDB: platformDB,
			})
			if err != nil {
				t.Fatal("create tenant:", err)
			}
			var got string
			query := db.Rebind(dbDialect, "SELECT full_name FROM _kora_user WHERE name = ? AND site = ?")
			if err := result.DB.QueryRowContext(t.Context(), query, ownerSubject, hostname).Scan(&got); err != nil {
				t.Fatal("read bootstrap user:", err)
			}
			if got != ownerName {
				t.Fatalf("bootstrap user full_name = %q, want %q", got, ownerName)
			}
		})
	}
}
