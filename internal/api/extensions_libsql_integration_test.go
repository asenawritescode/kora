package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/auth"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	ksite "github.com/asenawritescode/kora/internal/site"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// TestLiveLibSQLManagedCredentialLifecycle exercises create, idempotent replay,
// and rotation against a disposable LibSQL server. The target must be a fresh
// database: the test refuses to run if any user tables already exist.
func TestLiveLibSQLManagedCredentialLifecycle(t *testing.T) {
	t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "integration-provisioning-secret")
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
	if err = ksite.BootstrapSystemTables(context.Background(), database, db.Resolve("libsql")); err != nil {
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
	if err := database.QueryRow(`SELECT access_token_hash, managed_idempotency_key FROM _kora_extension WHERE site = ? AND name = ?`, siteName, "kora-cloud-channel").Scan(&storedToken, &storedKey); err != nil {
		t.Fatal("read persisted LibSQL managed credential:", err)
	}
	if storedToken != auth.HashExtensionAccessToken(rotatedCredential.AccessToken) || storedKey == "" || storedToken == auth.HashExtensionAccessToken(firstCredential.AccessToken) {
		t.Fatal("LibSQL managed credential did not persist the latest token and idempotency receipt")
	}
	sessionHash := sha256.Sum256([]byte("libsql-managed-session"))
	if _, err := database.Exec(`INSERT INTO _kora_channel_session (id, site, client_name, conversation_key, provider, sender_address, token_hash, api_permissions, trusted_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, "managed-session", siteName, "kora-cloud-channel", "conversation", "whatsapp", "sender", hex.EncodeToString(sessionHash[:]), "[]", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal("insert disposable LibSQL managed session:", err)
	}
	fingerprint := sha256.Sum256([]byte(rotatedCredential.AccessToken))
	revokeBody, _ := json.Marshal(contract.RevokeManagedChannelClientV2{CredentialFingerprint: hex.EncodeToString(fingerprint[:])})
	revokeRecorder := httptest.NewRecorder()
	revokeContext := extensionTestContext(revokeRecorder, database, registry, siteName, http.MethodPost, string(revokeBody), "")
	revokeContext.Set("site_db_type", "libsql")
	revokeContext.Set("auth_type", "engine_provisioner")
	revokeContext.Request.Header.Set("Idempotency-Key", "libsql-delete-revoke-0001")
	handler.HandleManagedChannelClientRevoke(revokeContext)
	if revokeRecorder.Code != http.StatusOK {
		t.Fatalf("LibSQL managed credential revocation returned HTTP %d: %s", revokeRecorder.Code, revokeRecorder.Body.String())
	}
	var active bool
	if err := database.QueryRow(`SELECT is_active FROM _kora_extension WHERE site = ? AND name = ?`, siteName, "kora-cloud-channel").Scan(&active); err != nil {
		t.Fatal("read LibSQL managed credential state:", err)
	}
	var revokedReason string
	if err := database.QueryRow(`SELECT revoked_reason FROM _kora_channel_session WHERE id = ?`, "managed-session").Scan(&revokedReason); err != nil {
		t.Fatal("read LibSQL channel session revocation:", err)
	}
	if active || revokedReason != "tenant_deletion" {
		t.Fatal("LibSQL revocation did not disable the credential and session")
	}
}
