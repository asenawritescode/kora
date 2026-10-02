package site

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCreateSiteRequiresDurableDirectoryBeforeProvisioning(t *testing.T) {
	_, err := CreateSite(CreateSiteInput{
		Hostname: "no-directory.example.invalid", DBType: "mysql", DBHost: "127.0.0.1", DBPort: 1,
		DBName: "must_not_be_created", AdminEmail: "admin@example.invalid", AdminPassword: "test-only",
	})
	if err == nil || !strings.Contains(err.Error(), "durable platform site directory is required") {
		t.Fatalf("CreateSite without directory error = %v, want fail-closed directory requirement", err)
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
		DBName: "must_not_be_created", AdminEmail: "admin@example.invalid", AdminPassword: "test-only",
		PlatformDBType: "libsql", PlatformDB: platformDB,
	})
	if err == nil || !strings.Contains(err.Error(), "initializing platform site directory") {
		t.Fatalf("CreateSite on directory bootstrap failure error = %v, want platform bootstrap failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database operation before durable provisioning intent: %v", err)
	}
}
