package site

import (
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestBootstrapSystemTablesReturnsDefaultRoleSeedFailure(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	seedFailure := errors.New("role table unavailable")
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')")).
		WillReturnError(seedFailure)

	if err := seedDefaultAdministratorRole(database, db.Resolve("mysql")); !errors.Is(err, seedFailure) {
		t.Fatalf("seedDefaultAdministratorRole error = %v, want wrapped seed failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
