package configstore

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestManagementActivationFenceSchemaAcrossDialects(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "libsql"} {
		statements := ManagementActivationFenceSchema(db.Resolve(driver))
		if len(statements) != 1 || !containsAll(statements[0], "_kora_management_activation_fence", "operation_id", "expected_revision", "artifact_digest", "fence") {
			t.Fatalf("%s schema = %#v", driver, statements)
		}
	}
}

func TestRecordManagementActivationFenceAdvancesAndRejectsStaleIdentity(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := db.Resolve("postgres")
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	insert := "INSERT INTO _kora_management_activation_fence (site, operation_id, fence, expected_revision, artifact_digest) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (site, operation_id) DO NOTHING"
	update := "UPDATE _kora_management_activation_fence SET fence = $1, updated_at = CURRENT_TIMESTAMP WHERE site = $2 AND operation_id = $3 AND expected_revision = $4 AND artifact_digest = $5 AND fence < $6"
	query := "SELECT fence, expected_revision, artifact_digest FROM _kora_management_activation_fence WHERE site = $1 AND operation_id = $2 FOR UPDATE"
	setup := func(requestFence, requestRevision, storedFence, storedRevision int64, storedDigest string, updateCount int64, commit bool) {
		t.Helper()
		mock.ExpectBegin()
		mock.ExpectExec(regexp.QuoteMeta(insert)).WithArgs("tenant-a", "operation-a", requestFence, requestRevision, digest).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(regexp.QuoteMeta(update)).WithArgs(requestFence, "tenant-a", "operation-a", requestRevision, digest, requestFence).WillReturnResult(sqlmock.NewResult(0, updateCount))
		mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "operation-a").WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(storedFence, storedRevision, storedDigest))
		if commit {
			mock.ExpectCommit()
		} else {
			mock.ExpectRollback()
		}
	}

	setup(1, 7, 1, 7, digest, 1, true)
	if err := RecordManagementActivationFence(context.Background(), database, dialect, "tenant-a", "operation-a", 1, 7, digest); err != nil {
		t.Fatalf("record first claim: %v", err)
	}
	setup(3, 7, 3, 7, digest, 1, true)
	if err := RecordManagementActivationFence(context.Background(), database, dialect, "tenant-a", "operation-a", 3, 7, digest); err != nil {
		t.Fatalf("record newer claim: %v", err)
	}
	setup(2, 7, 3, 7, digest, 0, false)
	if err := RecordManagementActivationFence(context.Background(), database, dialect, "tenant-a", "operation-a", 2, 7, digest); !errors.Is(err, ErrManagementActivationFence) {
		t.Fatalf("stale claim error = %v", err)
	}
	setup(4, 8, 3, 7, digest, 0, false)
	if err := RecordManagementActivationFence(context.Background(), database, dialect, "tenant-a", "operation-a", 4, 8, digest); !errors.Is(err, ErrManagementActivationFence) {
		t.Fatalf("changed expected revision error = %v", err)
	}
	if err := RecordManagementActivationFence(context.Background(), database, dialect, "tenant-a", "operation-a", 4, 7, "not-a-digest"); err == nil {
		t.Fatal("invalid digest accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}

func TestValidateManagementActivationFenceLocksAndChecksExactClaim(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	query := "SELECT fence, expected_revision, artifact_digest FROM _kora_management_activation_fence WHERE site = $1 AND operation_id = $2 FOR UPDATE"
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-a", "operation-a").WillReturnRows(sqlmock.NewRows([]string{"fence", "expected_revision", "artifact_digest"}).AddRow(6, 9, digest))
	if err := ValidateManagementActivationFenceTx(context.Background(), tx, db.Resolve("postgres"), "tenant-a", "operation-a", 6, 9, digest); err != nil {
		t.Fatalf("validate current claim: %v", err)
	}
	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("SQL expectations:", err)
	}
}
