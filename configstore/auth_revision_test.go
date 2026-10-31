package configstore

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestBumpAuthorizationRevisionSharesMutationTransaction(t *testing.T) {
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
	mock.ExpectExec(`UPDATE _kora_auth_revision SET policy_revision = policy_revision \+ 1, membership_revision = membership_revision \+ 1 WHERE id = 1`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := BumpAuthorizationRevisionTx(tx, db.Resolve("mysql"), true, true); err != nil {
		t.Fatal("bump both epochs:", err)
	}
	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatal("commit mutation:", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBumpAuthorizationRevisionFailsClosedWhenStateMissing(t *testing.T) {
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
	mock.ExpectExec(`UPDATE _kora_auth_revision SET membership_revision = membership_revision \+ 1 WHERE id = 1`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := BumpAuthorizationRevisionTx(tx, db.Resolve("mysql"), false, true); err == nil {
		t.Fatal("missing revision row was accepted")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("caller rollback: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
