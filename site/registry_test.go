package site

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSQLSiteRegistrySetStatusAdvancesRevision(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry SET status = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ?`)).
		WithArgs("suspended", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site, COALESCE(runtime_cell_id, ''), COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`)).
		WithArgs("site-1").WillReturnRows(sqlmock.NewRows([]string{"site", "runtime_cell_id", "domains_json", "status", "config_revision"}).AddRow("demo", "", `[]`, "suspended", 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(sqlmock.AnyArg(), uint64(1), "site-1", uint64(2), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	registry := NewSQLSiteRegistry(database, "mysql")
	descriptor, err := registry.SetStatusAndGetDescriptor("site-1", "SUSPENDED")
	if err != nil {
		t.Fatalf("SetStatusAndGetDescriptor: %v", err)
	}
	if descriptor.SiteID != "site-1" || descriptor.Status != "suspended" || descriptor.ConfigRevision != 2 || len(descriptor.Aliases) != 1 || descriptor.Aliases[0] != "demo" {
		t.Fatalf("status update returned incorrect routing descriptor: %#v", descriptor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryRollsBackStatusWhenOutboxAppendFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry SET status = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ?`)).
		WithArgs("suspended", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site, COALESCE(runtime_cell_id, ''), COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`)).
		WithArgs("site-1").WillReturnRows(sqlmock.NewRows([]string{"site", "runtime_cell_id", "domains_json", "status", "config_revision"}).AddRow("demo", "", `[]`, "suspended", 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WillReturnError(errors.New("outbox unavailable"))
	mock.ExpectRollback()
	if err := NewSQLSiteRegistry(database, "mysql").SetStatus("site-1", "suspended"); err == nil {
		t.Fatal("expected failed outbox append to abort status update")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryRejectsUnknownStatus(t *testing.T) {
	registry := NewSQLSiteRegistry(&sql.DB{}, "mysql")
	if err := registry.SetStatus("site-1", "unknown"); err == nil {
		t.Fatal("expected invalid status error")
	}
}

func TestSQLSiteRegistryChangesAfterReturnsVersionedDescriptor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	query := `SELECT directory_revision, id, payload, created_at FROM _kora_site_directory_outbox WHERE directory_revision > ? ORDER BY directory_revision LIMIT ?`
	created := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(uint64(0), 10).WillReturnRows(sqlmock.NewRows([]string{"directory_revision", "id", "payload", "created_at"}).AddRow(1, "01K6A2G3Q00000000000000000", `{"version":1,"site_id":"site-1","aliases":["demo.example"],"status":"active","config_revision":3}`, created))
	changes, err := NewSQLSiteRegistry(database, "mysql").ChangesAfter(0, 10)
	if err != nil {
		t.Fatalf("ChangesAfter: %v", err)
	}
	if len(changes) != 1 || changes[0].Cursor != 1 || changes[0].Descriptor.SiteID != "site-1" || changes[0].Descriptor.ConfigRevision != 3 {
		t.Fatalf("unexpected changes: %+v", changes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryChangesAfterBoundsPageSize(t *testing.T) {
	registry := NewSQLSiteRegistry(&sql.DB{}, "mysql")
	if _, err := registry.ChangesAfter(0, 501); err == nil {
		t.Fatal("expected oversized page to be rejected")
	}
}

func TestSQLSiteRegistryLoadsAndAdvancesDurableConsumerCursor(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_directory_consumers (consumer_id, directory_revision) VALUES (?, ?)`)).
		WithArgs("engine:test:123", uint64(6)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_consumers WHERE consumer_id = ?`)).
		WithArgs("engine:test:123").WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(7))
	registry := NewSQLSiteRegistry(database, "mysql")
	cursor, err := registry.LoadConsumerCursor("engine:test:123", 6)
	if err != nil || cursor != 7 {
		t.Fatalf("LoadConsumerCursor = %d, %v", cursor, err)
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_consumers SET directory_revision = ?, updated_at = CURRENT_TIMESTAMP WHERE consumer_id = ? AND lease_owner = ? AND lease_expires_at > CURRENT_TIMESTAMP AND directory_revision = ?`)).
		WithArgs(uint64(8), "engine:test:123", "owner:test:123", uint64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := registry.AdvanceConsumerCursor("engine:test:123", "owner:test:123", 7, 8); err != nil {
		t.Fatalf("AdvanceConsumerCursor: %v", err)
	}
	if err := registry.AdvanceConsumerCursor("engine:test:123", "owner:test:123", 7, 9); err == nil {
		t.Fatal("expected a multi-revision cursor advance to be rejected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryPrunesOnlyHistoryAppliedBySlowestConsumer(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(MAX(directory_revision), 0) FROM _kora_site_directory_outbox WHERE created_at < ?`)).
		WithArgs(cutoff).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(10))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(12))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT MIN(directory_revision) FROM _kora_site_directory_consumers WHERE updated_at >= ?`)).
		WillReturnRows(sqlmock.NewRows([]string{"min"}).AddRow(8))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM _kora_site_directory_outbox WHERE directory_revision <= ? AND created_at < ?`)).
		WithArgs(uint64(8), cutoff).WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectCommit()
	deleted, err := NewSQLSiteRegistry(database, "mysql").PruneOutboxBefore(cutoff)
	if err != nil || deleted != 4 {
		t.Fatalf("PruneOutboxBefore = %d, %v", deleted, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryGetByIDReturnsInactiveSites(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("site-1").WillReturnRows(sqlmock.NewRows([]string{
		"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision",
	}).AddRow("site-1", "cell-a", "demo", "mysql", "db", 3306, "demo", "user", "", 0, `["demo.example"]`, "local", "", "suspended", 9))
	info, err := NewSQLSiteRegistry(database, "mysql").GetByID("site-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if info.Status != "suspended" || info.ConfigRevision != 9 || info.RuntimeCellID != "cell-a" {
		t.Fatalf("got status=%q revision=%d cell=%q", info.Status, info.ConfigRevision, info.RuntimeCellID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryGetSnapshotForCellFiltersBeforeLoadingCredentials(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE status = 'active' AND runtime_cell_id = ? ORDER BY site`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("cell-a").WillReturnRows(sqlmock.NewRows([]string{
		"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision",
	}).AddRow("site-1", "cell-a", "a.example", "mysql", "db-a", 3306, "tenant-a", "user-a", "password-a", 0, `[]`, "local", "", "active", 1))
	sites, err := NewSQLSiteRegistry(database, "mysql").GetSnapshotForCell("cell-a")
	if err != nil {
		t.Fatal("GetSnapshotForCell:", err)
	}
	if len(sites) != 1 || sites[0].RuntimeCellID != "cell-a" || sites[0].DBName != "tenant-a" {
		t.Fatalf("unexpected assigned snapshot: %+v", sites)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistrySetRuntimeCellIDWritesRevisionedChange(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry SET runtime_cell_id = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ?`)).
		WithArgs("cell-a", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`)).
		WithArgs("site-1").WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "domains_json", "status", "config_revision"}).AddRow("site-1", "cell-a", "demo", `["demo.example"]`, "active", 3))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(8))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(sqlmock.AnyArg(), uint64(8), "site-1", uint64(3), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	descriptor, err := NewSQLSiteRegistry(database, "mysql").SetRuntimeCellID("site-1", " cell-a ")
	if err != nil {
		t.Fatal("SetRuntimeCellID:", err)
	}
	if descriptor.RuntimeCellID != "cell-a" || descriptor.SiteID != "site-1" || descriptor.ConfigRevision != 3 {
		t.Fatalf("unexpected assignment descriptor: %+v", descriptor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLSiteRegistryDirectoryDescriptorsAreCredentialFree(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry ORDER BY site`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(sqlmock.NewRows([]string{
		"site_id", "runtime_cell_id", "site", "domains_json", "status", "config_revision",
	}).AddRow("site-1", "cell-a", "POS.Example.", `["POS.Example.","pos-alt.example"]`, "suspended", 12))
	descriptors, err := NewSQLSiteRegistry(database, "mysql").GetDirectoryDescriptors()
	if err != nil {
		t.Fatal("GetDirectoryDescriptors:", err)
	}
	if len(descriptors) != 1 {
		t.Fatalf("descriptor count = %d", len(descriptors))
	}
	descriptor := descriptors[0]
	if descriptor.SiteID != "site-1" || descriptor.Status != "suspended" || descriptor.ConfigRevision != 12 || descriptor.RuntimeCellID != "cell-a" {
		t.Fatalf("unexpected descriptor: %#v", descriptor)
	}
	if len(descriptor.Aliases) != 2 || descriptor.Aliases[0] != "pos.example" || descriptor.Aliases[1] != "pos-alt.example" {
		t.Fatalf("aliases not normalized: %#v", descriptor.Aliases)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal("marshal descriptor:", err)
	}
	for _, forbidden := range []string{"db_password", "db_user", "db_host", "db_name"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("descriptor exposed database field %q: %s", forbidden, encoded)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

const registrySelect = `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE status = 'active' ORDER BY site`

func TestDiscoverSitesFromDBUsesRegistryWhenAvailable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision",
	}).AddRow(
		"site_0123456789abcdef0123456789abcdef", "cell-a", "acme.kora.dev", "mysql", "db.internal", 3306, "acme_kora_dev", "tenant_user", "", 0, `["acme.kora.dev","app.acme.dev"]`, "local", "", "active", 7,
	)
	mock.ExpectQuery(regexp.QuoteMeta(registrySelect)).
		WillReturnRows(rows)

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
	if sites[0].SiteID != "site_0123456789abcdef0123456789abcdef" {
		t.Fatalf("SiteID = %q", sites[0].SiteID)
	}
	if sites[0].ConfigRevision != 7 || sites[0].Status != "active" {
		t.Fatalf("revision/status = %d/%q", sites[0].ConfigRevision, sites[0].Status)
	}
	if sites[0].DBName != "acme_kora_dev" {
		t.Fatalf("DBName = %q", sites[0].DBName)
	}
	if len(sites[0].Domains) != 2 {
		t.Fatalf("Domains len = %d", len(sites[0].Domains))
	}
	if sites[0].FileStorage != "local" {
		t.Fatalf("FileStorage = %q", sites[0].FileStorage)
	}
	if sites[0].StorageBucket != "" {
		t.Fatalf("StorageBucket = %q", sites[0].StorageBucket)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestDiscoverSitesFromDBDoesNotFallBackWhenRegistryIsMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(registrySelect)).
		WillReturnError(assertRegistryMissingError{})

	if sites, err := DiscoverSitesFromDB(db); err == nil {
		t.Fatalf("DiscoverSitesFromDB returned %d sites without the canonical registry", len(sites))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestDiscoverSitesFromDBUsesOnlyRegistry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision",
	}).AddRow(
		"site_abcdef0123456789abcdef0123456789", "", "partner", "mysql", "kora-mysql-lh5l6r", 3306, "partner", "root", "", 0, `["partner"]`, "local", "", "active", 1,
	)
	mock.ExpectQuery(regexp.QuoteMeta(registrySelect)).
		WillReturnRows(rows)
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
		DBType:         "mysql",
		DBHost:         "127.0.0.1",
		DBPort:         3306,
		DBUser:         "root",
		DBPassword:     "rootpass",
		DBMaxOpenConns: 7,
		DBMaxIdleConns: 0,
	}

	cfg := ReconstructSiteConfigFromDBInfo(DBSiteInfo{
		Name:        "acme.kora.dev",
		Domains:     []string{"acme.kora.dev", "app.acme.dev"},
		DBType:      "mysql",
		DBHost:      "tenant-db.internal",
		DBPort:      3307,
		DBName:      "acme_prod",
		DBUser:      "tenant",
		DBPassword:  "secret",
		FileStorage: "s3",
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
	if cfg.DBMaxOpenConns != 7 || cfg.DBMaxIdleConns != 0 {
		t.Fatalf("tenant pool bounds = open %d / idle %d, want open 7 / idle 0", cfg.DBMaxOpenConns, cfg.DBMaxIdleConns)
	}
	if cfg.FileStorage != "s3" {
		t.Fatalf("FileStorage = %q", cfg.FileStorage)
	}
	if cfg.StorageBucket != BucketNameForSite("acme.kora.dev") {
		t.Fatalf("StorageBucket = %q", cfg.StorageBucket)
	}
}

func TestBucketNameForSite(t *testing.T) {
	if got := BucketNameForSite("Acme.Kora.Dev"); got != "acme.kora.dev" {
		t.Fatalf("BucketNameForSite = %q", got)
	}
	if got := BucketNameForSite("site with spaces"); got != "site-with-spaces" {
		t.Fatalf("BucketNameForSite = %q", got)
	}
}

func TestSiteDescriptorIsCredentialFreeAndNormalizesAliases(t *testing.T) {
	descriptor := DescriptorFromDBSiteInfo(DBSiteInfo{
		SiteID:         "site_0123456789abcdef0123456789abcdef",
		Name:           " Acme.Example.com. ",
		Domains:        []string{"ACME.EXAMPLE.COM", " POS.Example.com. "},
		Status:         "active",
		ConfigRevision: 9,
		DBPassword:     "must-not-leak",
		DBUser:         "tenant-user",
	})
	if len(descriptor.Aliases) != 2 || descriptor.Aliases[0] != "acme.example.com" || descriptor.Aliases[1] != "pos.example.com" {
		t.Fatalf("unexpected aliases: %#v", descriptor.Aliases)
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	if strings.Contains(string(encoded), "must-not-leak") || strings.Contains(string(encoded), "tenant-user") || strings.Contains(string(encoded), "db_password") {
		t.Fatalf("descriptor leaked internal connection settings: %s", encoded)
	}
}

func TestNewCanonicalSiteIDIsOpaqueAndUnique(t *testing.T) {
	first, err := newCanonicalSiteID()
	if err != nil {
		t.Fatalf("newCanonicalSiteID: %v", err)
	}
	second, err := newCanonicalSiteID()
	if err != nil {
		t.Fatalf("newCanonicalSiteID: %v", err)
	}
	if !strings.HasPrefix(first, "site_") || len(first) != 37 || first == second {
		t.Fatalf("unexpected canonical ids: %q and %q", first, second)
	}
}

func TestUpdatePlatformSiteRegistrationUpdatesStorageSettings(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	metadataQuery := `INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, ?) ON DUPLICATE KEY UPDATE initialized = VALUES(initialized)`
	mock.ExpectExec(regexp.QuoteMeta(metadataQuery)).WithArgs(0).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry
			 SET file_storage = ?, storage_bucket = ?, domains_json = ?, status = 'active', config_revision = config_revision + 1, updated_at = ?
			 WHERE site = ?`)).
		WithArgs("s3", "kora-cms", `["kora-cms","app.kora.dev"]`, sqlmock.AnyArg(), "kora-cms").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site = ?`)).
		WithArgs("kora-cms").WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "domains_json", "status", "config_revision"}).AddRow("site-1", "", "kora-cms", `["kora-cms","app.kora.dev"]`, "active", 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(sqlmock.AnyArg(), uint64(1), "site-1", uint64(2), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id FROM _kora_site_registry WHERE site = ?`)).
		WithArgs("kora-cms").WillReturnRows(sqlmock.NewRows([]string{"site_id"}).AddRow("site-1"))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM _kora_site_alias WHERE site_id = ?`)).
		WithArgs("site-1").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
		WithArgs("kora-cms", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
		WithArgs("app.kora.dev", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta(metadataQuery)).WithArgs(1).WillReturnResult(sqlmock.NewResult(1, 1))

	if err := UpdatePlatformSiteRegistration(db, "mysql", "kora-cms", []string{"kora-cms", "app.kora.dev"}, "s3", ""); err != nil {
		t.Fatalf("UpdatePlatformSiteRegistration: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

type assertRegistryMissingError struct{}

func (assertRegistryMissingError) Error() string {
	return "no such table: _kora_site_registry"
}
