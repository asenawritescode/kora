package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
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
	resource := contract.OwnedResource{Ref: contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1}, Access: contract.ModulePrivate, MutationMode: contract.MutationCommands, ScopeKind: contract.ScopeTenantShared}
	candidate := contract.ModuleSnapshot{TenantID: "tenant-a", Modules: []contract.ModuleDescriptor{{ID: "finance.ar", Version: "1.0.0", Resources: []contract.OwnedResource{resource}}}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: "finance.ar", Version: "1.0.0", ArtifactDigest: "digest"}}, Ownership: []contract.ResourceOwnership{{TenantID: "tenant-a", Resource: resource.Ref, OwnerModuleID: "finance.ar"}}}
	query := regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(query).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)`)).WithArgs("tenant-a", uint64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	if revision, err := store.CommitActivation(context.Background(), tx, 0, candidate); err != nil || revision != 1 {
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

func TestModuleStoreProviderSelectionsAreRevisioned(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, db.Resolve("libsql"))
	module := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: true}}}
	current := contract.ModuleSnapshot{TenantID: "tenant-a", Revision: 1, Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, ArtifactDigest: "artifact"}}}
	currentJSON, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	selection := contract.ProviderSelection{Key: contract.BindingKey{Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, ConsumerModuleID: module.ID, CapabilityName: "billing.issue"}, ProviderID: "billing-internal"}
	selectQuery := regexp.QuoteMeta(`SELECT revision, snapshot_json FROM _kora_module_snapshot WHERE tenant_id = ?`)
	mock.ExpectBegin()
	mock.ExpectQuery(selectQuery).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(currentJSON)))
	mock.ExpectQuery(selectQuery).WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"revision", "snapshot_json"}).AddRow(uint64(1), string(currentJSON)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_module_snapshot SET revision = ?, snapshot_json = ? WHERE tenant_id = ? AND revision = ?`)).WithArgs(uint64(2), sqlmock.AnyArg(), "tenant-a", uint64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	updated, err := store.CommitProviderSelections(context.Background(), "tenant-a", 1, []contract.ProviderSelection{selection})
	if err != nil || updated.Revision != 2 || len(updated.ProviderSelections) != 1 || updated.ProviderSelections[0].ProviderID != "billing-internal" {
		t.Fatalf("persist provider selection = %+v, %v", updated, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateModuleSnapshotRequiresSelectionsToMatchInstalledCapabilities(t *testing.T) {
	module := contract.ModuleDescriptor{ID: "sales.order", Version: "1.0.0", Requires: []contract.CapabilityRequirement{{Name: "billing.issue", Contract: contract.SchemaRef{ID: "billing.invoice", Version: 1}, Required: true}}}
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Modules: []contract.ModuleDescriptor{module}, Installations: []contract.ModuleInstallation{{TenantID: "tenant-a", ModuleID: module.ID, Version: module.Version, ArtifactDigest: "artifact"}}, ProviderSelections: []contract.ProviderSelection{{Key: contract.BindingKey{Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, ConsumerModuleID: module.ID, CapabilityName: "billing.issue"}, ProviderID: "billing.external"}}}
	if err := validateModuleSnapshot(snapshot); err != nil {
		t.Fatalf("valid persisted provider selection: %v", err)
	}
	snapshot.ProviderSelections = append(snapshot.ProviderSelections, snapshot.ProviderSelections[0])
	if err := validateModuleSnapshot(snapshot); err == nil {
		t.Fatal("duplicate persisted provider selection accepted")
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
