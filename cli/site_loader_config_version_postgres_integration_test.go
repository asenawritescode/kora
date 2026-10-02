package cli

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

// TestLivePostgresActiveConfigMinKoraVersion verifies the startup metadata
// query against a real PostgreSQL server. Set KORA_API_LIVE_POSTGRES_DSN to a
// disposable database; the test creates and drops a private schema.
func TestLivePostgresActiveConfigMinKoraVersion(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test startup config metadata against disposable PostgreSQL")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_startup_version_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_config_version (
		id TEXT PRIMARY KEY, site TEXT NOT NULL, status TEXT NOT NULL,
		version INTEGER NOT NULL, min_kora_version TEXT
	)`); err != nil {
		t.Fatal("create config-version fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_config_version (id, site, status, version, min_kora_version)
		VALUES ('active-1', 'tenant.example.test', 'Active', 1, '2.4.0')`); err != nil {
		t.Fatal("insert active config fixture:", err)
	}
	got, err := activeConfigMinKoraVersion(database, &kdb.PostgresDialect{}, "tenant.example.test")
	if err != nil {
		t.Fatal("read active config minimum version:", err)
	}
	if got != "2.4.0" {
		t.Fatalf("active config minimum version = %q, want 2.4.0", got)
	}
}
