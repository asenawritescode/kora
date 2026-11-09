package engine

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// installPackTenant imports pos+inventory into a fresh tenant DB and installs
// the tenant services, which validates every published operation's layout
// against its generated output schema (catalog.layout_invalid).
func installPackTenant(t *testing.T, driver string, database *sql.DB, dbName string) *knet.LoadedSite {
	t.Helper()
	dialect := db.Resolve(driver)
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal(err)
	}
	siteName := fmt.Sprintf("pack-install-%d.example.test", time.Now().UnixNano())
	if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
		t.Fatalf("import pos+inventory: %v", err)
	}
	info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: driver, DBName: dbName, Status: "active"}
	common := &site.CommonConfig{DBType: driver}
	loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
		connect:   func(*site.Config) (*sql.DB, error) { return database, nil },
		bootstrap: func(*sql.DB, db.Dialect) error { return nil },
	})
	if err != nil {
		t.Fatalf("load tenant runtime: %v", err)
	}
	install, err := NewTenantServiceInstaller(TenantServiceConfig{
		Common:         common,
		KernelCommands: kernel.NewCommandRegistry(),
		ResolveStorage: func(*site.Config) (storage.Backend, error) {
			return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := install(info, loaded); err != nil {
		t.Fatalf("install tenant services: %v", err)
	}
	if b := loaded.RuntimeServices.ModuleBindings; b == nil || len(b.Blockers) != 0 {
		t.Fatalf("module bindings = %+v", b)
	}
	return loaded
}

func TestPOSPackTenantServicesInstallLibSQL(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	installPackTenant(t, "libsql", database, "main")
}
