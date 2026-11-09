//go:build integration

package api

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestLibSQLPackageActivationRevisionCAS uses only a fresh disposable sqld
// HTTP service. It intentionally refuses a database with prior Kora tables.
func TestLibSQLPackageActivationRevisionCAS(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_LIBSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_LIBSQL_DSN to a fresh disposable LibSQL HTTP service")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open disposable LibSQL HTTP service:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL HTTP service:", err)
	}
	var existing int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '_kora_config_revision'`).Scan(&existing); err != nil {
		t.Fatal("inspect disposable LibSQL schema:", err)
	}
	if existing != 0 {
		t.Fatal("refusing to mutate LibSQL database that already has Kora config revision state")
	}
	dialect := db.Resolve("libsql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal("bootstrap disposable LibSQL database:", err)
	}
	store := configstore.NewStore(database, dialect)
	activate := func(name string, expected uint64) error {
		tx, err := database.BeginTx(context.Background(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		snapshot := &doctype.ConfigSnapshot{Roles: []*doctype.Role{{Name: name, WorkspaceAccess: true}}}
		if err := store.ActivateSnapshotAtConfigRevision(tx, snapshot, "libsql-cas.example.test", dialect, expected); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := activate("Management CAS Role", 1); err != nil {
		t.Fatal("activate at current LibSQL revision:", err)
	}
	if err := activate("Stale Management CAS Role", 1); !errors.Is(err, configstore.ErrConfigRevisionConflict) {
		t.Fatalf("stale LibSQL activation error = %v, want revision conflict", err)
	}
	if got, err := configstore.CurrentConfigRevision(context.Background(), database, dialect); err != nil || got != 2 {
		t.Fatalf("LibSQL config revision = %d, %v; want 2", got, err)
	}
	roles, err := store.LoadRoles("libsql-cas.example.test")
	if err != nil {
		t.Fatal("load LibSQL roles:", err)
	}
	for _, role := range roles {
		if role.Name == "Stale Management CAS Role" {
			t.Fatal("stale LibSQL activation persisted a role")
		}
	}
}
