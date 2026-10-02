package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/site"
)

// TestLiveMySQLSiteDirectoryAssignCellWorkflow exercises the real revisioned
// assignment path in a uniquely named, disposable schema. It never writes to
// the DSN's configured schema or opens a tenant database.
func TestLiveMySQLSiteDirectoryAssignCellWorkflow(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse disposable MySQL DSN:", err)
	}
	cfg.DBName = "mysql"
	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open MySQL admin database:", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping disposable MySQL server:", err)
	}

	schema := fmt.Sprintf("kora_cell_assign_%d", time.Now().UnixNano())
	if _, err := adminDB.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable assignment schema:", err)
	}
	defer func() {
		if _, err := adminDB.Exec("DROP DATABASE `" + schema + "`"); err != nil {
			t.Errorf("drop disposable assignment schema %s: %v", schema, err)
			return
		}
		var remaining string
		err := adminDB.QueryRow(`SELECT schema_name FROM information_schema.schemata WHERE schema_name = ?`, schema).Scan(&remaining)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("disposable assignment schema remains after rollback: name=%s err=%v", remaining, err)
		}
	}()

	cfg.DBName = schema
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open disposable assignment schema:", err)
	}
	defer database.Close()
	if err := site.BootstrapPlatformRegistry(database, kdb.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap disposable site directory:", err)
	}
	registry := site.NewSQLSiteRegistry(database, "mysql")
	hostname := fmt.Sprintf("cell-assignment-%d.example.invalid", time.Now().UnixNano())
	if err := registry.Upsert(&site.SiteConfig{
		Hostname: hostname, DomainsList: []string{hostname}, DBType: "mysql",
		DBHost: "127.0.0.1", DBPort: 3306, DBName: "unused_tenant_database",
	}); err != nil {
		t.Fatal("create disposable directory entry:", err)
	}
	info, err := registry.ResolveAlias(hostname)
	if err != nil {
		t.Fatal("resolve disposable entry:", err)
	}

	preview, err := applySiteCellAssignment(registry, info.SiteID, "cell-b", false, nil)
	if err != nil {
		t.Fatal("preview SQL-backed assignment:", err)
	}
	if preview.Applied || preview.RuntimeCell != "cell-b" || preview.ConfigRevision != info.ConfigRevision {
		t.Fatalf("SQL-backed preview = %+v; want unchanged revision %d and no apply", preview, info.ConfigRevision)
	}

	applied, err := applySiteCellAssignment(registry, info.SiteID, "cell-b", true, map[string]string{"cell-b": "http://127.0.0.1:8002"})
	if err != nil {
		t.Fatal("apply SQL-backed assignment:", err)
	}
	if !applied.Applied || applied.RuntimeCell != "cell-b" || applied.ConfigRevision != info.ConfigRevision+1 {
		t.Fatalf("applied SQL-backed assignment = %+v; want cell-b at revision %d", applied, info.ConfigRevision+1)
	}
	current, err := registry.GetDescriptorByID(info.SiteID)
	if err != nil {
		t.Fatal("read committed cell assignment:", err)
	}
	if current.RuntimeCellID != "cell-b" || current.ConfigRevision != applied.ConfigRevision {
		t.Fatalf("committed descriptor = %+v; want assignment report %+v", current, applied)
	}
	t.Logf("isolated MySQL assignment preview/apply passed: site=%s revision=%d→%d", info.SiteID, info.ConfigRevision, applied.ConfigRevision)
}
