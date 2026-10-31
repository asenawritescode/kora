//go:build integration

package site

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/db"
)

func TestLiveMySQLEnsureConfigVersionRecoversInterruptedActivation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_SITE_CONFIG_VERSION_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_SITE_CONFIG_VERSION_DISPOSABLE_MYSQL_DSN to a disposable MySQL database")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := BootstrapSystemTables(database, db.Resolve("mysql")); err != nil {
		t.Fatal(err)
	}
	siteName := fmt.Sprintf("config-version-recovery-%d.example.test", time.Now().UnixNano())
	defer database.Exec("DELETE FROM _kora_config_version WHERE site = ?", siteName)
	if err := ensureConfigVersion(database, db.Resolve("mysql"), siteName, []string{siteName}); err != nil {
		t.Fatalf("create initial config version: %v", err)
	}
	var versionID string
	if err := database.QueryRow("SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active'", siteName).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE _kora_config_version SET is_active = 0 WHERE id = ?", versionID); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfigVersion(database, db.Resolve("mysql"), siteName, []string{siteName}); err != nil {
		t.Fatalf("recover interrupted config activation: %v", err)
	}
	var active int
	if err := database.QueryRow("SELECT is_active FROM _kora_config_version WHERE id = ?", versionID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("recovered config version active flag = %d, want 1", active)
	}
	if err := ensureConfigVersion(database, db.Resolve("mysql"), siteName, []string{siteName}); err != nil {
		t.Fatalf("replay completed config bootstrap: %v", err)
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = ?", siteName).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("config version rows after replay = %d, want 1", count)
	}
}
