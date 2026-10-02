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

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/secret"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func TestLivePostgresSecretRoutes(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test secret API routes against a disposable PostgreSQL database")
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
	schemaName := fmt.Sprintf("kora_api_secrets_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	dialect := db.Resolve("postgres")
	store := secret.NewStore(database, dialect)
	if err := store.EnsureTable(); err != nil {
		t.Fatal("create PostgreSQL secret table:", err)
	}
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: dialect})
	makeContext := func(recorder *httptest.ResponseRecorder, method, path, body string) *gin.Context {
		gin.SetMode(gin.TestMode)
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(method, path, strings.NewReader(body))
		ctx.Set("site_db", database)
		ctx.Set("site_db_type", "postgres")
		ctx.Set("site_name", "site-postgres-secret")
		ctx.Set("user_roles", []string{doctype.AdminRole})
		return ctx
	}

	setRecorder := httptest.NewRecorder()
	setContext := makeContext(setRecorder, http.MethodPost, "/api/system/secrets", `{"key":"demo_api_key","value":"secret-value-sentinel"}`)
	handler.HandleSecretSet(setContext)
	if setRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL secret set returned HTTP %d: %s", setRecorder.Code, setRecorder.Body.String())
	}
	var encrypted []byte
	if err := database.QueryRow(`SELECT encrypted_value FROM _kora_secret WHERE site = $1 AND key_name = $2`, "site-postgres-secret", "demo_api_key").Scan(&encrypted); err != nil {
		t.Fatal("read encrypted PostgreSQL secret:", err)
	}
	if len(encrypted) == 0 || strings.Contains(string(encrypted), "secret-value-sentinel") {
		t.Fatal("PostgreSQL secret was not encrypted at rest")
	}

	listRecorder := httptest.NewRecorder()
	listContext := makeContext(listRecorder, http.MethodGet, "/api/system/secrets", "")
	handler.HandleSecretList(listContext)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL secret list returned HTTP %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Data []SecretEntry `json:"data"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil {
		t.Fatal("decode PostgreSQL secret list:", err)
	}
	if len(list.Data) != 1 || list.Data[0].KeyName != "demo_api_key" || strings.Contains(listRecorder.Body.String(), "secret-value-sentinel") {
		t.Fatalf("PostgreSQL secret list leaked or omitted key metadata: %s", listRecorder.Body.String())
	}

	deleteRecorder := httptest.NewRecorder()
	deleteContext := makeContext(deleteRecorder, http.MethodDelete, "/api/system/secrets/demo_api_key", "")
	deleteContext.Params = gin.Params{{Key: "key", Value: "demo_api_key"}}
	handler.HandleSecretDelete(deleteContext)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL secret delete returned HTTP %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	keys, err := store.List("site-postgres-secret")
	if err != nil || len(keys) != 0 {
		t.Fatalf("PostgreSQL secret remained after delete: keys=%v err=%v", keys, err)
	}
}
