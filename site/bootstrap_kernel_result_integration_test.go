//go:build integration

package site

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/db"
)

func TestBootstrapMigratesExistingKernelReceiptResults(t *testing.T) {
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		dsn = "root:kora123@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4"
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("MySQL unavailable: %v", err)
	}
	name := fmt.Sprintf("kora_bootstrap_receipt_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		root.Close()
		t.Fatal("create isolated database:", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`")
		_ = root.Close()
	})
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("parse mysql DSN:", err)
	}
	cfg.DBName = name
	database, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open isolated database:", err)
	}
	defer database.Close()
	if _, err := database.Exec(`CREATE TABLE _kora_idempotency_receipt (
		site VARCHAR(140) NOT NULL DEFAULT '', idempotency_key VARCHAR(255) NOT NULL,
		operation_id VARCHAR(26) NOT NULL, command_name VARCHAR(190) NOT NULL,
		payload_hash VARCHAR(64) NOT NULL, result_hash VARCHAR(64) NOT NULL DEFAULT '',
		status VARCHAR(20) NOT NULL DEFAULT 'completed', actor_user VARCHAR(140) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
		PRIMARY KEY (site, idempotency_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		t.Fatal("create pre-migration receipt table:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_system_bootstrap (
		id INTEGER PRIMARY KEY,
		version INTEGER NOT NULL
	)`); err != nil {
		t.Fatal("create existing bootstrap marker:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, 4)`); err != nil {
		t.Fatal("seed v4 bootstrap marker:", err)
	}
	if err := BootstrapSystemTables(database, db.Resolve("mysql")); err != nil {
		t.Fatal("bootstrap existing site:", err)
	}
	if _, err := database.Query(`SELECT result_json FROM _kora_idempotency_receipt LIMIT 0`); err != nil {
		t.Fatalf("bootstrap did not add receipt result column: %v", err)
	}
	var version int
	if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal("read migrated bootstrap marker:", err)
	}
	if version != systemBootstrapVersion {
		t.Fatalf("bootstrap marker version = %d, want %d", version, systemBootstrapVersion)
	}

	// A routine restart must be a no-op after the successful migration.
	if err := BootstrapSystemTables(database, db.Resolve("mysql")); err != nil {
		t.Fatalf("repeat bootstrap after migration: %v", err)
	}
	if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal("read bootstrap marker after repeat:", err)
	}
	if version != systemBootstrapVersion {
		t.Fatalf("bootstrap marker changed after repeat: got %d, want %d", version, systemBootstrapVersion)
	}
}
