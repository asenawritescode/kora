package site

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDiscoverSitesFromDBUsesRegistryWhenAvailable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json",
	}).AddRow(
		"acme.kora.dev", "mysql", "db.internal", 3306, "acme_kora_dev", "tenant_user", "", 0, `["acme.kora.dev","app.acme.dev"]`,
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]') FROM _kora_site_registry WHERE status = 'active' ORDER BY site`)).
		WillReturnRows(rows)

	// DiscoverSitesFromDB always also queries the legacy config table and merges.
	legacyRows := sqlmock.NewRows([]string{"site", "config"})
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DISTINCT site, config FROM _kora_config_version WHERE status = 'Active'`)).
		WillReturnRows(legacyRows)

	sites, err := DiscoverSitesFromDB(db)
	if err != nil {
		t.Fatalf("DiscoverSitesFromDB: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("len(sites) = %d, want 1", len(sites))
	}
	if sites[0].DBHost != "db.internal" {
		t.Fatalf("DBHost = %q", sites[0].DBHost)
	}
	if sites[0].DBName != "acme_kora_dev" {
		t.Fatalf("DBName = %q", sites[0].DBName)
	}
	if len(sites[0].Domains) != 2 {
		t.Fatalf("Domains len = %d", len(sites[0].Domains))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestDiscoverSitesFromDBFallsBackToLegacyConfigVersions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]') FROM _kora_site_registry WHERE status = 'active' ORDER BY site`)).
		WillReturnError(assertRegistryMissingError{})

	rows := sqlmock.NewRows([]string{"site", "config"}).
		AddRow("legacy.kora.dev", `{"domains":["legacy.kora.dev","legacy.example.com"]}`)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DISTINCT site, config FROM _kora_config_version WHERE status = 'Active'`)).
		WillReturnRows(rows)

	sites, err := DiscoverSitesFromDB(db)
	if err != nil {
		t.Fatalf("DiscoverSitesFromDB: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("len(sites) = %d, want 1", len(sites))
	}
	if sites[0].Name != "legacy.kora.dev" {
		t.Fatalf("Name = %q", sites[0].Name)
	}
	if len(sites[0].Domains) != 2 {
		t.Fatalf("Domains len = %d", len(sites[0].Domains))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestDiscoverSitesFromDBUsesRegistryWhenLegacyConfigTableMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json",
	}).AddRow(
		"partner", "mysql", "kora-mysql-lh5l6r", 3306, "partner", "root", "", 0, `["partner"]`,
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]') FROM _kora_site_registry WHERE status = 'active' ORDER BY site`)).
		WillReturnRows(rows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DISTINCT site, config FROM _kora_config_version WHERE status = 'Active'`)).
		WillReturnError(assertLegacyConfigMissingError{})

	sites, err := DiscoverSitesFromDB(db)
	if err != nil {
		t.Fatalf("DiscoverSitesFromDB: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("len(sites) = %d, want 1", len(sites))
	}
	if sites[0].Name != "partner" {
		t.Fatalf("Name = %q", sites[0].Name)
	}
	if sites[0].DBName != "partner" {
		t.Fatalf("DBName = %q", sites[0].DBName)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestReconstructSiteConfigFromDBInfoPrefersRegistryValues(t *testing.T) {
	common := &CommonConfig{
		DBType:     "mysql",
		DBHost:     "127.0.0.1",
		DBPort:     3306,
		DBUser:     "root",
		DBPassword: "rootpass",
	}

	cfg := ReconstructSiteConfigFromDBInfo(DBSiteInfo{
		Name:       "acme.kora.dev",
		Domains:    []string{"acme.kora.dev", "app.acme.dev"},
		DBType:     "mysql",
		DBHost:     "tenant-db.internal",
		DBPort:     3307,
		DBName:     "acme_prod",
		DBUser:     "tenant",
		DBPassword: "secret",
	}, common)

	if cfg.DBHost != "tenant-db.internal" {
		t.Fatalf("DBHost = %q", cfg.DBHost)
	}
	if cfg.DBPort != 3307 {
		t.Fatalf("DBPort = %d", cfg.DBPort)
	}
	if cfg.DBName != "acme_prod" {
		t.Fatalf("DBName = %q", cfg.DBName)
	}
	if cfg.DBUser != "tenant" {
		t.Fatalf("DBUser = %q", cfg.DBUser)
	}
	if cfg.DBPassword != "secret" {
		t.Fatalf("DBPassword = %q", cfg.DBPassword)
	}
}

type assertRegistryMissingError struct{}

func (assertRegistryMissingError) Error() string {
	return "no such table: _kora_site_registry"
}

type assertLegacyConfigMissingError struct{}

func (assertLegacyConfigMissingError) Error() string {
	return "no such table: _kora_config_version"
}
