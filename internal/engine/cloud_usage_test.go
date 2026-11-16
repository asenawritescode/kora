package engine

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"github.com/asenawritescode/kora/internal/usage"
	_ "github.com/mattn/go-sqlite3"
)

// The Cloud usage reporter is one more tenant worker start when reporting is
// configured, and none when it is not.
func TestCloudUsageReporterRegisteredOnlyWhenConfigured(t *testing.T) {
	tests := []struct {
		name       string
		cloudUsage *usage.Config
		wantStarts int
	}{
		{name: "reporting off", wantStarts: 0},
		{name: "reporting on", cloudUsage: &usage.Config{URL: "http://cloud.test/api/cloud/usage", Token: "t", Interval: time.Hour}, wantStarts: 1},
	}
	off := tenantWorkerStarts(t, nil)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tenantWorkerStarts(t, tt.cloudUsage) - off
			if got != tt.wantStarts {
				t.Fatalf("usage reporter registrations = %d, want %d", got, tt.wantStarts)
			}
		})
	}
}

// tenantWorkerStarts installs the tenant services on a fresh tenant and counts
// the worker starts they register.
func tenantWorkerStarts(t *testing.T, cloudUsage *usage.Config) int {
	t.Helper()
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err = site.BootstrapSystemTables(context.Background(), database, db.Resolve("libsql")); err != nil {
		t.Fatal(err)
	}
	const siteName = "usage.example.test"
	info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: "libsql", DBName: "main", Status: "active"}
	common := &site.CommonConfig{DBType: info.DBType}
	loaded, err := loadTenantRuntime(context.Background(), tenantRuntimeRequest{info: info, common: common, binaryVersion: "test"}, tenantRuntimeDependencies{
		connect:   func(context.Context, *site.Config) (*sql.DB, error) { return database, nil },
		bootstrap: func(context.Context, *sql.DB, db.Dialect) error { return nil },
	})
	if err != nil {
		t.Fatalf("load tenant runtime: %v", err)
	}
	starts := 0
	install, err := NewTenantServiceInstaller(TenantServiceConfig{
		Common:         common,
		KernelCommands: kernel.NewCommandRegistry(),
		CloudUsage:     cloudUsage,
		ResolveStorage: func(*site.Config) (storage.Backend, error) {
			return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
		},
		RegisterWorkerStart: func(func() error) error {
			starts++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := install(info, loaded); err != nil {
		t.Fatalf("install tenant services: %v", err)
	}
	return starts
}
