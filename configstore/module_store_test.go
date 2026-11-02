package configstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestCapabilityLeaseAdmissionAndRevocationAreEpochFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}, CallerModuleID: "sales.orders", ProviderID: "finance.billing", Operation: "invoice.issue", BindingRevision: 4, GrantEpoch: 3, ExpiresAt: now.Add(time.Hour), State: "active"}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_capability_lease (tenant_id, lease_id, scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref, revoked_by, revocation_reason, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)).WithArgs("tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "invoice.issue", uint64(4), uint64(3), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", uint64(0), "", "", "", "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.CreateCapabilityLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`)).WithArgs("tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "invoice.issue", uint64(4), uint64(3), now.Format(capabilityLeaseTimeLayout), "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.AdmitCapabilityAttempt(context.Background(), lease, now); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active'`)).WithArgs("security-admin", "credential compromised", now.Format(capabilityLeaseTimeLayout), "tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "finance.billing", "invoice.issue", uint64(4), uint64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.RevokeCapabilityLease(context.Background(), lease, "security-admin", "credential compromised", now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeCapabilitiesIsScopeAndRevisionFenced(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	request := contract.RevokeCapability{Scope: scope, Selector: contract.CapabilitySelector{CallerModuleID: "sales.orders", ProviderID: "billing.provider", Operation: "issue"}, ExpectedRevision: 4, Reason: "provider credential compromised"}
	scopeJSON, _ := json.Marshal(scope)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND state = 'active'`)).WithArgs("security-admin", request.Reason, now.Format(capabilityLeaseTimeLayout), "tenant-a", string(scopeJSON), "sales.orders", "billing.provider", "issue", uint64(4)).WillReturnResult(sqlmock.NewResult(0, 2))
	count, err := store.RevokeCapabilities(context.Background(), request, "security-admin", now)
	if err != nil || count != 2 {
		t.Fatalf("scope revocation = %d, %v; want two active matching leases", count, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseSchemaUsesTransactionalMySQLTable(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	mock.ExpectExec(regexp.QuoteMeta(`CREATE TABLE IF NOT EXISTS _kora_capability_lease (tenant_id VARCHAR(140) NOT NULL, lease_id VARCHAR(160) NOT NULL, scope_json TEXT NOT NULL, caller_module_id VARCHAR(160) NOT NULL, provider_id VARCHAR(160) NOT NULL, operation_name VARCHAR(160) NOT NULL, binding_revision BIGINT NOT NULL, grant_epoch BIGINT NOT NULL, expires_at VARCHAR(40) NOT NULL, state VARCHAR(16) NOT NULL, admitted_attempts BIGINT NOT NULL, credential_ref VARCHAR(320) NOT NULL, revoked_by VARCHAR(160) NOT NULL, revocation_reason VARCHAR(1000) NOT NULL, revoked_at VARCHAR(40) NOT NULL, PRIMARY KEY (tenant_id, lease_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.EnsureCapabilityLeaseSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseValidationRejectsStaleOrExpiredGrant(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-2", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderID: "billing", Operation: "issue", BindingRevision: 8, GrantEpoch: 2, ExpiresAt: now.Add(time.Hour), State: "active"}
	query := regexp.QuoteMeta(`SELECT scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`)
	scopeJSON, _ := json.Marshal(lease.Scope)
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-2").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "billing", "issue", uint64(8), uint64(2), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", ""))
	if err := store.ValidateCapabilityLease(context.Background(), lease, now); err != nil {
		t.Fatalf("valid durable lease rejected: %v", err)
	}
	lease.GrantEpoch++
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-2").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "billing", "issue", uint64(8), uint64(2), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "active", ""))
	if err := store.ValidateCapabilityLease(context.Background(), lease, now); err == nil {
		t.Fatal("stale grant epoch accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCapabilityLeasePreservesRevokedGrantForRecovery(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-3", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 4, ExpiresAt: now.Add(time.Hour), State: "active"}
	query := regexp.QuoteMeta(`SELECT scope_json, caller_module_id, provider_id, operation_name, binding_revision, grant_epoch, expires_at, state, admitted_attempts, credential_ref FROM _kora_capability_lease WHERE tenant_id = ? AND lease_id = ?`)
	scopeJSON, _ := json.Marshal(lease.Scope)
	mock.ExpectQuery(query).WithArgs("tenant-a", "lease-3").WillReturnRows(sqlmock.NewRows([]string{"scope_json", "caller_module_id", "provider_id", "operation_name", "binding_revision", "grant_epoch", "expires_at", "state", "admitted_attempts", "credential_ref"}).AddRow(string(scopeJSON), "sales.orders", "billing", "issue", uint64(2), uint64(5), lease.ExpiresAt.Format(capabilityLeaseTimeLayout), "revoked", uint64(7), ""))
	if err := store.EnsureCapabilityLease(context.Background(), lease); err != nil {
		t.Fatalf("revoked lease should not prevent reconciliation runtime startup: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseAdmissionPersistsExpiry(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.LibSQLDialect{})
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := contract.CapabilityLease{ID: "lease-expired", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderID: "billing", Operation: "issue", BindingRevision: 2, GrantEpoch: 1, ExpiresAt: now.Add(-time.Second), State: "active"}
	scopeJSON, _ := json.Marshal(lease.Scope)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET admitted_attempts = admitted_attempts + 1 WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at > ? AND credential_ref = ?`)).WithArgs("tenant-a", "lease-expired", string(scopeJSON), "sales.orders", "billing", "issue", uint64(2), uint64(1), now.Format(capabilityLeaseTimeLayout), "").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET state = 'expired' WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active' AND expires_at <= ? AND credential_ref = ?`)).WithArgs("tenant-a", "lease-expired", string(scopeJSON), "sales.orders", "billing", "issue", uint64(2), uint64(1), now.Format(capabilityLeaseTimeLayout), "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.AdmitCapabilityAttempt(context.Background(), lease, now); err == nil {
		t.Fatal("expired capability lease admitted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityLeaseRejectsInvalidScopeAndStaleRevocation(t *testing.T) {
	invalid := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-b", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderID: "billing", Operation: "issue", BindingRevision: 1, GrantEpoch: 1, ExpiresAt: time.Now().Add(time.Hour), State: "active"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("cross-tenant capability lease accepted")
	}
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewModuleStore(database, &db.MySQLDialect{})
	stale := contract.CapabilityLease{ID: "lease-1", TenantID: "tenant-a", Scope: contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindTenant}, CallerModuleID: "sales.orders", ProviderID: "billing", Operation: "issue", BindingRevision: 1, GrantEpoch: 2, ExpiresAt: time.Now().Add(time.Hour), State: "active"}
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_capability_lease SET grant_epoch = grant_epoch + 1, state = 'revoked', revoked_by = ?, revocation_reason = ?, revoked_at = ? WHERE tenant_id = ? AND lease_id = ? AND scope_json = ? AND caller_module_id = ? AND provider_id = ? AND operation_name = ? AND binding_revision = ? AND grant_epoch = ? AND state = 'active'`)).WithArgs("admin", "revoke", sqlmock.AnyArg(), "tenant-a", "lease-1", sqlmock.AnyArg(), "sales.orders", "billing", "issue", uint64(1), uint64(2)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := store.RevokeCapabilityLease(context.Background(), stale, "admin", "revoke", time.Now().UTC()); err == nil {
		t.Fatal("stale revocation accepted")
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

func TestValidateModuleSnapshotRejectsInvalidInstalledPackageComposition(t *testing.T) {
	snapshot := contract.ModuleSnapshot{TenantID: "tenant-a", Packages: []contract.InstalledPackage{
		{PackageID: "sales", ReleaseID: "sales-1", Version: "1.0.0", Digest: "sales-digest", Requires: []contract.ManifestPackRef{{PackageID: "billing", ReleaseID: "billing-1"}}},
	}}
	if err := validateModuleSnapshot(snapshot); err == nil {
		t.Fatal("module snapshot with missing package requirement accepted")
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
