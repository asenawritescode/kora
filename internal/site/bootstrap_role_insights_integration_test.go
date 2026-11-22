//go:build integration

package site

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// TestBootstrapAddsInsightsAccessToExistingRoleTable upgrades a tenant whose _kora_role table predates the
// insights_access flag. The marker stays at the previous bootstrap version, so the upgrade must add the column.
func TestBootstrapAddsInsightsAccessToExistingRoleTable(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		open    func(t *testing.T) *sql.DB
	}{
		{name: "mysql", dialect: "mysql", open: openMySQLUpgradeDatabase},
		{name: "postgres", dialect: "postgres", open: openPostgresUpgradeSchema},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := tc.open(t)
			dialect := db.Resolve(tc.dialect)
			for _, stmt := range []string{
				`CREATE TABLE _kora_role (name VARCHAR(140) PRIMARY KEY, workspace_access INT NOT NULL DEFAULT 1, description TEXT)`,
				`INSERT INTO _kora_role (name, workspace_access, description) VALUES ('Manager', 1, '')`,
				`CREATE TABLE _kora_system_bootstrap (id INTEGER PRIMARY KEY, version INTEGER NOT NULL)`,
				`INSERT INTO _kora_system_bootstrap (id, version) VALUES (1, 34)`,
			} {
				if _, err := database.Exec(stmt); err != nil {
					t.Fatalf("create pre-upgrade fixture: %v\n%s", err, stmt)
				}
			}
			for run := 1; run <= 2; run++ {
				if err := BootstrapSystemTables(context.Background(), database, dialect); err != nil {
					t.Fatalf("bootstrap run %d: %v", run, err)
				}
			}
			if _, err := database.Exec(`SELECT insights_access FROM _kora_role WHERE 1 = 0`); err != nil {
				t.Fatalf("insights_access column missing after upgrade: %v", err)
			}
			var version int
			if err := database.QueryRow(`SELECT version FROM _kora_system_bootstrap WHERE id = 1`).Scan(&version); err != nil {
				t.Fatal("read bootstrap marker:", err)
			}
			if version != systemBootstrapVersion {
				t.Fatalf("bootstrap marker = %d, want %d", version, systemBootstrapVersion)
			}
			store := configstore.NewStore(database, dialect)
			if err := store.SaveRoles([]*doctype.Role{{Name: "Cashier", WorkspaceAccess: true, InsightsAccess: boolPointer(false)}}, "site-a"); err != nil {
				t.Fatal("save roles after upgrade:", err)
			}
			roles, err := store.LoadRoles("site-a")
			if err != nil {
				t.Fatal("load roles after upgrade:", err)
			}
			allowed := map[string]bool{}
			for _, role := range roles {
				allowed[role.Name] = role.InsightsAllowed()
			}
			if allowed["Cashier"] || !allowed["Manager"] {
				t.Fatalf("insights after upgrade = %v, want Cashier denied and pre-existing Manager allowed", allowed)
			}
		})
	}
}

func boolPointer(b bool) *bool { return &b }

// openMySQLUpgradeDatabase creates an empty database from KORA_TEST_DSN and returns a handle to it.
func openMySQLUpgradeDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KORA_TEST_DSN")
	if dsn == "" {
		t.Skip("set KORA_TEST_DSN to run against a disposable MySQL server")
	}
	root, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal("open mysql:", err)
	}
	name := fmt.Sprintf("kora_role_upgrade_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err = root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = root.Close()
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
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// openPostgresUpgradeSchema creates an empty schema from KORA_SITE_ROLE_UPGRADE_POSTGRES_DSN and pins the
// returned handle's only connection to it.
func openPostgresUpgradeSchema(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("KORA_SITE_ROLE_UPGRADE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_SITE_ROLE_UPGRADE_POSTGRES_DSN to run against a disposable PostgreSQL server")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open postgres:", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	schema := fmt.Sprintf("kora_role_upgrade_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := database.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		t.Fatal("create isolated schema:", err)
	}
	t.Cleanup(func() { _, _ = database.Exec(`DROP SCHEMA "` + schema + `" CASCADE`) })
	if _, err := database.Exec(`SET search_path TO "` + schema + `"`); err != nil {
		t.Fatal("select isolated schema:", err)
	}
	return database
}
