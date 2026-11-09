package configstore

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestActivationJournalSchemaSupportsEveryDialect(t *testing.T) {
	for _, driver := range []string{"mysql", "postgres", "libsql"} {
		t.Run(driver, func(t *testing.T) {
			statements := ActivationJournalSchema(db.Resolve(driver))
			if len(statements) != 1 || !containsAll(statements[0], "_kora_config_operation", "target_snapshot", "ddl_plan", "recovery_plan", "last_error") {
				t.Fatalf("activation journal schema for %s: %#v", driver, statements)
			}
		})
	}
}

func TestEnsureActivationOperationResumesSameSnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := db.MySQL()
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Invoice"}}}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("INSERT IGNORE INTO _kora_config_operation").
		WithArgs("acme", "version-1", "activate", ActivationRequested, string(encoded)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT site, version_id, kind, phase, target_snapshot").
		WithArgs("acme", "version-1", "activate").
		WillReturnRows(sqlmock.NewRows([]string{"site", "version_id", "kind", "phase", "target_snapshot", "ddl_plan", "recovery_plan", "last_error"}).
			AddRow("acme", "version-1", "activate", ActivationApplying, string(encoded), "[]", "retry", "DDL interrupted"))

	op, err := EnsureActivationOperation(database, dialect, "acme", "version-1", "activate", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if op.Phase != ActivationApplying || op.LastError != "DDL interrupted" {
		t.Fatalf("resumed operation = %+v", op)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureActivationOperationRejectsChangedRetrySnapshot(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	dialect := db.MySQL()
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Invoice"}}}
	different := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Receipt"}}}
	existing, err := json.Marshal(different)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec("INSERT IGNORE INTO _kora_config_operation").
		WithArgs("acme", "version-1", "activate", ActivationRequested, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT site, version_id, kind, phase, target_snapshot").
		WithArgs("acme", "version-1", "activate").
		WillReturnRows(sqlmock.NewRows([]string{"site", "version_id", "kind", "phase", "target_snapshot", "ddl_plan", "recovery_plan", "last_error"}).
			AddRow("acme", "version-1", "activate", ActivationApplying, string(existing), "", "", ""))
	if _, err := EnsureActivationOperation(database, dialect, "acme", "version-1", "activate", snapshot); err == nil {
		t.Fatal("expected changed target under an existing operation identity to fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateActivationOperationRequiresExistingRow(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectExec("UPDATE _kora_config_operation SET phase").
		WithArgs(ActivationValidated, "[]", "resume", "failure", "acme", "version-1", "activate", ActivationActivated, ActivationCompleted, ActivationValidated, ActivationCompleted, ActivationActivated).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT phase FROM _kora_config_operation").
		WithArgs("acme", "version-1", "activate").
		WillReturnRows(sqlmock.NewRows([]string{"phase"}))
	if err := UpdateActivationOperation(database, db.MySQL(), "acme", "version-1", "activate", ActivationValidated, "[]", "resume", "failure"); err == nil {
		t.Fatal("missing journal update returned success")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateActivationOperationPhaseTxCommitsWithCallerTransaction(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(`UPDATE _kora_config_operation SET phase = \?, recovery_plan = \?, last_error = '', updated_at = CURRENT_TIMESTAMP WHERE site = \? AND version_id = \? AND kind = \? AND phase = \?`).
		WithArgs(ActivationValidated, "version-id", "acme", "operation-id", "import", ActivationApplying).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := UpdateActivationOperationPhaseTx(tx, db.Resolve("mysql"), "acme", "operation-id", "import", ActivationApplying, ActivationValidated, "version-id"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateActivationOperationPhaseTxRejectsStalePhase(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectBegin()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(`UPDATE _kora_config_operation SET phase = \?, recovery_plan = \?, last_error = '', updated_at = CURRENT_TIMESTAMP WHERE site = \? AND version_id = \? AND kind = \? AND phase = \?`).
		WithArgs(ActivationValidated, "version-id", "acme", "operation-id", "import", ActivationApplying).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := UpdateActivationOperationPhaseTx(tx, db.Resolve("mysql"), "acme", "operation-id", "import", ActivationApplying, ActivationValidated, "version-id"); err == nil {
		t.Fatal("stale phase update succeeded")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
