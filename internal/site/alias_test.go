package site

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/internal/db"
)

func TestInsertSiteAliasesNormalizesAndDeduplicates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
		WithArgs("pos.example.test", "site-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
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
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"initialized"}).AddRow(1))
	query := `SELECT site_id FROM _kora_site_alias WHERE alias = ? ORDER BY site_id LIMIT 2`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("shared.example.test").
		WillReturnRows(sqlmock.NewRows([]string{"site_id"}).AddRow("site-1").AddRow("site-2"))
	_, err = ResolveSiteAlias(context.Background(), db, "mysql", " SHARED.EXAMPLE.TEST. ")
	if err == nil || !regexp.MustCompile("multiple canonical sites").MatchString(err.Error()) {
		t.Fatalf("ResolveSiteAlias error = %v, want ambiguous alias error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestResolveSiteAliasFailsClosedWhenProjectionIsDirty(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT initialized FROM _kora_site_alias_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"initialized"}).AddRow(0))
	if _, err := ResolveSiteAlias(context.Background(), database, "mysql", "tenant.example.test"); !errors.Is(err, ErrSiteAliasIndexUnavailable) {
		t.Fatalf("ResolveSiteAlias error = %v, want ErrSiteAliasIndexUnavailable", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureUniqueSiteAliasIndexRejectsLegacyOwnershipConflicts(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	query := `SELECT alias, COUNT(DISTINCT site_id) FROM _kora_site_alias GROUP BY alias HAVING COUNT(DISTINCT site_id) > 1 ORDER BY alias LIMIT 1`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WillReturnRows(
		sqlmock.NewRows([]string{"alias", "count"}).AddRow("shared.example.test", 2),
	)
	err = ensureUniqueSiteAliasIndex(context.Background(), database, kdb.Resolve("mysql"))
	if err == nil || !strings.Contains(err.Error(), `site alias "shared.example.test" has 2 tenant owners`) {
		t.Fatalf("ensureUniqueSiteAliasIndex error = %v, want explicit legacy ownership conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
