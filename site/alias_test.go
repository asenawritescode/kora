package site

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestInsertSiteAliasesNormalizesAndDeduplicates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
		WithArgs("pos.example.test", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT IGNORE INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
		WithArgs("register.example.test", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := insertSiteAliases(db, kdb.Resolve("mysql"), "site-1", "POS.EXAMPLE.TEST.", "pos.example.test", " Register.Example.Test "); err != nil {
		t.Fatalf("insertSiteAliases: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestResolveSiteAliasRejectsAmbiguousAlias(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	query := `SELECT site_id FROM _kora_site_alias WHERE alias = ? ORDER BY site_id LIMIT 2`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("shared.example.test").
		WillReturnRows(sqlmock.NewRows([]string{"site_id"}).AddRow("site-1").AddRow("site-2"))
	_, err = ResolveSiteAlias(db, "mysql", " SHARED.EXAMPLE.TEST. ")
	if err == nil || !regexp.MustCompile("multiple canonical sites").MatchString(err.Error()) {
		t.Fatalf("ResolveSiteAlias error = %v, want ambiguous alias error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}
