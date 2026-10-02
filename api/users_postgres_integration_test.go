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

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresUserLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test user lifecycle routes against a disposable PostgreSQL database")
	}
	database, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal("open disposable PostgreSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable PostgreSQL database:", err)
	}
	schemaName := fmt.Sprintf("kora_api_users_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_user (
		name TEXT PRIMARY KEY, site TEXT NOT NULL, email TEXT NOT NULL, password_hash TEXT NOT NULL,
		full_name TEXT NOT NULL, enabled BOOLEAN NOT NULL, email_verified_at TIMESTAMPTZ,
		roles TEXT NOT NULL, creation TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		modified TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatal("create user fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_session (id TEXT PRIMARY KEY, site TEXT NOT NULL, "user" TEXT NOT NULL)`); err != nil {
		t.Fatal("create session fixture:", err)
	}
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})

	createRecorder := httptest.NewRecorder()
	handler.HandleUserCreate(userPostgresContext(createRecorder, database, registry, "site-pg", http.MethodPost, `/api/system/users`, `{"email":"operator@example.test","password":"initial-password","full_name":"Operator","roles":["Store Manager"]}`, ""))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("PostgreSQL user create returned HTTP %d: %s", createRecorder.Code, createRecorder.Body.String())
	}
	var created struct {
		Data UserResponse `json:"data"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &created); err != nil {
		t.Fatal("decode created user:", err)
	}
	userName := created.Data.Name
	if userName == "" || len(created.Data.Roles) != 1 || created.Data.Roles[0] != "Store Manager" {
		t.Fatalf("created PostgreSQL user = %#v", created.Data)
	}

	listRecorder := httptest.NewRecorder()
	handler.HandleUserList(userPostgresContext(listRecorder, database, registry, "site-pg", http.MethodGet, "/api/system/users", "", ""))
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), userName) {
		t.Fatalf("PostgreSQL user list returned HTTP %d: %s", listRecorder.Code, listRecorder.Body.String())
	}

	updateRecorder := httptest.NewRecorder()
	handler.HandleUserUpdate(userPostgresContext(updateRecorder, database, registry, "site-pg", http.MethodPut, "/api/system/users/"+userName, `{"full_name":"Shift Lead","roles":["Store Manager","Inventory Clerk"],"enabled":false,"password":"updated-password"}`, userName))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL user update returned HTTP %d: %s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var updated struct {
		Data UserResponse `json:"data"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &updated); err != nil {
		t.Fatal("decode updated user:", err)
	}
	if updated.Data.FullName != "Shift Lead" || updated.Data.Enabled || len(updated.Data.Roles) != 2 {
		t.Fatalf("updated PostgreSQL user = %#v", updated.Data)
	}

	if _, err := database.Exec(`INSERT INTO _kora_session (id, site, "user") VALUES ($1,$2,$3)`, "session-reset", "site-pg", userName); err != nil {
		t.Fatal("insert reset session:", err)
	}
	resetRecorder := httptest.NewRecorder()
	handler.HandleUserResetPassword(userPostgresContext(resetRecorder, database, registry, "site-pg", http.MethodPost, "/api/system/users/"+userName+"/reset-password", `{"password":"reset-password-1"}`, userName))
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL user password reset returned HTTP %d: %s", resetRecorder.Code, resetRecorder.Body.String())
	}
	var sessions int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_session WHERE site = $1 AND "user" = $2`, "site-pg", userName).Scan(&sessions); err != nil {
		t.Fatal("count reset sessions:", err)
	}
	if sessions != 0 {
		t.Fatalf("sessions after reset = %d, want 0", sessions)
	}
	var hashBeforeFailure string
	if err := database.QueryRow(`SELECT password_hash FROM _kora_user WHERE site = $1 AND name = $2`, "site-pg", userName).Scan(&hashBeforeFailure); err != nil {
		t.Fatal("read reset password hash:", err)
	}

	if _, err := database.Exec(`INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, roles) VALUES ($1,$2,$3,$4,$5,$6,$7)`, "delete-target", "site-pg", "delete@example.test", "hash", "Delete Target", true, ""); err != nil {
		t.Fatal("insert delete target:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_session (id, site, "user") VALUES ($1,$2,$3)`, "session-delete", "site-pg", "delete-target"); err != nil {
		t.Fatal("insert delete session:", err)
	}
	if _, err := database.Exec(`DROP TABLE _kora_session`); err != nil {
		t.Fatal("drop session table for failure-path tests:", err)
	}

	failedResetRecorder := httptest.NewRecorder()
	handler.HandleUserResetPassword(userPostgresContext(failedResetRecorder, database, registry, "site-pg", http.MethodPost, "/api/system/users/"+userName+"/reset-password", `{"password":"reset-password-2"}`, userName))
	if failedResetRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("reset with session storage unavailable returned HTTP %d: %s", failedResetRecorder.Code, failedResetRecorder.Body.String())
	}
	var hashAfterFailure string
	if err := database.QueryRow(`SELECT password_hash FROM _kora_user WHERE site = $1 AND name = $2`, "site-pg", userName).Scan(&hashAfterFailure); err != nil {
		t.Fatal("read password hash after failed reset:", err)
	}
	if hashAfterFailure != hashBeforeFailure {
		t.Fatal("password changed despite session-revocation failure")
	}

	deleteRecorder := httptest.NewRecorder()
	handler.HandleUserDelete(userPostgresContext(deleteRecorder, database, registry, "site-pg", http.MethodDelete, "/api/system/users/delete-target", "", "delete-target"))
	if deleteRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("delete with session storage unavailable returned HTTP %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	var deleteTargetCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_user WHERE site = $1 AND name = $2`, "site-pg", "delete-target").Scan(&deleteTargetCount); err != nil {
		t.Fatal("count user after failed delete:", err)
	}
	if deleteTargetCount != 1 {
		t.Fatal("user was deleted despite session-revocation failure")
	}
}

func userPostgresContext(recorder *httptest.ResponseRecorder, database *sql.DB, registry *doctype.Registry, site, method, path, body, name string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", site)
	ctx.Set("site_db_type", "postgres")
	ctx.Set("user", "admin-user")
	ctx.Set("user_roles", []string{doctype.AdminRole})
	if name != "" {
		ctx.Params = gin.Params{{Key: "name", Value: name}}
	}
	return ctx
}
