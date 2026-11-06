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

func TestLivePostgresUserAccessUpdateKeepsCloudIdentityImmutable(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test tenant user access against a disposable PostgreSQL database")
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
	// Access changes revoke the user's sessions in the same transaction.
	if _, err := database.Exec(`CREATE TABLE _kora_session (id TEXT PRIMARY KEY, site TEXT NOT NULL DEFAULT '', "user" TEXT NOT NULL)`); err != nil {
		t.Fatal("create session fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_auth_revision (id INTEGER PRIMARY KEY, policy_revision BIGINT NOT NULL, membership_revision BIGINT NOT NULL)`); err != nil {
		t.Fatal("create authorization revision fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_auth_revision (id, policy_revision, membership_revision) VALUES (1,0,0)`); err != nil {
		t.Fatal("seed authorization revision fixture:", err)
	}
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})
	if _, err := database.Exec(`INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled, email_verified_at, roles) VALUES ($1,$2,$3,$4,$5,$6,CURRENT_TIMESTAMP,$7)`, "cloud-user-1", "site-pg", "owner@example.test", "$kora$passwordless$disabled", "Cloud Owner", true, "Owner"); err != nil {
		t.Fatal("insert Cloud-linked principal fixture:", err)
	}

	listRecorder := httptest.NewRecorder()
	handler.HandleUserList(userPostgresContext(t, listRecorder, database, registry, "site-pg", http.MethodGet, "/api/system/users", "", ""))
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), "cloud-user-1") {
		t.Fatalf("PostgreSQL user list returned HTTP %d: %s", listRecorder.Code, listRecorder.Body.String())
	}

	updateRecorder := httptest.NewRecorder()
	handler.HandleUserUpdate(userPostgresContext(t, updateRecorder, database, registry, "site-pg", http.MethodPut, "/api/system/users/cloud-user-1", `{"roles":["Owner","Inventory Clerk"],"enabled":false}`, "cloud-user-1"))
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL access update returned HTTP %d: %s", updateRecorder.Code, updateRecorder.Body.String())
	}
	var updated struct {
		Data UserResponse `json:"data"`
	}
	if err := json.Unmarshal(updateRecorder.Body.Bytes(), &updated); err != nil {
		t.Fatal("decode updated user:", err)
	}
	if updated.Data.Name != "cloud-user-1" || updated.Data.Email != "owner@example.test" || updated.Data.FullName != "Cloud Owner" || updated.Data.Enabled || len(updated.Data.Roles) != 2 {
		t.Fatalf("updated Cloud-linked principal = %#v", updated.Data)
	}
	var passwordHash string
	if err := database.QueryRow(`SELECT password_hash FROM _kora_user WHERE site = $1 AND name = $2`, "site-pg", "cloud-user-1").Scan(&passwordHash); err != nil {
		t.Fatal("read Engine password hash:", err)
	}
	if passwordHash != "$kora$passwordless$disabled" {
		t.Fatal("workspace role update changed the Cloud-owned credential state")
	}

	identityUpdate := httptest.NewRecorder()
	handler.HandleUserUpdate(userPostgresContext(t, identityUpdate, database, registry, "site-pg", http.MethodPut, "/api/system/users/cloud-user-1", `{"email":"other@example.test","password":"new-password","full_name":"Other"}`, "cloud-user-1"))
	if identityUpdate.Code != http.StatusBadRequest {
		t.Fatalf("Engine identity mutation returned HTTP %d, want 400: %s", identityUpdate.Code, identityUpdate.Body.String())
	}
}

func userPostgresContext(t testing.TB, recorder *httptest.ResponseRecorder, database *sql.DB, registry *doctype.Registry, site, method, path, body, name string) *gin.Context {
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
	ctx.Set("policy_execution", postgresTestExecution(t, site, "admin-user", []string{doctype.AdminRole}))
	if name != "" {
		ctx.Params = gin.Params{{Key: "name", Value: name}}
	}
	return ctx
}
