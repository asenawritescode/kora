package site

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCreateSiteContextStopsBeforeProvisioningWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateSiteContext(ctx, CreateSiteInput{Hostname: "cancelled.example.invalid"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateSiteContext error = %v, want context cancellation", err)
	}
}

func TestBootstrapSystemTablesContextStopsBeforeDatabaseAccessWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := BootstrapSystemTablesContext(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("BootstrapSystemTablesContext error = %v, want context cancellation", err)
	}
}

func TestCreateSiteRequiresDurableDirectoryBeforeProvisioning(t *testing.T) {
	_, err := CreateSite(CreateSiteInput{
		Hostname: "no-directory.example.invalid", DBType: "mysql", DBHost: "127.0.0.1", DBPort: 1,
		DBName: "must_not_be_created", AdminEmail: "admin@example.invalid", CloudSubjectID: "cloud-user-test",
	})
	if err == nil || !strings.Contains(err.Error(), "durable platform site directory is required") {
		t.Fatalf("CreateSite without directory error = %v, want fail-closed directory requirement", err)
	}
}

func TestCreateSiteRejectsInvalidCanonicalTenantBeforeDatabaseAccess(t *testing.T) {
	_, err := CreateSite(CreateSiteInput{Hostname: "tenant.example.invalid", SiteID: " bad-id ", CloudSubjectID: "cloud-user-test"})
	if err == nil || !strings.Contains(err.Error(), "invalid canonical tenant ID") {
		t.Fatalf("CreateSite invalid canonical tenant ID error = %v", err)
	}
}

func TestCreateSiteRejectsUnsafeDatabaseNameBeforeDirectoryAccess(t *testing.T) {
	platformDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	_, err = CreateSite(CreateSiteInput{
		Hostname: "tenant.example.invalid", DBType: "mysql", DBName: "tenant` CREATE USER root",
		AdminEmail: "admin@example.invalid", CloudSubjectID: "cloud-user-test", PlatformDBType: "mysql", PlatformDB: platformDB,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid tenant database name") {
		t.Fatalf("CreateSite unsafe database name error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database was accessed before rejecting unsafe database name: %v", err)
	}
}

func TestCreateSiteDirectoryWriteFailurePrecedesTenantProvisioning(t *testing.T) {
	platformDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	mock.ExpectExec(regexp.QuoteMeta(`CREATE TABLE IF NOT EXISTS "_kora_site_directory_outbox" ("id" TEXT PRIMARY KEY, "directory_revision" INTEGER NOT NULL DEFAULT 0, "site_id" TEXT NOT NULL, "config_revision" INTEGER NOT NULL, "payload" TEXT NOT NULL, "created_at" TEXT NOT NULL DEFAULT (datetime('now')))`)).
		WillReturnError(errors.New("platform directory unavailable"))
	_, err = CreateSite(CreateSiteInput{
		Hostname: "registry-down.example.invalid", DBType: "mysql", DBHost: "127.0.0.1", DBPort: 1,
		DBName: "must_not_be_created", AdminEmail: "admin@example.invalid", CloudSubjectID: "cloud-user-test",
		PlatformDBType: "libsql", PlatformDB: platformDB,
	})
	if err == nil || !strings.Contains(err.Error(), "initializing platform site directory") {
		t.Fatalf("CreateSite on directory bootstrap failure error = %v, want platform bootstrap failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database operation before durable provisioning intent: %v", err)
	}
}
