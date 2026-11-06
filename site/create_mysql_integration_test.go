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

	"github.com/asenawritescode/kora/db"
	"github.com/go-sql-driver/mysql"
)

func TestLiveMySQLCreateSiteRegistersCanonicalDirectoryAndRetries(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	host, port := mysqlHostPort(parsed.Addr)
	if host == "" || port == 0 {
		t.Fatalf("MySQL DSN must include a host and port; got %q", parsed.Addr)
	}
	platformDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL platform database:", err)
	}
	defer platformDB.Close()
	if err := platformDB.PingContext(t.Context()); err != nil {
		t.Fatal("ping disposable MySQL platform database:", err)
	}
	if err := BootstrapPlatformRegistryContext(t.Context(), platformDB, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap MySQL platform registry:", err)
	}

	stamp := time.Now().UnixNano()
	hostname := fmt.Sprintf("create-site-%d.example.invalid", stamp)
	databaseName := fmt.Sprintf("kora_create_%d", stamp)
	tenantID := fmt.Sprintf("tenant-mysql-%d", stamp)
	var result *CreateSiteResult
	t.Cleanup(func() {
		if result != nil && result.DB != nil {
			_ = result.DB.Close()
		}
		_ = removePlatformSiteRegistration(platformDB, "mysql", hostname)
		_, _ = platformDB.Exec("DROP DATABASE IF EXISTS `" + databaseName + "`")
	})
	input := CreateSiteInput{
		Hostname: hostname, SiteID: tenantID, DBType: "mysql", DBHost: host, DBPort: port,
		DBName: databaseName, DBUser: parsed.User, DBPassword: parsed.Passwd,
		RuntimeCellID: "deployment-test",
		AdminEmail:    "owner@" + hostname, CloudSubjectID: "cloud-user-" + tenantID, PlatformDBType: "mysql", PlatformDB: platformDB,
		ExtraDomains: []string{"workspace." + hostname},
	}
	result, err = CreateSiteContext(t.Context(), input)
	if err != nil {
		t.Fatal("create MySQL tenant:", err)
	}
	if result.SiteID != tenantID || result.Config == nil || result.Config.DBName != databaseName {
		t.Fatalf("created site result = %#v, want canonical ID %q and database %q", result, tenantID, databaseName)
	}
	descriptor, err := NewSQLSiteRegistry(platformDB, "mysql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-test" {
		t.Fatalf("created site's deployment assignment = %q, %v; want deployment-test", descriptor.RuntimeCellID, err)
	}
	if got, err := ResolveSiteAliasContext(t.Context(), platformDB, "mysql", "workspace."+hostname); err != nil || got != tenantID {
		t.Fatalf("additional alias resolves to %q, %v; want %q", got, err, tenantID)
	}
	var admins, activeVersions int
	if err := result.DB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM _kora_user WHERE email = ?", "owner@"+hostname).Scan(&admins); err != nil {
		t.Fatal("read created tenant admin:", err)
	}
	if err := result.DB.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND is_active = 1", hostname).Scan(&activeVersions); err != nil {
		t.Fatal("read active tenant config version:", err)
	}
	if admins != 1 || activeVersions != 1 {
		t.Fatalf("created tenant has %d admins and %d active config versions, want one each", admins, activeVersions)
	}
	if err := result.DB.Close(); err != nil {
		t.Fatal("close first tenant DB handle before retry:", err)
	}
	result.DB = nil
	input.RuntimeCellID = "deployment-other"
	result, err = CreateSiteContext(context.Background(), input)
	if err != nil {
		t.Fatal("retry MySQL tenant creation:", err)
	}
	if result.SiteID != tenantID {
		t.Fatalf("retry changed canonical tenant ID to %q", result.SiteID)
	}
	descriptor, err = NewSQLSiteRegistry(platformDB, "mysql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-test" {
		t.Fatalf("retry changed active deployment assignment to %q, %v; want deployment-test", descriptor.RuntimeCellID, err)
	}
	if err := result.DB.Close(); err != nil {
		t.Fatal("close retried tenant DB handle:", err)
	}
	result.DB = nil
}

func TestLiveMySQLProvisioningRegistrationBackfillsOnlyUnassignedRows(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	platformDB, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open disposable MySQL platform database:", err)
	}
	defer platformDB.Close()
	if err := platformDB.PingContext(t.Context()); err != nil {
		t.Fatal("ping disposable MySQL platform database:", err)
	}
	if err := BootstrapPlatformRegistryContext(t.Context(), platformDB, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap MySQL platform registry:", err)
	}

	stamp := time.Now().UnixNano()
	hostname := fmt.Sprintf("provisioning-assignment-%d.example.invalid", stamp)
	tenantID := fmt.Sprintf("tenant-provisioning-assignment-%d", stamp)
	t.Cleanup(func() { _ = removePlatformSiteRegistration(platformDB, "mysql", hostname) })
	config := &SiteConfig{Hostname: hostname, DBType: "mysql", DBHost: "127.0.0.1", DBPort: 3306, DBName: "tenant_db", DBUser: parsed.User, DomainsList: []string{hostname}}
	register := func(status, assignment string) error {
		return ensurePlatformSiteRegistrationAssignmentContext(t.Context(), platformDB, "mysql", config, status, assignment, tenantID)
	}
	if err := register("provisioning", ""); err != nil {
		t.Fatal("create legacy provisioning row:", err)
	}
	if err := register("provisioning", "deployment-recovery"); err != nil {
		t.Fatal("recover provisioning assignment:", err)
	}
	descriptor, err := NewSQLSiteRegistry(platformDB, "mysql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-recovery" {
		t.Fatalf("recovered assignment = %q, %v; want deployment-recovery", descriptor.RuntimeCellID, err)
	}
	if err := register("active", "deployment-other"); err != nil {
		t.Fatal("activate existing assignment row:", err)
	}
	descriptor, err = NewSQLSiteRegistry(platformDB, "mysql").GetDescriptorByIDContext(t.Context(), tenantID)
	if err != nil || descriptor.RuntimeCellID != "deployment-recovery" || descriptor.Status != "active" {
		t.Fatalf("active assignment changed = %q status=%q err=%v; want deployment-recovery/active", descriptor.RuntimeCellID, descriptor.Status, err)
	}
}
