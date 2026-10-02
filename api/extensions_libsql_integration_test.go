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
	ksite "github.com/asenawritescode/kora/site"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestLiveLibSQLManagedCredentialLifecycle exercises create, idempotent replay,
// and rotation against a disposable LibSQL server. The target must be a fresh
// database: the test refuses to run if any user tables already exist.
func TestLiveLibSQLManagedCredentialLifecycle(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_LIBSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_LIBSQL_DSN to a fresh disposable LibSQL database")
	}
	database, err := sql.Open("libsql", dsn)
	if err != nil {
		t.Fatal("open isolated LibSQL database:", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if err := database.Ping(); err != nil {
		t.Fatal("ping disposable LibSQL database:", err)
	}
	var existingTables int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&existingTables); err != nil {
		t.Fatal("check that LibSQL target is empty:", err)
	}
	if existingTables != 0 {
		t.Fatalf("refusing to mutate non-empty LibSQL target: found %d existing tables", existingTables)
	}
	var legacyExtensionDDL string
	for _, statement := range db.ExtensibilityTablesLibSQL() {
		if strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS _kora_extension") {
			// Model a pre-migration schema and verify the additive migration
			// installs the retry key rather than relying on a fresh-table shape.
			legacyExtensionDDL = strings.Replace(statement, "managed_idempotency_key TEXT NOT NULL DEFAULT '',\n", "", 1)
			break
		}
	}
	if legacyExtensionDDL == "" {
		t.Fatal("LibSQL extension DDL did not contain the extension table")
	}
	if _, err := database.Exec(legacyExtensionDDL); err != nil {
		t.Fatalf("create legacy LibSQL extension fixture: %v", err)
	}
	if err := ksite.BootstrapSystemTables(database, db.Resolve("libsql")); err != nil {
		t.Fatal("upgrade legacy LibSQL system tables:", err)
	}
	var migrated bool
	rows, err := database.Query(`PRAGMA table_info(_kora_extension)`)
	if err != nil {
		t.Fatal("inspect LibSQL extension migration:", err)
	}
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal("scan LibSQL extension columns:", err)
		}
		if name == "managed_idempotency_key" {
			migrated = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal("read LibSQL extension columns:", err)
	}
	rows.Close()
	if !migrated {
		t.Fatal("pre-migration LibSQL extension table lacks managed_idempotency_key after upgrade")
	}

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: db.Resolve("libsql")})
	siteName := fmt.Sprintf("site-libsql-%d", time.Now().UnixNano())
	rotate := func(key string) (*httptest.ResponseRecorder, managedChannelClientResponse) {
		recorder := httptest.NewRecorder()
		ctx := extensionTestContext(recorder, database, registry, siteName, http.MethodPost, `{}`, "")
		ctx.Set("site_db_type", "libsql")
		ctx.Set("auth_type", "engine_provisioner")
		ctx.Request.Header.Set("Idempotency-Key", key)
		handler.HandleManagedChannelClientRotate(ctx)
		var response struct {
			Data managedChannelClientResponse `json:"data"`
		}
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode LibSQL managed credential response: %v", err)
			}
		}
		return recorder, response.Data
	}

	first, firstCredential := rotate("libsql-operation-0001")
	if first.Code != http.StatusOK || firstCredential.AccessToken == "" {
		t.Fatalf("LibSQL managed credential create returned HTTP %d: %s", first.Code, first.Body.String())
	}
	replay, replayCredential := rotate("libsql-operation-0001")
	if replay.Code != http.StatusOK || replayCredential.AccessToken != firstCredential.AccessToken {
		t.Fatalf("LibSQL same-key replay returned HTTP %d or changed the token: %s", replay.Code, replay.Body.String())
	}
	rotated, rotatedCredential := rotate("libsql-operation-0002")
	if rotated.Code != http.StatusOK || rotatedCredential.AccessToken == firstCredential.AccessToken {
		t.Fatalf("LibSQL new-key rotation returned HTTP %d or retained the prior token: %s", rotated.Code, rotated.Body.String())
	}
	var storedToken, storedKey string
	if err := database.QueryRow(`SELECT access_token, managed_idempotency_key FROM _kora_extension WHERE site = ? AND name = ?`, siteName, "kora-cloud-channel").Scan(&storedToken, &storedKey); err != nil {
		t.Fatal("read persisted LibSQL managed credential:", err)
	}
	if storedToken != rotatedCredential.AccessToken || storedKey == "" || storedToken == firstCredential.AccessToken {
		t.Fatal("LibSQL managed credential did not persist the latest token and idempotency receipt")
	}
}
