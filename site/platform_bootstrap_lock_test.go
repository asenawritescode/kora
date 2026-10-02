package site

import (
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
)

func TestPlatformBootstrapAdvisoryLockIsReleasedAndRestoresSingleConnectionPool(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create SQL mock:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT DATABASE()`)).
		WillReturnRows(sqlmock.NewRows([]string{"DATABASE()"}).AddRow("kora_platform"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT GET_LOCK(?, 55)`)).
		WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"GET_LOCK"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT RELEASE_LOCK(?)`)).
		WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"RELEASE_LOCK"}).AddRow(1))
	release, err := acquirePlatformRegistryBootstrapLock(database, db.Resolve("mysql"))
	if err != nil {
		t.Fatal("acquire MySQL platform bootstrap lock:", err)
	}
	if err := release(); err != nil {
		t.Fatal("release MySQL platform bootstrap lock:", err)
	}
	if got := database.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("platform max-open after release = %d, want restored 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet SQL expectations:", err)
	}
}
