//go:build integration

package site

import (
	"context"
	"os"
	"testing"
)

func TestLiveMySQLTenantPoolUsesConfiguredBounds(t *testing.T) {
	dsn := os.Getenv("KORA_POOL_MYSQL_DSN")
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		t.Skip("set KORA_POOL_MYSQL_DSN (or DB_DSN) to a disposable MySQL database for the live pool test")
	}
	t.Setenv("DB_DSN", dsn)
	t.Setenv("KORA_DB_TYPE", "mysql")
	t.Setenv("KORA_DB_MAX_OPEN", "2")
	t.Setenv("KORA_DB_MAX_IDLE", "0")
	t.Setenv("KORA_DB_USER", "")
	common := CommonConfigFromEnv()
	cfg := ReconstructSiteConfig("pool-budget-check", common, nil)
	database, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatal("connect to the configured read-only test database:", err)
	}
	defer database.Close()

	if got := database.Stats().MaxOpenConnections; got != 2 {
		t.Fatalf("live MySQL max-open = %d, want 2", got)
	}
	var one int
	if err := database.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("read-only connection check returned %d, %v", one, err)
	}
	stats := database.Stats()
	if stats.OpenConnections != 0 || stats.Idle != 0 {
		t.Fatalf("live MySQL pool retained connections with max-idle zero: %+v", stats)
	}
}
