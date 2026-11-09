package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

type revocationUser struct {
	name, roles string
	enabled     bool
}

// userRevocationTargets lists the databases the access-update tests run on:
// in-memory SQLite (LibSQL dialect) always, MySQL when KORA_API_USERS_MYSQL_DSN
// points at a disposable server (each test gets its own database there).
func userRevocationTargets() []string {
	targets := []string{"libsql"}
	if os.Getenv("KORA_API_USERS_MYSQL_DSN") != "" {
		targets = append(targets, "mysql")
	}
	return targets
}

func newUserRevocationDB(t *testing.T, dialectName string, users []revocationUser) *sql.DB {
	t.Helper()
	var database *sql.DB
	var authRevisionDDL string
	switch dialectName {
	case "mysql":
		config, err := mysql.ParseDSN(os.Getenv("KORA_API_USERS_MYSQL_DSN"))
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("kora_user_revoke_%d", time.Now().UnixNano()%1_000_000_000)
		config.DBName = ""
		root, err := sql.Open("mysql", config.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = root.Exec("DROP DATABASE IF EXISTS `" + name + "`"); _ = root.Close() })
		config.DBName = name
		if database, err = sql.Open("mysql", config.FormatDSN()); err != nil {
			t.Fatal(err)
		}
		authRevisionDDL = `CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL) ENGINE=InnoDB`
	default:
		var err error
		if database, err = sql.Open("sqlite3", ":memory:"); err != nil {
			t.Fatal(err)
		}
		authRevisionDDL = `CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY CHECK (id = 1), policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL)`
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	for _, statement := range kdb.Resolve(dialectName).SystemTableSQL() {
		if strings.HasPrefix(statement, "CREATE TABLE") && (strings.Contains(statement, "_kora_user\"") || strings.Contains(statement, "_kora_user (") || strings.Contains(statement, "_kora_session\"") || strings.Contains(statement, "_kora_session (")) {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, statement := range []string{authRevisionDDL, `INSERT INTO _kora_auth_revision (id, policy_revision, membership_revision) VALUES (1, 0, 0)`} {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range users {
		if _, err := database.Exec(`INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES (?, 'site-a', ?, 'x', ?, ?, ?)`,
			u.name, u.name+"@example.test", u.name, u.enabled, u.roles); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestHandleUserUpdateRevokesSessionsAndGuardsAdministrators(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admin := doctype.AdminRole
	tests := []struct {
		name         string
		users        []revocationUser
		caller       string
		target       string
		body         string
		wantStatus   int
		wantCode     string
		wantField    string
		wantRevoked  bool
		wantRoles    string // target roles after the request
		wantDisabled bool
	}{
		{name: "role change revokes the user's sessions", target: "cashier", body: `{"roles":["Manager"]}`, wantStatus: 200, wantRevoked: true, wantRoles: "Manager"},
		{name: "disable revokes the user's sessions", target: "cashier", body: `{"enabled":false}`, wantStatus: 200, wantRevoked: true, wantRoles: "Cashier", wantDisabled: true},
		{name: "an unchanged role set keeps sessions", target: "cashier", body: `{"roles":["Cashier"]}`, wantStatus: 200, wantRoles: "Cashier"},
		{name: "enabling an enabled user keeps sessions", target: "cashier", body: `{"enabled":true}`, wantStatus: 200, wantRoles: "Cashier"},
		{name: "self disable is a conflict", target: "admin-1", body: `{"enabled":false}`, wantStatus: 409, wantCode: "user.self_disable", wantField: "enabled", wantRoles: admin},
		{name: "removing own admin role is a conflict", target: "admin-1", body: `{"roles":["Manager"]}`, wantStatus: 409, wantCode: "user.self_demote", wantField: "roles", wantRoles: admin},
		{name: "self role change that keeps Administrator works", target: "admin-1", body: `{"roles":["Administrator","Manager"]}`, wantStatus: 200, wantRevoked: true, wantRoles: "Administrator,Manager"},
		{name: "demoting another administrator works while one remains", target: "admin-2", body: `{"roles":["Manager"]}`, wantStatus: 200, wantRevoked: true, wantRoles: "Manager"},
		{name: "disabling another administrator works while one remains", target: "admin-2", body: `{"enabled":false}`, wantStatus: 200, wantRevoked: true, wantRoles: admin, wantDisabled: true},
		{
			name:       "disabling the last administrator is a conflict",
			users:      []revocationUser{{"admin-2", admin, true}, {"admin-3", admin, false}, {"cashier", "Cashier", true}},
			caller:     "stale-admin",
			target:     "admin-2",
			body:       `{"enabled":false}`,
			wantStatus: 409, wantCode: "user.last_administrator", wantField: "enabled", wantRoles: admin,
		},
		{
			name:       "demoting the last enabled administrator is a conflict",
			users:      []revocationUser{{"admin-2", admin, true}, {"admin-3", admin, false}, {"cashier", "Cashier", true}},
			caller:     "stale-admin",
			target:     "admin-2",
			body:       `{"roles":["Manager"]}`,
			wantStatus: 409, wantCode: "user.last_administrator", wantField: "roles", wantRoles: admin,
		},
		{
			name:       "changing roles of a non-admin never trips the last administrator guard",
			users:      []revocationUser{{"admin-2", admin, true}, {"cashier", "Cashier", true}},
			caller:     "stale-admin",
			target:     "cashier",
			body:       `{"roles":["Manager"],"enabled":false}`,
			wantStatus: 200, wantRevoked: true, wantRoles: "Manager", wantDisabled: true,
		},
	}
	for _, dialectName := range userRevocationTargets() {
		for _, tt := range tests {
			t.Run(dialectName+"/"+tt.name, func(t *testing.T) {
				users := tt.users
				if users == nil {
					users = []revocationUser{{"admin-1", admin, true}, {"admin-2", admin, true}, {"cashier", "Cashier", true}}
				}
				caller := tt.caller
				if caller == "" {
					caller = "admin-1"
				}
				database := newUserRevocationDB(t, dialectName, users)
				sm := auth.NewSessionManager(database).SetDialect(kdb.Resolve(dialectName))
				t.Cleanup(func() { _ = sm.Close() })
				sid, err := sm.CreateSession("site-a", &auth.User{Name: tt.target, Roles: []string{"Cashier"}, Enabled: true})
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := sm.GetSession("site-a", sid); err != nil { // warm the cache
					t.Fatal(err)
				}

				execution, err := policy.NewVerifiedExecution("tenant-a", contract.ActorContext{PrincipalID: caller, SubjectUserID: caller, PrincipalType: contract.PrincipalHuman, Site: "site-a", Roles: []string{admin}, AuthenticatedAt: time.Now().UTC()})
				if err != nil {
					t.Fatal(err)
				}
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/system/users/"+tt.target, strings.NewReader(tt.body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Params = gin.Params{{Key: "name", Value: tt.target}}
				c.Set("site_db", database)
				c.Set("site_db_type", dialectName)
				c.Set("site_name", "site-a")
				c.Set("site_id", "tenant-a")
				c.Set("site_registry", doctype.NewRegistry())
				c.Set("site_session_manager", sm)
				c.Set("policy_execution", execution)
				NewHandler(doctype.NewRegistry(), nil).HandleUserUpdate(c)

				if recorder.Code != tt.wantStatus {
					t.Fatalf("HTTP %d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
				}
				if tt.wantCode != "" {
					var body struct {
						Error ErrorBody `json:"error"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.Error.Code != tt.wantCode || body.Error.Details["field"] != tt.wantField {
						t.Fatalf("error = %+v, want code %s field %s", body.Error, tt.wantCode, tt.wantField)
					}
				}
				var roles string
				var enabled bool
				if err := database.QueryRow(`SELECT roles, enabled FROM _kora_user WHERE name = ?`, tt.target).Scan(&roles, &enabled); err != nil {
					t.Fatal(err)
				}
				if roles != tt.wantRoles || enabled == tt.wantDisabled {
					t.Fatalf("stored roles=%q enabled=%v, want roles=%q disabled=%v", roles, enabled, tt.wantRoles, tt.wantDisabled)
				}
				// The old cookie must stop working through the same path validateSession uses.
				_, _, sessionErr := sm.GetSession("site-a", sid)
				if tt.wantRevoked && sessionErr == nil {
					t.Fatal("old session still valid after the change")
				}
				if !tt.wantRevoked && sessionErr != nil {
					t.Fatalf("session was revoked without a role or enabled change: %v", sessionErr)
				}
			})
		}
	}
}
