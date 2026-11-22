//go:build integration

package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/go-sql-driver/mysql"
)

// TestBootstrapUpgradeAddsAIPendingWriteTable models a site at bootstrap version 35, which has
// no chat preview table, and checks that the upgrade to 36 creates it on MySQL and Postgres.
func TestBootstrapUpgradeAddsAIPendingWriteTable(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			database, dialect := openUpgradeDatabase(t, driver)
			ctx := context.Background()
			if err := BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("bootstrap fresh site:", err)
			}
			if _, err := database.Exec(`DROP TABLE _kora_ai_pending_write`); err != nil {
				t.Fatal("simulate v35 site without the pending-write table:", err)
			}
			if _, err := database.Exec(`UPDATE _kora_system_bootstrap SET version = 35 WHERE id = 1`); err != nil {
				t.Fatal("set v35 marker:", err)
			}
			if err := BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("upgrade v35 site:", err)
			}
			var tables int
			query := `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '_kora_ai_pending_write'`
			if driver == "postgres" {
				query = `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = '_kora_ai_pending_write'`
			}
			if err := database.QueryRow(query).Scan(&tables); err != nil {
				t.Fatal("check pending-write table:", err)
			}
			var version int
			if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
				t.Fatal("read bootstrap marker:", err)
			}
			if tables != 1 || version != systemBootstrapVersion {
				t.Fatalf("pending-write tables = %d, marker = %d; want 1 and %d", tables, version, systemBootstrapVersion)
			}
		})
	}
}

// openUpgradeDatabase returns an empty database of the driver: a new MySQL database from
// KORA_TEST_DSN, or the isolated Postgres schema of the role-upgrade helper.
func openUpgradeDatabase(t *testing.T, driver string) (*sql.DB, db.Dialect) {
	t.Helper()
	if driver == "postgres" {
		return openPostgresUpgradeSchema(t), db.Resolve("postgres")
	}
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		t.Skip("set KORA_TEST_DSN to run against a disposable MySQL server")
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	name := fmt.Sprintf("kora_ai_pending_upgrade_%d", time.Now().UnixNano()%1_000_000_000)
	if _, cerr := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); cerr != nil {
		t.Fatal("create isolated database:", cerr)
	}
	t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`") })
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse MySQL DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, db.Resolve("mysql")
}
