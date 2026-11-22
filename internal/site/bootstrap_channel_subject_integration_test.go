//go:build integration

package site

import (
	"context"
	"testing"
)

// TestBootstrapUpgradeAddsChannelSubjectUser models a site at bootstrap version 36, whose channel sessions have no
// subject_user column, and checks that the upgrade to 37 adds it on MySQL and Postgres.
func TestBootstrapUpgradeAddsChannelSubjectUser(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			database, dialect := openUpgradeDatabase(t, driver)
			ctx := context.Background()
			if err := BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("bootstrap fresh site:", err)
			}
			if _, err := database.Exec(`ALTER TABLE _kora_channel_session DROP COLUMN subject_user`); err != nil {
				t.Fatal("simulate v36 site without subject_user:", err)
			}
			if _, err := database.Exec(`UPDATE _kora_system_bootstrap SET version = 36 WHERE id = 1`); err != nil {
				t.Fatal("set v36 marker:", err)
			}
			if err := BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatal("upgrade v36 site:", err)
			}
			var columns int
			query := `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = '_kora_channel_session' AND column_name = 'subject_user'`
			if driver == "postgres" {
				query = `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = '_kora_channel_session' AND column_name = 'subject_user'`
			}
			if err := database.QueryRow(query).Scan(&columns); err != nil {
				t.Fatal("check subject_user column:", err)
			}
			var version int
			if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
				t.Fatal("read bootstrap marker:", err)
			}
			if columns != 1 || version != systemBootstrapVersion {
				t.Fatalf("subject_user columns = %d, marker = %d; want 1 and %d", columns, version, systemBootstrapVersion)
			}
		})
	}
}
