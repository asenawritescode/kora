package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/extension"
)

func newMockStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return New(database, db.Resolve("mysql")), mock
}

func TestVerifyAccessToken(t *testing.T) {
	tests := []struct {
		name    string
		rows    *sqlmock.Rows
		want    extension.Credential
		wantErr error
	}{
		{"active", sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("crm", `[]`), extension.Credential{Name: "crm", APIPermissions: `[]`}, nil},
		{"unknown", sqlmock.NewRows([]string{"name", "api_permissions"}), extension.Credential{}, extension.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, mock := newMockStore(t)
			mock.ExpectQuery(`SELECT name, api_permissions FROM _kora_extension WHERE site = \? AND access_token_hash = \? AND is_active = 1`).
				WithArgs("acme", "hash").WillReturnRows(tt.rows)
			got, err := store.VerifyAccessToken(context.Background(), "acme", "hash")
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("VerifyAccessToken() = %+v, %v; want %+v, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestDeleteRemovesDeliveriesAndReportsMissing(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM _kora_extension`).WithArgs("acme", "crm").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM _kora_webhook_delivery`).WithArgs("acme", "crm").WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()
	if err := store.Delete(context.Background(), "acme", "crm"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM _kora_extension`).WithArgs("acme", "gone").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := store.Delete(context.Background(), "acme", "gone"); !errors.Is(err, extension.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateSecretReportsMissing(t *testing.T) {
	store, mock := newMockStore(t)
	mock.ExpectExec(`UPDATE _kora_extension SET old_secret = secret`).WillReturnResult(sqlmock.NewResult(0, 0))
	err := store.RotateSecret(context.Background(), extension.SecretRotation{Site: "acme", Name: "gone", Secret: "s", PreviousExpiresAt: time.Now()})
	if !errors.Is(err, extension.ErrNotFound) {
		t.Fatalf("RotateSecret(missing) = %v, want ErrNotFound", err)
	}
}
