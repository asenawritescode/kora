package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"

	kdb "github.com/asenawritescode/kora/db"
)

func newRevokeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	for _, statement := range (&kdb.LibSQLDialect{}).SystemTableSQL() {
		if strings.Contains(statement, "_kora_session") && strings.HasPrefix(statement, "CREATE TABLE") {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	return database
}

func TestRevokeUserSessionsRemovesOnlyThatUsersSessionsInThatSite(t *testing.T) {
	type session struct{ site, user string }
	tests := []struct {
		name       string
		revokeSite string
		revokeUser string
		viaTx      bool
		wantGone   []int // indexes into the session fixtures
	}{
		{"all sessions of the user in the site", "site-a", "u1", false, []int{0, 1}},
		{"same user name in another site is kept", "site-b", "u1", false, []int{3}},
		{"unknown user is a no-op", "site-a", "nobody", false, nil},
		{"transactional variant removes the same rows", "site-a", "u1", true, []int{0, 1}},
	}
	fixtures := []session{{"site-a", "u1"}, {"site-a", "u1"}, {"site-a", "u2"}, {"site-b", "u1"}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := newRevokeTestDB(t)
			sm := newRequestSessionManager(database).SetDialect(&kdb.LibSQLDialect{})
			sids := make([]string, len(fixtures))
			for i, f := range fixtures {
				sid, err := sm.CreateSession(f.site, &User{Name: f.user, Roles: []string{"Cashier"}, Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				sids[i] = sid
				// Warm the cache so the test proves cached sessions are dropped too.
				if _, _, err := sm.GetSession(f.site, sid); err != nil {
					t.Fatal(err)
				}
			}
			if tt.viaTx {
				tx, err := database.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if err := sm.RevokeUserSessionsTx(context.Background(), tx, tt.revokeSite, tt.revokeUser); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				sm.PurgeUserCache(tt.revokeSite, tt.revokeUser)
			} else if err := sm.RevokeUserSessions(context.Background(), tt.revokeSite, tt.revokeUser); err != nil {
				t.Fatal(err)
			}
			gone := map[int]bool{}
			for _, i := range tt.wantGone {
				gone[i] = true
			}
			for i, f := range fixtures {
				_, _, err := sm.GetSession(f.site, sids[i])
				if gone[i] && err == nil {
					t.Errorf("session %d (%s/%s) still valid after revoke", i, f.site, f.user)
				}
				if !gone[i] && err != nil {
					t.Errorf("session %d (%s/%s) wrongly revoked: %v", i, f.site, f.user, err)
				}
			}
		})
	}
}

func TestRevokeUserSessionsRolledBackTransactionKeepsSessions(t *testing.T) {
	database := newRevokeTestDB(t)
	sm := newRequestSessionManager(database).SetDialect(&kdb.LibSQLDialect{})
	sid, err := sm.CreateSession("site-a", &User{Name: "u1", Roles: []string{"Cashier"}})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.RevokeUserSessionsTx(context.Background(), tx, "site-a", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sm.GetSession("site-a", sid); err != nil {
		t.Fatalf("rolled-back revocation must keep the session: %v", err)
	}
}

func TestValidateSessionRejectsOldCookieAfterRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database := newRevokeTestDB(t)
	sm := newRequestSessionManager(database).SetDialect(&kdb.LibSQLDialect{})
	sid, err := sm.CreateSession("site-a", &User{Name: "u1", Roles: []string{"Cashier"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	request := func() int {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.AddCookie(&http.Cookie{Name: "kora_sid", Value: sid})
		c.Set("site_name", "site-a")
		c.Set("site_session_manager", sm)
		c.Set("site_db", database)
		validateSession(c, sm)
		return recorder.Code
	}
	if code := request(); code != http.StatusOK {
		t.Fatalf("before revoke: HTTP %d", code)
	}
	if err := sm.RevokeUserSessions(context.Background(), "site-a", "u1"); err != nil {
		t.Fatal(err)
	}
	if code := request(); code != http.StatusUnauthorized {
		t.Fatalf("after revoke: HTTP %d, want 401", code)
	}
}
