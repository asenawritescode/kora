package configstore

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
)

func TestModuleStoreActivationIsAtomicAndRevisionFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, db.Resolve("libsql"))
	candidate := contract.ModuleSnapshot{TenantID: "tenant-a", Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", Version: "1.0.0", ArtifactDigest: "digest"}}, Ownership: []contract.ResourceOwnership{{TenantID: "tenant-a", Resource: contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}, OwnerModuleID: "finance.ar"}}}
	query := regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(query).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`)).WithArgs("tenant-a", uint64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	if revision, err := store.CommitActivation(tx, 0, candidate); err != nil || revision != 1 {
		t.Fatalf("revision=%d err=%v", revision, err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestModuleStoreSchemaUsesSupportedDialectTypes(t *testing.T) {
	for _, name := range []string{"mysql", "postgres", "libsql"} {
		statement := NewModuleStore(nil, db.Resolve(name)).SchemaStatement()
		for _, required := range []string{"_kora_module_snapshot", "tenant_id", "revision", "snapshot_json"} {
			if !strings.Contains(statement, required) {
				t.Fatalf("%s schema missing %s: %s", name, required, statement)
			}
		}
		if name == "mysql" && !strings.Contains(statement, "LONGTEXT") {
			t.Fatalf("mysql snapshot must use LONGTEXT: %s", statement)
		}
	}
}

var _ *sql.DB
