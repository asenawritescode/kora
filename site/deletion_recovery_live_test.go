package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// TestLiveMySQLDeleteRegisteredSite verifies deletion intent, canonical-ID
// removal, and restart-safe cleanup using only task-created disposable schemas.
func TestLiveMySQLDeleteRegisteredSiteUsesCanonicalIdentity(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse live MySQL DSN")
	}
	cfg.DBName = "mysql"
	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open live MySQL admin connection")
	}
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping live MySQL admin connection")
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	platformSchema := "kora_delete_platform_" + suffix
	tenantSchema := "kora_delete_tenant_" + suffix
	for _, schema := range []string{platformSchema, tenantSchema} {
		if _, err := adminDB.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatalf("create disposable schema %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		for _, schema := range []string{tenantSchema, platformSchema} {
			if _, err := adminDB.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
				t.Errorf("drop disposable schema %s: %v", schema, err)
			}
		}
		if err := adminDB.Close(); err != nil {
			t.Errorf("close live MySQL admin connection: %v", err)
		}
	})

	cfg.DBName = platformSchema
	platformDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open disposable platform registry")
	}
	defer platformDB.Close()
	if err := BootstrapPlatformRegistry(platformDB, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap disposable platform registry:", err)
	}
	hostname := "canonical-delete-" + suffix + ".example.invalid"
	registry := NewSQLSiteRegistry(platformDB, "mysql")
	if err := registry.Upsert(&SiteConfig{Hostname: hostname, DBType: "mysql", DBName: tenantSchema}); err != nil {
		t.Fatal("register disposable site:", err)
	}
	descriptor, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable site's canonical identity:", err)
	}
	dbHost, dbPortText, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		t.Fatal("parse MySQL host and port")
	}
	dbPort, err := strconv.Atoi(dbPortText)
	if err != nil {
		t.Fatal("parse MySQL port")
	}
	common := &CommonConfig{DBType: "mysql", DBHost: dbHost, DBPort: dbPort, DBUser: cfg.User, DBPassword: cfg.Passwd}
	if err := DeleteRegisteredSite(context.Background(), platformDB, "mysql", descriptor.SiteID, hostname, common); err != nil {
		t.Fatal("delete registered site by canonical ID:", err)
	}
	if _, err := registry.GetByID(descriptor.SiteID); err == nil {
		t.Fatal("canonical registry row remains after deletion")
	}
	var exists int
	if err := adminDB.QueryRow(`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?`, tenantSchema).Scan(&exists); err != nil {
		t.Fatal("check disposable tenant schema:", err)
	}
	if exists != 0 {
		t.Fatal("canonical tenant database remains after deletion")
	}
	var revision uint64
	var payload string
	if err := platformDB.QueryRow(`SELECT directory_revision, payload FROM _kora_site_directory_outbox WHERE site_id = ? ORDER BY directory_revision DESC LIMIT 1`, descriptor.SiteID).Scan(&revision, &payload); err != nil {
		t.Fatal("read durable deletion event:", err)
	}
	var change struct {
		SiteID string `json:"site_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(payload), &change); err != nil {
		t.Fatal("decode durable deletion event:", err)
	}
	if change.Status != "deleted" || change.SiteID != descriptor.SiteID || revision == 0 {
		t.Fatalf("last directory change = status %q, id %q, revision %d; want deleted canonical site", change.Status, change.SiteID, revision)
	}
	// Replaying the Cloud request after the registry row has been removed is a
	// no-op, not a second hostname-derived database drop.
	if err := DeleteRegisteredSite(context.Background(), platformDB, "mysql", descriptor.SiteID, hostname, common); err != nil {
		t.Fatal("idempotent delete replay:", err)
	}
}
