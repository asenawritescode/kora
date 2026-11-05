package auth

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	kdb "github.com/asenawritescode/kora/db"
)

func TestSessionManagerUsesDialectPlaceholders(t *testing.T) {
	tests := []struct {
		name    string
		dialect kdb.Dialect
		insert  string
		read    string
	}{
		{
			name:    "postgres",
			dialect: kdb.Resolve("postgres"),
			insert: `INSERT INTO _kora_session (sid, site, "user", data, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
			read: `SELECT data, expires_at, created_at FROM _kora_session WHERE site = $1 AND sid = $2`,
		},
		{
			name:    "mysql",
			dialect: kdb.Resolve("mysql"),
			insert:  "INSERT INTO _kora_session (sid, site, `user`, data, expires_at, created_at)\n\t\t VALUES (?, ?, ?, ?, ?, ?)",
			read:    `SELECT data, expires_at, created_at FROM _kora_session WHERE site = ? AND sid = ?`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(tt.insert).WillReturnResult(sqlmock.NewResult(0, 1))
			now := time.Now().UTC()
			mock.ExpectQuery(tt.read).WillReturnRows(sqlmock.NewRows([]string{"data", "expires_at", "created_at"}).
				AddRow(`{"name":"u1","email":"u1@example.com","full_name":"U","roles":["Sales User"],"enabled":true}`, now.Add(time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)))

			writer := newRequestSessionManager(db).SetDialect(tt.dialect)
			sid, err := writer.CreateSession("site-a", &User{Name: "u1", Email: "u1@example.com", Roles: []string{"Sales User"}, Enabled: true})
			if err != nil {
				t.Fatal("create session:", err)
			}
			reader := newRequestSessionManager(db).SetDialect(tt.dialect)
			user, _, err := reader.GetSession("site-a", sid)
			if err != nil {
				t.Fatal("read session:", err)
			}
			if user.Name != "u1" {
				t.Fatalf("user = %q", user.Name)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSessionManagerWithoutDialectKeepsQuestionMarks(t *testing.T) {
	sm := newRequestSessionManager(nil)
	if got := sm.sql("SELECT 1 WHERE a = ?"); got != "SELECT 1 WHERE a = ?" {
		t.Fatalf("sql() = %q", got)
	}
	if !regexp.MustCompile(`^user$`).MatchString(sm.userColumn()) {
		t.Fatalf("userColumn() = %q", sm.userColumn())
	}
}
