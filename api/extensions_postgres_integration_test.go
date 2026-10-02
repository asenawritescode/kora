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

func TestLivePostgresExtensionRoutes(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test extension API routes against a disposable PostgreSQL database")
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
	schemaName := fmt.Sprintf("kora_api_extensions_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	for _, statement := range kdb.ExtensibilityTablesPostgres() {
		if strings.HasPrefix(statement, "CREATE TABLE IF NOT EXISTS _kora_extension") {
			// Model an existing tenant schema from before managed credential
			// idempotency was introduced, so the additive migration below is
			// exercised instead of colliding with the column in a fresh table.
			statement = strings.Replace(statement, "managed_idempotency_key CHAR(64) NOT NULL DEFAULT '', ", "", 1)
		}
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create extensibility fixture: %v\nSQL: %s", err, statement)
		}
	}
	var migrationColumnCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = '_kora_extension' AND column_name = 'managed_idempotency_key'`, schemaName).Scan(&migrationColumnCount); err != nil {
		t.Fatal("verify PostgreSQL idempotency migration:", err)
	}
	if migrationColumnCount != 1 {
		t.Fatal("existing PostgreSQL extension schema was not upgraded with managed_idempotency_key")
	}
	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})
	identityRecorder := httptest.NewRecorder()
	identityContext := extensionTestContext(identityRecorder, database, registry, "site-a", http.MethodGet, "", "")
	identityContext.Set("auth_type", "engine_provisioner")
	identityContext.Set("site_id", "engine-site-a")
	identityContext.Set("site_status", "active")
	identityContext.Set("site_config_revision", uint64(9))
	handler.HandleManagedSiteIdentity(identityContext)
	if identityRecorder.Code != http.StatusOK {
		t.Fatalf("managed site identity returned HTTP %d: %s", identityRecorder.Code, identityRecorder.Body.String())
	}
	var identityPayload struct {
		Data managedSiteIdentityResponse `json:"data"`
	}
	if err := json.Unmarshal(identityRecorder.Body.Bytes(), &identityPayload); err != nil {
		t.Fatal("decode managed site identity:", err)
	}
	if identityPayload.Data.SiteID != "engine-site-a" || identityPayload.Data.Status != "active" || !identityPayload.Data.Healthy || identityPayload.Data.ConfigRevision != 9 {
		t.Fatalf("unexpected managed site identity: %+v", identityPayload.Data)
	}
	auditContext := extensionTestContext(httptest.NewRecorder(), database, registry, "site-a", http.MethodPost, "", "")
	if err := handler.insertChannelAudit(auditContext, "read_catalog", "query", "success", "{}", "3 rows", ""); err != nil {
		t.Fatalf("live PostgreSQL channel audit insert failed: %v", err)
	}
	var auditCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_channel_audit WHERE site = $1`, "site-a").Scan(&auditCount); err != nil {
		t.Fatal("read channel audit fixture:", err)
	}
	if auditCount != 1 {
		t.Fatalf("channel audit count = %d, want 1", auditCount)
	}

	createRecorder := httptest.NewRecorder()
	create := extensionTestContext(createRecorder, database, registry, "site-a", http.MethodPost, `{"name":"demo-ext","display_name":"Demo","description":"Demo extension","endpoint_url":"https://example.test/hook","subscriptions":"[]","api_permissions":"[]"}`, "")
	handler.HandleExtensionCreate(create)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("live PostgreSQL extension create returned HTTP %d: %s", createRecorder.Code, createRecorder.Body.String())
	}
	managedFirst := httptest.NewRecorder()
	managedContext := extensionTestContext(managedFirst, database, registry, "site-a", http.MethodPost, "{}", "")
	managedContext.Set("auth_type", "engine_provisioner")
	managedContext.Request.Header.Set("Idempotency-Key", "integration-attempt-1")
	handler.HandleManagedChannelClientRotate(managedContext)
	if managedFirst.Code != http.StatusOK {
		t.Fatalf("managed channel credential issuance returned HTTP %d: %s", managedFirst.Code, managedFirst.Body.String())
	}
	var managedPayload struct {
		Data extensionCreatedResponse `json:"data"`
	}
	if err := json.Unmarshal(managedFirst.Body.Bytes(), &managedPayload); err != nil {
		t.Fatal("decode managed channel credential response:", err)
	}
	var storedManagedToken string
	if err := database.QueryRow(`SELECT access_token FROM _kora_extension WHERE site = $1 AND name = $2`, "site-a", "kora-cloud-channel").Scan(&storedManagedToken); err != nil {
		t.Fatal("read managed channel credential:", err)
	}
	if managedPayload.Data.AccessToken == "" || storedManagedToken != managedPayload.Data.AccessToken {
		t.Fatal("Engine did not persist the returned managed channel credential")
	}
	managedReplay := httptest.NewRecorder()
	managedContext = extensionTestContext(managedReplay, database, registry, "site-a", http.MethodPost, "{}", "")
	managedContext.Set("auth_type", "engine_provisioner")
	managedContext.Request.Header.Set("Idempotency-Key", "integration-attempt-1")
	handler.HandleManagedChannelClientRotate(managedContext)
	if managedReplay.Code != http.StatusOK {
		t.Fatalf("managed channel credential replay returned HTTP %d: %s", managedReplay.Code, managedReplay.Body.String())
	}
	if err := database.QueryRow(`SELECT access_token FROM _kora_extension WHERE site = $1 AND name = $2`, "site-a", "kora-cloud-channel").Scan(&storedManagedToken); err != nil {
		t.Fatal("read replayed managed channel credential:", err)
	}
	var replayPayload struct {
		Data extensionCreatedResponse `json:"data"`
	}
	if err := json.Unmarshal(managedReplay.Body.Bytes(), &replayPayload); err != nil {
		t.Fatal("decode managed channel replay:", err)
	}
	if replayPayload.Data.AccessToken != managedPayload.Data.AccessToken || storedManagedToken != managedPayload.Data.AccessToken {
		t.Fatal("same idempotency key did not return the persisted credential")
	}
	managedSecond := httptest.NewRecorder()
	managedContext = extensionTestContext(managedSecond, database, registry, "site-a", http.MethodPost, "{}", "")
	managedContext.Set("auth_type", "engine_provisioner")
	managedContext.Request.Header.Set("Idempotency-Key", "integration-attempt-2")
	handler.HandleManagedChannelClientRotate(managedContext)
	if managedSecond.Code != http.StatusOK {
		t.Fatalf("new managed credential rotation returned HTTP %d: %s", managedSecond.Code, managedSecond.Body.String())
	}
	if err := database.QueryRow(`SELECT access_token FROM _kora_extension WHERE site = $1 AND name = $2`, "site-a", "kora-cloud-channel").Scan(&storedManagedToken); err != nil {
		t.Fatal("read rotated managed channel credential:", err)
	}
	if storedManagedToken == managedPayload.Data.AccessToken {
		t.Fatal("new idempotency key did not rotate the managed channel credential")
	}
	if _, err := database.Exec(`INSERT INTO _kora_webhook_delivery (id, site, extension_name, event_id, event_type, endpoint_url, status, attempt, response_status, error_message, duration_ms) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11),($12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`,
		"delivery-site-a", "site-a", "demo-ext", "event-a", "doc.updated", "https://a.test/hook", "delivered", 1, 200, "", 12,
		"delivery-site-b", "site-b", "demo-ext", "event-b", "doc.updated", "https://b.test/hook", "delivered", 1, 200, "", 10); err != nil {
		t.Fatal("insert webhook delivery fixtures:", err)
	}

	for _, site := range []struct {
		name string
		want int
	}{{"site-a", 2}, {"site-b", 0}} {
		recorder := httptest.NewRecorder()
		handler.HandleExtensionList(extensionTestContext(recorder, database, registry, site.name, http.MethodGet, "", ""))
		if recorder.Code != http.StatusOK {
			t.Fatalf("extension list for %s returned HTTP %d: %s", site.name, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data extensionListResponse `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal("decode extension list:", err)
		}
		if len(response.Data.Extensions) != site.want {
			t.Fatalf("extension list for %s has %d records, want %d", site.name, len(response.Data.Extensions), site.want)
		}
	}

	for _, site := range []struct {
		name string
		want int
	}{{"site-a", 1}, {"site-b", 1}} {
		recorder := httptest.NewRecorder()
		handler.HandleExtensionDeliveries(extensionTestContext(recorder, database, registry, site.name, http.MethodGet, "", "demo-ext"))
		if recorder.Code != http.StatusOK {
			t.Fatalf("extension delivery list for %s returned HTTP %d: %s", site.name, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Data extensionDeliveriesResponse `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal("decode extension deliveries:", err)
		}
		if len(response.Data.Deliveries) != site.want {
			t.Fatalf("extension delivery list for %s has %d records, want %d", site.name, len(response.Data.Deliveries), site.want)
		}
	}

	foreignRotate := httptest.NewRecorder()
	handler.HandleExtensionRotateSecret(extensionTestContext(foreignRotate, database, registry, "site-b", http.MethodPost, "", "demo-ext"))
	if foreignRotate.Code != http.StatusNotFound {
		t.Fatalf("cross-site secret rotation returned HTTP %d, want 404: %s", foreignRotate.Code, foreignRotate.Body.String())
	}
	rotate := httptest.NewRecorder()
	handler.HandleExtensionRotateSecret(extensionTestContext(rotate, database, registry, "site-a", http.MethodPost, "", "demo-ext"))
	if rotate.Code != http.StatusOK {
		t.Fatalf("extension secret rotation returned HTTP %d: %s", rotate.Code, rotate.Body.String())
	}
	var secretCount int
	if err := database.QueryRow(`SELECT secret_count FROM _kora_extension WHERE site = $1 AND name = $2`, "site-a", "demo-ext").Scan(&secretCount); err != nil {
		t.Fatal("read extension secret count:", err)
	}
	if secretCount != 2 {
		t.Fatalf("extension secret count = %d, want 2 after rotation", secretCount)
	}

	foreignDelete := httptest.NewRecorder()
	handler.HandleExtensionDelete(extensionTestContext(foreignDelete, database, registry, "site-b", http.MethodDelete, "", "demo-ext"))
	if foreignDelete.Code != http.StatusNotFound {
		t.Fatalf("cross-site extension delete returned HTTP %d, want 404: %s", foreignDelete.Code, foreignDelete.Body.String())
	}
	deleteRecorder := httptest.NewRecorder()
	handler.HandleExtensionDelete(extensionTestContext(deleteRecorder, database, registry, "site-a", http.MethodDelete, "", "demo-ext"))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("extension delete returned HTTP %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	var remainingA, remainingB int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_webhook_delivery WHERE site = $1`, "site-a").Scan(&remainingA); err != nil {
		t.Fatal("count deleted site's webhook deliveries:", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_webhook_delivery WHERE site = $1`, "site-b").Scan(&remainingB); err != nil {
		t.Fatal("count other site's webhook deliveries:", err)
	}
	if remainingA != 0 || remainingB != 1 {
		t.Fatalf("webhook deliveries after delete: site-a=%d site-b=%d, want 0 and 1", remainingA, remainingB)
	}
}

func extensionTestContext(recorder *httptest.ResponseRecorder, database *sql.DB, registry *doctype.Registry, site, method, body, name string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/api/extensions", strings.NewReader(body))
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", site)
	ctx.Set("site_db_type", "postgres")
	ctx.Set("user", "test-user")
	ctx.Set("user_roles", []string{"Administrator"})
	if name != "" {
		ctx.Params = gin.Params{{Key: "name", Value: name}}
	}
	return ctx
}
