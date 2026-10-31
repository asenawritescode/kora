package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
)

func TestAssembleProcessStartupRollsBackPlatformAcquisitionFailures(t *testing.T) {
	for _, stage := range []string{"platform", "tenants"} {
		t.Run(stage, func(t *testing.T) {
			var closed []string
			startupErr := errors.New(stage + " startup failure")
			resources := &ProcessResources{}
			var platformDB *sql.DB
			config := ProcessStartupConfig{
				Telemetry: TelemetryConfig{ServiceVersion: "test"},
				DBDriver:  "mysql", DBDSN: "mysql://platform", DBDialect: "mysql",
				Common: &site.CommonConfig{}, ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
			}
			deps := processStartupDependencies{
				initializeTelemetry: func(context.Context, TelemetryConfig) (func(context.Context) error, error) {
					return func(context.Context) error { closed = append(closed, "telemetry"); return nil }, nil
				},
			}
			if stage == "platform" {
				deps.openPlatform = func(string, string, string, string) (PlatformSiteDirectory, error) {
					return PlatformSiteDirectory{}, startupErr
				}
				deps.loadTenants = func(context.Context, StartupTenantConfig) (StartupTenants, error) {
					t.Fatal("tenant loader ran after platform startup failed")
					return StartupTenants{}, nil
				}
			} else {
				database, mock, err := sqlmock.New()
				if err != nil {
					t.Fatal("create platform DB mock:", err)
				}
				mock.ExpectClose()
				platformDB = database
				deps.openPlatform = func(string, string, string, string) (PlatformSiteDirectory, error) {
					return PlatformSiteDirectory{DB: database}, nil
				}
				deps.loadTenants = func(context.Context, StartupTenantConfig) (StartupTenants, error) {
					return StartupTenants{}, startupErr
				}
				defer func() {
					if err := mock.ExpectationsWereMet(); err != nil {
						t.Error("platform DB rollback was not completed:", err)
					}
				}()
			}
			_, err := assembleProcessStartup(context.Background(), config, resources, deps)
			if !errors.Is(err, startupErr) {
				t.Fatalf("assembleProcessStartup error = %v, want %v", err, startupErr)
			}
			if err := resources.Shutdown(context.Background()); err != nil {
				t.Fatalf("startup rollback: %v", err)
			}
			if len(closed) != 1 || closed[0] != "telemetry" {
				t.Fatalf("rollback order = %v, want [telemetry]", closed)
			}
			if stage == "tenants" {
				if err := platformDB.Ping(); err == nil {
					t.Fatal("startup rollback left platform DB open")
				}
			}
		})
	}
}

func TestAssembleProcessStartupUsesCredentialFreeLazyTenantSnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create platform DB mock:", err)
	}
	mock.ExpectClose()
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error("platform DB was not closed:", err)
		}
	}()
	credentialBearing := []site.DBSiteInfo{{SiteID: "tenant-a", DBPassword: "sensitive-test-value"}}
	config := ProcessStartupConfig{
		Telemetry: TelemetryConfig{ServiceVersion: "test"},
		DBDriver:  "mysql", DBDSN: "mysql://platform", DBDialect: "mysql",
		Common: &site.CommonConfig{}, ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) { return nil, nil },
	}
	deps := processStartupDependencies{
		initializeTelemetry: func(context.Context, TelemetryConfig) (func(context.Context) error, error) {
			return func(context.Context) error { return nil }, nil
		},
		openPlatform: func(string, string, string, string) (PlatformSiteDirectory, error) {
			return PlatformSiteDirectory{
				DB: database, Sites: credentialBearing,
				Descriptors: []contract.SiteDescriptor{{SiteID: "tenant-a", Aliases: []string{"tenant-a.example"}, Status: "active", RuntimeCellID: "default", ConfigRevision: 4}},
			}, nil
		},
		loadTenants: func(_ context.Context, cfg StartupTenantConfig) (StartupTenants, error) {
			if len(cfg.Sites) != 0 {
				t.Fatalf("startup loader received tenant credential records: %#v", cfg.Sites)
			}
			return StartupTenants{}, nil
		},
	}
	resources := &ProcessResources{}
	startup, err := assembleProcessStartup(context.Background(), config, resources, deps)
	if err != nil {
		t.Fatal("assemble startup:", err)
	}
	if startup.DiscoveredTenants != 1 {
		t.Fatalf("discovered tenant count = %d, want 1", startup.DiscoveredTenants)
	}
	if len(startup.Platform.Sites) != 0 {
		t.Fatalf("startup assembly retained credential-bearing tenant snapshot: %#v", startup.Platform.Sites)
	}
	if len(startup.Platform.Descriptors) != 1 || startup.Platform.Descriptors[0].Aliases[0] != "tenant-a.example" {
		t.Fatalf("startup descriptors = %#v, want the credential-free local descriptor", startup.Platform.Descriptors)
	}
	if len(startup.Tenants.Runtimes) != 0 || startup.DiscoveredTenants != 1 || len(startup.Tenants.Domains) != 1 {
		t.Fatalf("lazy startup tenant result = runtimes %d, discovered %d, domains %v", len(startup.Tenants.Runtimes), startup.DiscoveredTenants, startup.Tenants.Domains)
	}
	if err := resources.Shutdown(context.Background()); err != nil {
		t.Fatal("shutdown startup resources:", err)
	}
}
