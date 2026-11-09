package site

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
)

func TestClaimDatabaseOwner(t *testing.T) {
	createSQL := regexp.QuoteMeta("CREATE TABLE IF NOT EXISTS _kora_database_owner")
	selectSQL := regexp.QuoteMeta("SELECT tenant_id FROM _kora_database_owner WHERE id = 1")
	insertSQL := regexp.QuoteMeta("INSERT INTO _kora_database_owner (id, tenant_id) VALUES (1, ?)")
	tests := []struct {
		name    string
		expect  func(sqlmock.Sqlmock)
		wantErr error
	}{
		{
			name: "first tenant claims an unowned database",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectExec(createSQL).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery(selectSQL).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}))
				m.ExpectExec(insertSQL).WithArgs("tenant-a").WillReturnResult(sqlmock.NewResult(1, 1))
			},
		},
		{
			name: "owning tenant reloads",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectExec(createSQL).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery(selectSQL).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow("tenant-a"))
			},
		},
		{
			name: "second tenant is refused",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectExec(createSQL).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery(selectSQL).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow("tenant-b"))
			},
			wantErr: ErrDatabaseOwnedByAnotherTenant,
		},
		{
			name: "concurrent claim lost to another tenant is refused",
			expect: func(m sqlmock.Sqlmock) {
				m.ExpectExec(createSQL).WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery(selectSQL).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}))
				m.ExpectExec(insertSQL).WithArgs("tenant-a").WillReturnError(errors.New("UNIQUE constraint failed"))
				m.ExpectQuery(selectSQL).WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow("tenant-b"))
			},
			wantErr: ErrDatabaseOwnedByAnotherTenant,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			tt.expect(mock)
			err = ClaimDatabaseOwner(context.Background(), database, db.Resolve("libsql"), "tenant-a")
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ClaimDatabaseOwner() error = %v, want %v", err, tt.wantErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
