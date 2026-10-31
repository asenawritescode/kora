package engine

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/auth"
	kdb "github.com/asenawritescode/kora/db"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
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
	_, err = loadTenantRuntime(context.Background(), site.DBSiteInfo{Name: "tenant.example", DBType: "mysql", DBName: "tenant_test"}, nil, nil, "test-version", tenantRuntimeDependencies{
		connect: func(*site.SiteConfig) (*sql.DB, error) { return database, nil },
		bootstrap: func(*sql.DB, kdb.Dialect) error {
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
	_, err = loadTenantRuntime(context.Background(), info, nil, reusable, "test-version", tenantRuntimeDependencies{
		connect: func(*site.SiteConfig) (*sql.DB, error) {
			t.Fatal("compatible replacement unexpectedly opened a new DB")
			return nil, nil
		},
		bootstrap: func(*sql.DB, kdb.Dialect) error { return bootstrapErr },
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
