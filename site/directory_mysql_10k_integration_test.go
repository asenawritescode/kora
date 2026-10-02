package site

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// TestLiveMySQLDirectoryDescriptorsAt10K validates the real metadata query at
// 10k rows without creating or connecting to any tenant database. It creates
// and drops its own temporary schema on the explicitly configured test server.
func TestLiveMySQLDirectoryDescriptorsAt10K(t *testing.T) {
	dsn := os.Getenv("KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_DIRECTORY_LIVE_MYSQL_DSN to a disposable MySQL server")
	}
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse live MySQL DSN:", err)
	}
	cfg.DBName = "mysql"
	adminDB, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open MySQL admin connection:", err)
	}
	defer adminDB.Close()
	if err := adminDB.Ping(); err != nil {
		t.Fatal("ping MySQL admin connection:", err)
	}
	schema := fmt.Sprintf("kora_directory_10k_%d", time.Now().UnixNano())
	if _, err := adminDB.Exec("CREATE DATABASE `" + schema + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatal("create disposable benchmark schema:", err)
	}
	defer func() {
		if _, err := adminDB.Exec("DROP DATABASE `" + schema + "`"); err != nil {
			t.Errorf("drop disposable benchmark schema: %v", err)
		}
	}()

	cfg.DBName = schema
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open disposable benchmark schema:", err)
	}
	defer database.Close()
	if err := BootstrapPlatformRegistry(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap site registry:", err)
	}
	const count = 10_000
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin metadata setup:", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO _kora_site_registry
		(site, site_id, db_type, db_host, db_port, db_name, db_user, db_password, domains_json, status, config_revision, runtime_cell_id)
		VALUES (?, ?, 'mysql', 'credential-host-sentinel', 3306, 'credential-db-sentinel', 'credential-user-sentinel', 'credential-password-sentinel', ?, 'active', 1, ?)`)
	if err != nil {
		tx.Rollback()
		t.Fatal("prepare metadata setup:", err)
	}
	for i := 0; i < count; i++ {
		hostname := fmt.Sprintf("site-%05d.example.invalid", i)
		aliases, _ := json.Marshal([]string{hostname, "www." + hostname})
		cellID := fmt.Sprintf("cell-%02d", i%10)
		if _, err := stmt.Exec(hostname, fmt.Sprintf("site-id-%05d", i), string(aliases), cellID); err != nil {
			stmt.Close()
			tx.Rollback()
			t.Fatalf("insert metadata row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		t.Fatal("close metadata insert:", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit metadata setup:", err)
	}
	if _, err := database.Exec(`ANALYZE TABLE _kora_site_registry`); err != nil {
		t.Fatal("analyze seeded site registry:", err)
	}
	var plan string
	if err := database.QueryRow(`EXPLAIN FORMAT=JSON SELECT site_id, runtime_cell_id, site FROM _kora_site_registry WHERE status = 'active' AND runtime_cell_id = ? ORDER BY site`, "cell-00").Scan(&plan); err != nil {
		t.Fatal("explain cell-scoped startup query:", err)
	}
	if !strings.Contains(plan, "idx_site_registry_cell_startup") {
		t.Fatalf("cell startup query does not use composite index: %s", plan)
	}

	started := time.Now()
	descriptors, err := NewSQLSiteRegistry(database, "mysql").GetDirectoryDescriptors()
	duration := time.Since(started)
	if err != nil {
		t.Fatal("read 10k credential-free descriptors:", err)
	}
	if len(descriptors) != count {
		t.Fatalf("descriptor count = %d, want %d", len(descriptors), count)
	}
	registry := NewSQLSiteRegistry(database, "mysql")
	for i := 0; i < 10; i++ {
		cellID := fmt.Sprintf("cell-%02d", i)
		cellSites, err := registry.GetSnapshotForCell(cellID)
		if err != nil {
			t.Fatalf("read eager snapshot for %s: %v", cellID, err)
		}
		if len(cellSites) != count/10 {
			t.Fatalf("cell %s returned %d sites, want %d", cellID, len(cellSites), count/10)
		}
		for _, info := range cellSites {
			if info.RuntimeCellID != cellID {
				t.Fatalf("cell %s loaded foreign site assignment %q", cellID, info.RuntimeCellID)
			}
		}
	}
	defaultSites, err := registry.GetSnapshotForCell("default")
	if err != nil {
		t.Fatal("read default eager snapshot:", err)
	}
	if len(defaultSites) != 0 {
		t.Fatalf("default cell returned %d explicitly assigned sites, want zero", len(defaultSites))
	}
	wire, err := json.Marshal(descriptors)
	if err != nil {
		t.Fatal("marshal descriptors:", err)
	}
	for _, sentinel := range []string{"credential-host-sentinel", "credential-db-sentinel", "credential-user-sentinel", "credential-password-sentinel"} {
		if strings.Contains(string(wire), sentinel) {
			t.Fatalf("credential field %q leaked into directory descriptors", sentinel)
		}
	}
	t.Logf("10,000 MySQL directory descriptors read in %s; no tenant pools opened", duration)
}
