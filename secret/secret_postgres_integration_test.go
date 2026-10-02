package secret

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	_ "github.com/lib/pq"
)

// TestLivePostgresSecretStore is opt-in and must only use a disposable database:
// it creates the shared _kora_secret table. Set KORA_SECRET_LIVE_POSTGRES_DSN.
func TestLivePostgresSecretStore(t *testing.T) {
	dsn := os.Getenv("KORA_SECRET_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SECRET_LIVE_POSTGRES_DSN to run live PostgreSQL secret-store acceptance against a disposable database")
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

	store := NewStore(database, kdb.Resolve("postgres"))
	if err := store.EnsureTable(); err != nil {
		t.Fatal("create PostgreSQL secret table:", err)
	}
	site := fmt.Sprintf("secret-live-%d", time.Now().UnixNano())
	if err := store.Set(site, "provider-key", "initial-value"); err != nil {
		t.Fatal("insert PostgreSQL secret:", err)
	}
	if got, err := store.Get(site, "provider-key"); err != nil || got != "initial-value" {
		t.Fatalf("read PostgreSQL secret = %q, %v", got, err)
	}
	if err := store.Set(site, "provider-key", "updated-value"); err != nil {
		t.Fatal("update PostgreSQL secret:", err)
	}
	if got, err := store.Get(site, "provider-key"); err != nil || got != "updated-value" {
		t.Fatalf("read updated PostgreSQL secret = %q, %v", got, err)
	}
	keys, err := store.List(site)
	if err != nil || len(keys) != 1 || keys[0] != "provider-key" {
		t.Fatalf("list PostgreSQL secrets = %v, %v", keys, err)
	}
	if err := store.Delete(site, "provider-key"); err != nil {
		t.Fatal("delete PostgreSQL secret:", err)
	}
	if _, err := store.Get(site, "provider-key"); err == nil {
		t.Fatal("deleted PostgreSQL secret still exists")
	}
}
