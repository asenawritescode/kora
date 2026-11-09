package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	resolveLinkSQL = "SELECT user_name FROM _kora_user_identity WHERE site = \\? AND issuer = \\? AND subject = \\?"
	subjectForSQL  = "SELECT subject FROM _kora_user_identity WHERE site = \\? AND user_name = \\? AND issuer = \\?"
	insertLinkSQL  = "INSERT INTO _kora_user_identity"
)

func TestSQLIdentityLinksLink(t *testing.T) {
	linkedAt := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		mock    func(sqlmock.Sqlmock)
		wantErr error
	}{
		{
			name: "new link inserts",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(resolveLinkSQL).WithArgs("s1", "https://idp", "sub-1").WillReturnError(sql.ErrNoRows)
				m.ExpectQuery(subjectForSQL).WithArgs("s1", "u1", "https://idp").WillReturnError(sql.ErrNoRows)
				m.ExpectExec(insertLinkSQL).WithArgs("s1", "u1", "https://idp", "sub-1", "a@example.com", linkedAt).WillReturnResult(sqlmock.NewResult(0, 1))
			},
		},
		{
			name: "same pair same user is a no-op",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(resolveLinkSQL).WillReturnRows(sqlmock.NewRows([]string{"user_name"}).AddRow("u1"))
			},
		},
		{
			name: "pair owned by another user collides",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(resolveLinkSQL).WillReturnRows(sqlmock.NewRows([]string{"user_name"}).AddRow("u2"))
			},
			wantErr: ErrIdentityCollision,
		},
		{
			name: "user already linked to another subject for this issuer collides",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(resolveLinkSQL).WillReturnError(sql.ErrNoRows)
				m.ExpectQuery(subjectForSQL).WillReturnRows(sqlmock.NewRows([]string{"subject"}).AddRow("sub-old"))
			},
			wantErr: ErrIdentityCollision,
		},
		{
			name: "lost insert race to another user collides",
			mock: func(m sqlmock.Sqlmock) {
				m.ExpectQuery(resolveLinkSQL).WillReturnError(sql.ErrNoRows)
				m.ExpectQuery(subjectForSQL).WillReturnError(sql.ErrNoRows)
				m.ExpectExec(insertLinkSQL).WillReturnError(errors.New("duplicate key"))
				m.ExpectQuery(resolveLinkSQL).WillReturnRows(sqlmock.NewRows([]string{"user_name"}).AddRow("u2"))
			},
			wantErr: ErrIdentityCollision,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tc.mock(mock)
			err = SQLIdentityLinks{DB: db}.Link(context.Background(), "s1", "u1", "https://idp", "sub-1", "a@example.com", linkedAt)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Link() error = %v, want %v", err, tc.wantErr)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLIdentityLinksResolveMissingIsLinkNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(resolveLinkSQL).WillReturnError(sql.ErrNoRows)
	if _, err := (SQLIdentityLinks{DB: db}).Resolve(context.Background(), "s1", "https://idp", "nobody"); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrLinkNotFound", err)
	}
}
