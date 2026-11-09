package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
)

func TestCompatibleReloadCanReuseDatabaseAndSessionResources(t *testing.T) {
	database := &sql.DB{}
	manager := &auth.SessionManager{}
	existing := &knet.LoadedSite{DB: database, SessionManager: manager, DatabaseIdentity: "same-target"}
	if !canReuseTenantResources(existing, "same-target") {
		t.Fatal("compatible DB identity should retain stable pool and session resources")
	}
	for _, candidate := range []string{"changed-target", ""} {
		if canReuseTenantResources(existing, candidate) {
			t.Fatalf("identity %q must not reuse a tenant pool", candidate)
		}
	}
	if canReuseTenantResources(&knet.LoadedSite{DatabaseIdentity: "same-target"}, "same-target") {
		t.Fatal("incomplete runtime resources must not be reused")
	}
}

func TestLoadTenantRuntimeJoinsOwnedDBCloseFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	bootstrapErr := errors.New("tenant bootstrap failed")
	closeErr := errors.New("tenant DB close failed")
	mock.ExpectClose().WillReturnError(closeErr)
	_, err = loadTenantRuntime(context.Background(), tenantRuntimeRequest{info: site.DBSiteInfo{Name: "tenant.example", DBType: "mysql", DBName: "tenant_test"}, binaryVersion: "test-version"}, tenantRuntimeDependencies{
		connect: func(context.Context, *site.Config) (*sql.DB, error) { return database, nil },
		bootstrap: func(context.Context, *sql.DB, kdb.Dialect) error {
			return bootstrapErr
		},
	})
	if !errors.Is(err, bootstrapErr) || !errors.Is(err, closeErr) {
		t.Fatalf("LoadTenantRuntime error = %v, want bootstrap and close failures", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTenantRuntimeNeverClosesBorrowedDBOnFailedReplacement(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	bootstrapErr := errors.New("replacement bootstrap failed")
	info := site.DBSiteInfo{Name: "tenant.example", DBType: "mysql", DBName: "tenant_test"}
	config := site.ReconstructSiteConfigFromDBInfo(info, nil)
	reusable := &knet.LoadedSite{
		DB: database, SessionManager: &auth.SessionManager{}, DatabaseIdentity: site.ConnectionIdentity(config),
	}
	_, err = loadTenantRuntime(context.Background(), tenantRuntimeRequest{info: info, reusable: reusable, binaryVersion: "test-version"}, tenantRuntimeDependencies{
		connect: func(context.Context, *site.Config) (*sql.DB, error) {
			t.Fatal("compatible replacement unexpectedly opened a new DB")
			return nil, nil
		},
		bootstrap: func(context.Context, *sql.DB, kdb.Dialect) error { return bootstrapErr },
	})
	if !errors.Is(err, bootstrapErr) {
		t.Fatalf("LoadTenantRuntime error = %v, want bootstrap failure", err)
	}
	mock.ExpectPing()
	if err := database.Ping(); err != nil {
		t.Fatalf("failed replacement closed borrowed DB: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTenantRuntimeRefusesSecondTenantOnSharedLibSQL(t *testing.T) {
	tests := []struct {
		name      string
		dbType    string
		wantClaim bool
	}{
		{name: "libsql tenant must claim the shared database", dbType: "libsql", wantClaim: true},
		{name: "mysql tenant has its own database", dbType: "mysql", wantClaim: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, _, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			claimed := ""
			_, err = loadTenantRuntime(context.Background(), tenantRuntimeRequest{info: site.DBSiteInfo{SiteID: "tenant-b", Name: "b.example", DBType: tt.dbType, DBHost: "libsql://shared.example", DBName: "tenant_b"}, binaryVersion: "test-version"}, tenantRuntimeDependencies{
				connect:   func(context.Context, *site.Config) (*sql.DB, error) { return database, nil },
				bootstrap: func(context.Context, *sql.DB, kdb.Dialect) error { return nil },
				claimOwner: func(_ context.Context, _ *sql.DB, _ kdb.Dialect, tenantID string) error {
					claimed = tenantID
					return fmt.Errorf("%w: owned by tenant-a", site.ErrDatabaseOwnedByAnotherTenant)
				},
			})
			if tt.wantClaim {
				if claimed != "tenant-b" {
					t.Fatalf("claimed tenant = %q, want tenant-b", claimed)
				}
				if !errors.Is(err, site.ErrDatabaseOwnedByAnotherTenant) {
					t.Fatalf("LoadTenantRuntime error = %v, want ownership refusal", err)
				}
				return
			}
			if claimed != "" {
				t.Fatalf("mysql tenant unexpectedly claimed database as %q", claimed)
			}
		})
	}
}
