package site

import (
	"context"
	"net/url"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAcquireSiteDeletionLockReleasesMySQLAdvisoryLock(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT GET_LOCK(?, 0)`)).
		WithArgs("kora:site-delete:site-1").WillReturnRows(sqlmock.NewRows([]string{"GET_LOCK"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT RELEASE_LOCK(?)`)).
		WithArgs("kora:site-delete:site-1").WillReturnRows(sqlmock.NewRows([]string{"RELEASE_LOCK"}).AddRow(1))
	locked, release, err := acquireSiteDeletionLock(context.Background(), database, "mysql", "site-1")
	if err != nil || !locked {
		t.Fatalf("acquire lock = (%v, %v), want acquired lock", locked, err)
	}
	release()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireSiteDeletionLockReportsAnotherWorker(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT GET_LOCK(?, 0)`)).
		WithArgs("kora:site-delete:site-1").WillReturnRows(sqlmock.NewRows([]string{"GET_LOCK"}).AddRow(0))
	locked, release, err := acquireSiteDeletionLock(context.Background(), database, "mysql", "site-1")
	if err != nil || locked || release != nil {
		t.Fatalf("acquire contended lock = (locked=%v, release-set=%v, err=%v), want not acquired without error", locked, release != nil, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryPendingSiteDeletionsIgnoresNonDeletingSites(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry ORDER BY site`)).
		WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "domains_json", "status", "config_revision"}).
			AddRow("site-active", "", "active.example", `[]`, "active", 1).
			AddRow("site-suspended", "", "paused.example", `[]`, "suspended", 2))
	if err := RetryPendingSiteDeletions(context.Background(), database, "mysql", nil); err != nil {
		t.Fatalf("retry deletions with no pending sites: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRegisteredSiteRejectsMismatchedHostnameBeforeFencing(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	expectDeletionLookup(mock, "site-1", "active")
	if err := DeleteRegisteredSite(context.Background(), database, "mysql", "site-1", "other.example", nil); err == nil {
		t.Fatal("DeleteRegisteredSite accepted a hostname that is not an alias of the canonical site")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryOneSiteDeletionDoesNotInferDatabaseName(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	expectDeletionLookup(mock, "site-1", "deleting")
	if err := retryOneSiteDeletion(NewSQLSiteRegistry(database, "mysql"), "site-1", database, "mysql", nil); err == nil {
		t.Fatal("deletion inferred a database name from the hostname")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectDeletionLookup(mock sqlmock.Sqlmock, siteID, status string) {
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(siteID).
		WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision"}).
			AddRow(siteID, "", "shop.example", "mysql", "db.example", 3306, "", "user", "secret", 0, `[]`, "local", "", status, 3))
}

func TestPostgresAdminDSNTargetsMaintenanceDatabase(t *testing.T) {
	dsn, err := postgresAdminDSN("postgres://user:secret@db.example:5432/tenant?sslmode=require", "", 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/postgres" || parsed.Query().Get("sslmode") != "require" {
		t.Fatalf("admin DSN path/options = %q/%q, want /postgres and sslmode=require", parsed.Path, parsed.Query().Get("sslmode"))
	}
}
