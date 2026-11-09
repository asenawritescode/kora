package db

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLAllocateNameNumberBootstrapsFromExistingRows(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(7))
	mock.ExpectExec("INSERT INTO _kora_naming_series").
		WithArgs("Customer", "CUST", int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT LAST_INSERT_ID\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"last_insert_id"}).AddRow(8))

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	got, err := (&MySQLDialect{}).AllocateNameNumber(tx, "Customer", "CUST", "tabCustomer")
	if err != nil {
		t.Fatalf("AllocateNameNumber: %v", err)
	}
	if got != 8 {
		t.Fatalf("allocated = %d, want 8", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPostgresAllocateNameNumberBootstrapsFromExistingRows(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(7))
	mock.ExpectQuery(`INSERT INTO "_kora_naming_series"`).
		WithArgs("Customer", "CUST", int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"current"}).AddRow(8))

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	got, err := (&PostgresDialect{}).AllocateNameNumber(tx, "Customer", "CUST", "tabCustomer")
	if err != nil {
		t.Fatalf("AllocateNameNumber: %v", err)
	}
	if got != 8 {
		t.Fatalf("allocated = %d, want 8", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestLibSQLAllocateNameNumberBootstrapsFromExistingRows(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer sqlDB.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(7))
	mock.ExpectExec(`INSERT INTO "_kora_naming_series"`).
		WithArgs("Customer", "CUST", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`UPDATE "_kora_naming_series"`).
		WithArgs("Customer", "CUST").
		WillReturnRows(sqlmock.NewRows([]string{"current"}).AddRow(8))

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	got, err := (&LibSQLDialect{}).AllocateNameNumber(tx, "Customer", "CUST", "tabCustomer")
	if err != nil {
		t.Fatalf("AllocateNameNumber: %v", err)
	}
	if got != 8 {
		t.Fatalf("allocated = %d, want 8", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
