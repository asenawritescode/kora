package api

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	koraNet "github.com/asenawritescode/kora/net"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresConsolePasswordReset(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test console password reset against a disposable PostgreSQL database")
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
	schemaName := fmt.Sprintf("kora_console_reset_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_user (name TEXT PRIMARY KEY, email TEXT NOT NULL, password_hash TEXT NOT NULL, modified TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal("create user fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_session (id TEXT PRIMARY KEY, "user" TEXT NOT NULL)`); err != nil {
		t.Fatal("create session fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_user (name, email, password_hash) VALUES ($1,$2,$3)`, "user-1", "owner@example.test", "initial-hash"); err != nil {
		t.Fatal("insert user fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_session (id, "user") VALUES ($1,$2)`, "session-1", "user-1"); err != nil {
		t.Fatal("insert session fixture:", err)
	}
	loaded := &koraNet.LoadedSite{Name: "site-pg", DBType: "postgres", DB: database}
	handler := &ConsoleHandler{SiteRouter: koraNet.NewSiteRouter([]*koraNet.LoadedSite{loaded})}

	recorder := httptest.NewRecorder()
	ctx := consoleResetContext(recorder, "site-pg", `{"email":"owner@example.test","new_password":"new-password-1"}`)
	handler.HandleResetSitePassword(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL password reset returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var passwordHash string
	if err := database.QueryRow(`SELECT password_hash FROM _kora_user WHERE name = $1`, "user-1").Scan(&passwordHash); err != nil {
		t.Fatal("read updated password hash:", err)
	}
	if passwordHash == "initial-hash" {
		t.Fatal("password hash was not updated")
	}
	var sessions int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_session WHERE "user" = $1`, "user-1").Scan(&sessions); err != nil {
		t.Fatal("count remaining user sessions:", err)
	}
	if sessions != 0 {
		t.Fatalf("remaining sessions = %d, want 0", sessions)
	}

	if _, err := database.Exec(`DROP TABLE _kora_session`); err != nil {
		t.Fatal("remove session table for rollback check:", err)
	}
	failureRecorder := httptest.NewRecorder()
	failureContext := consoleResetContext(failureRecorder, "site-pg", `{"email":"owner@example.test","new_password":"new-password-2"}`)
	handler.HandleResetSitePassword(failureContext)
	if failureRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("reset with unavailable session table returned HTTP %d, want 500: %s", failureRecorder.Code, failureRecorder.Body.String())
	}
	var hashAfterFailure string
	if err := database.QueryRow(`SELECT password_hash FROM _kora_user WHERE name = $1`, "user-1").Scan(&hashAfterFailure); err != nil {
		t.Fatal("read password hash after failed reset:", err)
	}
	if hashAfterFailure != passwordHash {
		t.Fatal("password changed even though session revocation failed")
	}
}

func consoleResetContext(recorder *httptest.ResponseRecorder, siteName, body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/console/sites/"+siteName+"/reset-password", strings.NewReader(body))
	ctx.Params = gin.Params{{Key: "name", Value: siteName}}
	ctx.Set("user", "console-admin")
	return ctx
}
