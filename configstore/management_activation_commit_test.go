package configstore

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func TestManagementModuleCommitKeepsSchemaDDLOutsideTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := NewStore(database, db.Resolve("postgres"))
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	mock.ExpectQuery("SELECT fence, expected_revision, artifact_digest").WithArgs("tenant-a", "operation-a").WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(4, 1, digest))
	mock.ExpectExec("UPDATE _kora_auth_revision").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE _kora_config_revision").WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	readFailure := errors.New("injected module snapshot read failure")
	// Any schema DDL after the revision write is unexpected: on MySQL it
	// would commit those writes before this later failure could roll back.
	mock.ExpectQuery("SELECT revision, snapshot_json FROM _kora_module_snapshot").WithArgs("tenant-a").WillReturnError(readFailure)
	_, _, err = store.CommitManagementActivation(context.Background(), tx, &doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{{ID: "owner.module", Version: "1.0.0"}}}, "tenant-a", db.Resolve("postgres"), ManagementActivationCommit{OperationID: "operation-a", JournalKind: "management_package", FromPhase: ActivationValidated, CreatedBy: "management", VersionLabel: "Apply package", Fence: 4, ExpectedRevision: 1, ArtifactDigest: digest, ModuleTenantID: "tenant-a"})
	if !errors.Is(err, readFailure) {
		t.Fatalf("expected module read failure without intervening DDL, got %v", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitManagementActivationVersionFailureRequiresTransactionRollback(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
	store := NewStore(database, dialect)
	digest := strings.Repeat("a", 64)

	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal("begin activation transaction:", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fence, expected_revision, artifact_digest FROM _kora_management_activation_fence WHERE site = $1 AND operation_id = $2 FOR UPDATE")).
		WithArgs("tenant-a", "operation-a").
		WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(4, 1, digest))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_auth_revision SET policy_revision = policy_revision + 1 WHERE id = 1")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_config_revision SET revision = revision + 1 WHERE id = 1 AND revision = $1 AND revision < 9223372036854775807")).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(MAX(version), 0) FROM _kora_config_version WHERE site = $1")).
		WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE _kora_config_version SET status = 'Superseded', is_active = $1 WHERE site = $2 AND status = 'Active'")).
		WithArgs(0, "tenant-a").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT config, version FROM _kora_config_version WHERE site = $1 AND status = 'Active' AND version <= $2 ORDER BY version DESC LIMIT 1")).
		WithArgs("tenant-a", 0).WillReturnRows(sqlmock.NewRows([]string{"config", "version"}))
	versionInsert := "INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, changelog, status, is_active, config, change_list, config_hash, base_version_id, min_kora_version)"
	mock.ExpectExec(regexp.QuoteMeta(versionInsert)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(errors.New("config history storage unavailable"))

	_, _, err = store.CommitManagementActivation(context.Background(), tx, &doctype.ConfigSnapshot{}, "tenant-a", dialect, ManagementActivationCommit{
		OperationID: "operation-a", JournalKind: "management_package", FromPhase: ActivationValidated,
		CreatedBy: "management", VersionLabel: "Apply retail package", Fence: 4,
		ExpectedRevision: 1, ArtifactDigest: digest,
	})
	if err == nil || !strings.Contains(err.Error(), "recording management activation config version") {
		t.Fatalf("CommitManagementActivation() error = %v, want version-history failure", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal("rollback incomplete activation transaction:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
