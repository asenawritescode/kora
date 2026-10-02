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

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/conversation"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

// TestLivePostgresConfigVersionRoutes is opt-in and must use a disposable DB.
// It creates and removes an isolated schema; set KORA_API_LIVE_POSTGRES_DSN.
func TestLivePostgresConfigVersionRoutes(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test config-version API routes against a disposable PostgreSQL database")
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

	schemaName := fmt.Sprintf("kora_api_versions_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	_, err = database.Exec(`CREATE TABLE _kora_config_version (
		id TEXT PRIMARY KEY, site TEXT NOT NULL, version INTEGER NOT NULL,
		created_at TIMESTAMPTZ NOT NULL, created_by TEXT NOT NULL, label TEXT NOT NULL,
		status TEXT, is_active SMALLINT NOT NULL DEFAULT 0,
		config TEXT NOT NULL DEFAULT '', changelog JSONB,
		change_list TEXT, config_hash TEXT NOT NULL DEFAULT '',
		base_version_id TEXT NOT NULL DEFAULT '', min_kora_version TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		t.Fatal("create config-version fixture table:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, status, is_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, "draft-pg", "site-pg", 1, "2026-09-29T00:00:00Z", "tester", "Draft", "Draft", 0); err != nil {
		t.Fatal("insert config-version fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_config_version (id, site, version, created_at, created_by, label, status, config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, "view-draft-pg", "site-pg", 2, "2026-09-29T00:00:01Z", "tester", "Draft view", "Draft", `{"views":[{"name":"Draft register","route":"/draft-register"}]}`); err != nil {
		t.Fatal("insert draft view fixture:", err)
	}

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})
	listRecorder := httptest.NewRecorder()
	listContext := postgresConfigVersionContext(listRecorder, database, registry, http.MethodGet)
	handler.HandleConfigVersions(listContext)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL config-version list returned HTTP %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	draftViewRecorder := httptest.NewRecorder()
	draftViewContext := postgresConfigVersionContext(draftViewRecorder, database, registry, http.MethodGet)
	draftViewContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/views?route=%2Fdraft-register&version=draft", nil)
	handler.HandleViewByRoute(draftViewContext)
	if draftViewRecorder.Code != http.StatusOK || !strings.Contains(draftViewRecorder.Body.String(), "Draft register") {
		t.Fatalf("live PostgreSQL draft view lookup returned HTTP %d: %s", draftViewRecorder.Code, draftViewRecorder.Body.String())
	}

	discardRecorder := httptest.NewRecorder()
	discardContext := postgresConfigVersionContext(discardRecorder, database, registry, http.MethodPost)
	discardContext.Params = gin.Params{{Key: "id", Value: "draft-pg"}}
	handler.HandleConfigVersionDiscard(discardContext)
	if discardRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL config-version discard returned HTTP %d: %s", discardRecorder.Code, discardRecorder.Body.String())
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM _kora_config_version WHERE id = $1`, "draft-pg").Scan(&status); err != nil {
		t.Fatal("read discarded config version:", err)
	}
	if status != "Superseded" {
		t.Fatalf("discarded config version status = %q, want Superseded", status)
	}

	// Activate a Draft over an existing Active version, then roll back to that
	// superseded snapshot. This exercises boolean writes, SQL rebinding, and
	// truthful state transitions through the live PostgreSQL routes.
	emptySnapshot := doctype.ToSExpr(&doctype.ConfigSnapshot{})
	_, err = database.Exec(`INSERT INTO _kora_config_version
		(id, site, version, created_at, created_by, label, status, is_active, config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		"active-before-pg", "site-pg", 3, time.Now().UTC(), "tester", "Previously active", "Active", 1, emptySnapshot)
	if err != nil {
		t.Fatal("insert active config version:", err)
	}
	_, err = database.Exec(`INSERT INTO _kora_config_version
		(id, site, version, created_at, created_by, label, status, is_active, config)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		"activate-draft-pg", "site-pg", 4, time.Now().UTC(), "tester", "Activation draft", "Draft", 0, emptySnapshot)
	if err != nil {
		t.Fatal("insert activation draft:", err)
	}
	activateRecorder := httptest.NewRecorder()
	activateContext := postgresConfigVersionContext(activateRecorder, database, registry, http.MethodPost)
	activateContext.Params = gin.Params{{Key: "id", Value: "activate-draft-pg"}}
	handler.HandleConfigVersionActivate(activateContext)
	if activateRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL config-version activation returned HTTP %d: %s", activateRecorder.Code, activateRecorder.Body.String())
	}
	var activeCount, staleActiveFlagCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_config_version WHERE site = $1 AND status = 'Active' AND is_active = 1`, "site-pg").Scan(&activeCount); err != nil {
		t.Fatal("count active versions after activation:", err)
	}
	if activeCount != 1 {
		t.Fatalf("active config version count after activation = %d, want exactly one", activeCount)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_config_version WHERE id = $1 AND status = 'Superseded' AND is_active = 1`, "active-before-pg").Scan(&staleActiveFlagCount); err != nil {
		t.Fatal("check superseded version flag:", err)
	}
	if staleActiveFlagCount != 0 {
		t.Fatal("superseded version retained its active flag")
	}

	rollbackRecorder := httptest.NewRecorder()
	rollbackContext := postgresConfigVersionContext(rollbackRecorder, database, registry, http.MethodPost)
	rollbackContext.Params = gin.Params{{Key: "id", Value: "active-before-pg"}}
	handler.HandleConfigVersionRollback(rollbackContext)
	if rollbackRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL config-version rollback returned HTTP %d: %s", rollbackRecorder.Code, rollbackRecorder.Body.String())
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_config_version WHERE site = $1 AND status = 'Active' AND is_active = 1`, "site-pg").Scan(&activeCount); err != nil {
		t.Fatal("count active versions after rollback:", err)
	}
	if activeCount != 1 {
		t.Fatalf("active config version count after rollback = %d, want exactly one", activeCount)
	}

	_, err = database.Exec(`INSERT INTO _kora_config_version
		(id, site, version, created_at, created_by, label, status, is_active, config)
		SELECT 'activate-fails-pg', 'site-pg', COALESCE(MAX(version), 0) + 1, NOW(), 'tester', 'Failure injection', 'Draft', 0, $1
		FROM _kora_config_version WHERE site = 'site-pg'`, emptySnapshot)
	if err != nil {
		t.Fatal("insert failure-injection draft:", err)
	}
	_, err = database.Exec(`CREATE FUNCTION reject_config_activation_version() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.status = 'Active' AND NEW.label LIKE 'Activated version %' THEN
				RAISE EXCEPTION 'intentional active-version write failure';
			END IF;
			RETURN NEW;
		END;
	$$;
	CREATE TRIGGER reject_config_activation_version BEFORE INSERT ON _kora_config_version
		FOR EACH ROW EXECUTE FUNCTION reject_config_activation_version()`)
	if err != nil {
		t.Fatal("install activation failure-injection trigger:", err)
	}
	failRecorder := httptest.NewRecorder()
	failContext := postgresConfigVersionContext(failRecorder, database, registry, http.MethodPost)
	failContext.Params = gin.Params{{Key: "id", Value: "activate-fails-pg"}}
	handler.HandleConfigVersionActivate(failContext)
	if failRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("activation with active-history write failure returned HTTP %d, want 500: %s", failRecorder.Code, failRecorder.Body.String())
	}
	var failedDraftStatus string
	if err := database.QueryRow(`SELECT status FROM _kora_config_version WHERE id = $1`, "activate-fails-pg").Scan(&failedDraftStatus); err != nil {
		t.Fatal("read failed activation draft status:", err)
	}
	if failedDraftStatus != "Draft" {
		t.Fatalf("failed activation changed draft status to %q, want Draft", failedDraftStatus)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM _kora_config_version WHERE site = $1 AND status = 'Active' AND is_active = 1`, "site-pg").Scan(&activeCount); err != nil {
		t.Fatal("count active versions after injected failure:", err)
	}
	if activeCount != 1 {
		t.Fatalf("failed activation left %d active versions, want the previous single active version", activeCount)
	}

	_, err = database.Exec(`CREATE TABLE _kora_user (roles TEXT NOT NULL);
		CREATE TABLE _kora_role (name TEXT PRIMARY KEY);
		CREATE TABLE _kora_permission (role TEXT NOT NULL);
		INSERT INTO _kora_user (roles) VALUES ('Store Manager,Manager'), ('Assistant Store Manager');
		INSERT INTO _kora_role (name) VALUES ('Store Manager');
		INSERT INTO _kora_permission (role) VALUES ('Store Manager')`)
	if err != nil {
		t.Fatal("create role-delete fixtures:", err)
	}
	roleRecorder := httptest.NewRecorder()
	roleContext := postgresConfigVersionContext(roleRecorder, database, registry, http.MethodDelete)
	roleContext.Params = gin.Params{{Key: "name", Value: "Store Manager"}}
	handler.HandleSystemRoleDelete(roleContext)
	if roleRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL role delete returned HTTP %d: %s", roleRecorder.Code, roleRecorder.Body.String())
	}
	var roleResponse struct {
		Data struct {
			UsersWithRole int `json:"users_with_role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(roleRecorder.Body.Bytes(), &roleResponse); err != nil {
		t.Fatal("decode PostgreSQL role-delete response:", err)
	}
	if roleResponse.Data.UsersWithRole != 1 {
		t.Fatalf("PostgreSQL role-delete user count = %d, want exact-token count 1", roleResponse.Data.UsersWithRole)
	}

	if _, err := database.Exec(`DROP TABLE _kora_config_version`); err != nil {
		t.Fatal("remove config-version table for database-error response check:", err)
	}
	draftViewFailureRecorder := httptest.NewRecorder()
	draftViewFailureContext := postgresConfigVersionContext(draftViewFailureRecorder, database, registry, http.MethodGet)
	draftViewFailureContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/views?route=%2Fregister&version=draft", nil)
	handler.HandleViewByRoute(draftViewFailureContext)
	if draftViewFailureRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("draft view preview database error returned HTTP %d, want 500: %s", draftViewFailureRecorder.Code, draftViewFailureRecorder.Body.String())
	}
	viewFailureRecorder := httptest.NewRecorder()
	viewFailureContext := postgresConfigVersionContext(viewFailureRecorder, database, registry, http.MethodGet)
	viewFailureContext.Params = gin.Params{{Key: "name", Value: "register"}}
	handler.HandleSystemView(viewFailureContext)
	if viewFailureRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("view lookup database error returned HTTP %d, want 500: %s", viewFailureRecorder.Code, viewFailureRecorder.Body.String())
	}
	failedPreviewRecorder := httptest.NewRecorder()
	failedPreviewContext := postgresConfigVersionContext(failedPreviewRecorder, database, registry, http.MethodGet)
	failedPreviewContext.Params = gin.Params{{Key: "id", Value: "missing-because-table-is-unavailable"}}
	handler.HandleConfigVersionPreview(failedPreviewContext)
	if failedPreviewRecorder.Code != http.StatusInternalServerError {
		t.Fatalf("config preview database error returned HTTP %d, want 500 (not a false 404): %s", failedPreviewRecorder.Code, failedPreviewRecorder.Body.String())
	}
}

func TestLivePostgresConversationRoutes(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test conversation API routes against a disposable PostgreSQL database")
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

	schemaName := fmt.Sprintf("kora_api_conversations_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	for _, statement := range kdb.ConversationTablesPostgres() {
		if _, err := database.Exec(statement); err != nil {
			t.Fatalf("create conversation fixture table: %v", err)
		}
	}

	registry := doctype.NewRegistry()
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: kdb.Resolve("postgres")})
	createRecorder := httptest.NewRecorder()
	createContext := postgresConversationContext(createRecorder, database, registry, http.MethodPost, `{"channel":"web","external_contact_id":"tester@example.test","correlation_id":"pg-conversation-1"}`, "", "")
	handler.HandleConversationCreate(createContext)
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("live PostgreSQL conversation create returned HTTP %d: %s", createRecorder.Code, createRecorder.Body.String())
	}
	var createResponse struct {
		Data conversation.Conversation `json:"data"`
	}
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &createResponse); err != nil {
		t.Fatal("decode created conversation:", err)
	}
	conversationID := createResponse.Data.ID
	if conversationID == "" {
		t.Fatal("conversation create returned an empty id")
	}
	idempotentRecorder := httptest.NewRecorder()
	idempotentContext := postgresConversationContext(idempotentRecorder, database, registry, http.MethodPost, `{"channel":"web","external_contact_id":"tester@example.test","correlation_id":"pg-conversation-1"}`, "", "")
	handler.HandleConversationCreate(idempotentContext)
	if idempotentRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL idempotent conversation create returned HTTP %d: %s", idempotentRecorder.Code, idempotentRecorder.Body.String())
	}
	var idempotentResponse struct {
		Data conversation.Conversation `json:"data"`
	}
	if err := json.Unmarshal(idempotentRecorder.Body.Bytes(), &idempotentResponse); err != nil {
		t.Fatal("decode idempotent conversation response:", err)
	}
	if idempotentResponse.Data.ID != conversationID {
		t.Fatalf("idempotent create returned conversation %q, want original %q", idempotentResponse.Data.ID, conversationID)
	}

	messageRecorder := httptest.NewRecorder()
	messageContext := postgresConversationContext(messageRecorder, database, registry, http.MethodPost, `{"text":"Track stock levels","question_key":"business"}`, conversationID, "")
	handler.HandleConversationMessage(messageContext)
	if messageRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL conversation message returned HTTP %d: %s", messageRecorder.Code, messageRecorder.Body.String())
	}

	confirmRecorder := httptest.NewRecorder()
	confirmContext := postgresConversationContext(confirmRecorder, database, registry, http.MethodPost, "", conversationID, "business")
	handler.HandleConversationConfirm(confirmContext)
	if confirmRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL answer confirmation returned HTTP %d: %s", confirmRecorder.Code, confirmRecorder.Body.String())
	}

	getRecorder := httptest.NewRecorder()
	getContext := postgresConversationContext(getRecorder, database, registry, http.MethodGet, "", conversationID, "")
	handler.HandleConversationGet(getContext)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("live PostgreSQL conversation get returned HTTP %d: %s", getRecorder.Code, getRecorder.Body.String())
	}

	supportRecorder := httptest.NewRecorder()
	supportContext := postgresConversationContext(supportRecorder, database, registry, http.MethodPost, `{"reason":"password=do-not-store please help","confirm":true}`, conversationID, "")
	handler.HandleConversationSupport(supportContext)
	if supportRecorder.Code != http.StatusCreated {
		t.Fatalf("live PostgreSQL support ticket returned HTTP %d: %s", supportRecorder.Code, supportRecorder.Body.String())
	}
	var safeSummary string
	if err := database.QueryRow(`SELECT safe_summary FROM _kora_support_ticket WHERE conversation_id = $1`, conversationID).Scan(&safeSummary); err != nil {
		t.Fatal("read PostgreSQL support summary:", err)
	}
	if strings.Contains(safeSummary, "do-not-store") || !strings.Contains(safeSummary, "[redacted]") {
		t.Fatalf("support summary did not redact secret: %q", safeSummary)
	}
}

func postgresConversationContext(recorder *httptest.ResponseRecorder, database *sql.DB, registry *doctype.Registry, method, body, id, key string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/api/cloud/conversations", strings.NewReader(body))
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", "site-pg")
	ctx.Set("site_db_type", "postgres")
	ctx.Set("user", "test-user")
	ctx.Set("user_roles", []string{"Administrator"})
	if id != "" {
		ctx.Params = append(ctx.Params, gin.Param{Key: "id", Value: id})
	}
	if key != "" {
		ctx.Params = append(ctx.Params, gin.Param{Key: "key", Value: key})
	}
	return ctx
}

func TestLivePostgresAnalyticsMetadataQueries(t *testing.T) {
	dsn := os.Getenv("KORA_API_LIVE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set KORA_API_LIVE_POSTGRES_DSN to test analytics metadata queries against a disposable PostgreSQL database")
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
	schemaName := fmt.Sprintf("kora_api_analytics_%d", time.Now().UnixNano())
	if _, err := database.Exec(`CREATE SCHEMA "` + schemaName + `"`); err != nil {
		t.Fatal("create isolated PostgreSQL schema:", err)
	}
	defer func() { _, _ = database.Exec(`DROP SCHEMA "` + schemaName + `" CASCADE`) }()
	if _, err := database.Exec(`SET search_path TO "` + schemaName + `"`); err != nil {
		t.Fatal("select isolated PostgreSQL schema:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_config_version (site TEXT NOT NULL, status TEXT NOT NULL, version INTEGER NOT NULL, config TEXT NOT NULL)`); err != nil {
		t.Fatal("create config-version fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_analytics_metric (site TEXT NOT NULL, name TEXT NOT NULL, label TEXT NOT NULL, type TEXT NOT NULL, doctype TEXT NOT NULL, field_name TEXT NOT NULL, link_field TEXT NOT NULL, group_by_field TEXT NOT NULL, UNIQUE (site, name))`); err != nil {
		t.Fatal("create analytics metric fixture:", err)
	}
	if _, err := database.Exec(`CREATE TABLE _kora_analytics_daily (site TEXT NOT NULL, doctype TEXT NOT NULL, metric TEXT NOT NULL, dimension TEXT NOT NULL, date DATE NOT NULL, value DOUBLE PRECISION NOT NULL)`); err != nil {
		t.Fatal("create analytics daily fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_analytics_metric (site, name, label, type, doctype, field_name, link_field, group_by_field) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, "site-pg", "sales_total", "Sales Total", "sum", "Sale", "total", "", ""); err != nil {
		t.Fatal("insert analytics metric fixture:", err)
	}
	if _, err := database.Exec(`INSERT INTO _kora_analytics_daily (site, doctype, metric, dimension, date, value) VALUES ($1,$2,$3,$4,CURRENT_DATE,$5)`, "site-pg", "Sale", "sales_total", "", 27.5); err != nil {
		t.Fatal("insert analytics rollup fixture:", err)
	}
	registry := doctype.NewRegistry()
	ctx := postgresConversationContext(httptest.NewRecorder(), database, registry, http.MethodGet, "", "", "")
	reports, err := loadSemanticReports(ctx, database, kdb.Resolve("postgres"))
	if err != nil {
		t.Fatalf("live PostgreSQL semantic report lookup failed: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("semantic report lookup returned %d unexpected reports", len(reports))
	}
	metrics := resolveMetrics(ctx, registry, kdb.Resolve("postgres"))
	if len(metrics) != 1 || metrics[0].Name != "sales_total" {
		t.Fatalf("live PostgreSQL custom metrics = %#v, want sales_total", metrics)
	}
	today := time.Now().Format("2006-01-02")
	result, err := (&analytics.QueryEngine{DB: database, SiteName: "site-pg", Dialect: kdb.Resolve("postgres")}).Resolve(metrics[0], analytics.QueryRequest{From: today, To: today})
	if err != nil {
		t.Fatalf("live PostgreSQL analytics rollup query failed: %v", err)
	}
	if result.Total != 1 || len(result.Rows) != 1 || result.Rows[0]["value"] != float64(27.5) {
		t.Fatalf("live PostgreSQL analytics result = %#v, want value 27.5", result)
	}

	analyticsRouter := gin.New()
	analyticsRouter.Use(func(c *gin.Context) {
		c.Set("site_db", database)
		c.Set("site_registry", registry)
		c.Set("site_name", "site-pg")
		c.Set("site_db_type", "postgres")
		c.Next()
	})
	RegisterAnalyticsRoutes(analyticsRouter.Group("/api"), registry, database, nil, nil, kdb.Resolve("postgres"))
	createMetric := httptest.NewRecorder()
	createMetricRequest := httptest.NewRequest(http.MethodPost, "/api/analytics/metrics", strings.NewReader(`{"name":"units_sold","label":"Units Sold","type":"sum","doctype":"Sale","field":"quantity"}`))
	createMetricRequest.Header.Set("Content-Type", "application/json")
	analyticsRouter.ServeHTTP(createMetric, createMetricRequest)
	if createMetric.Code != http.StatusCreated {
		t.Fatalf("live PostgreSQL custom metric create returned HTTP %d: %s", createMetric.Code, createMetric.Body.String())
	}
	var metricLabel string
	if err := database.QueryRow(`SELECT label FROM _kora_analytics_metric WHERE site = $1 AND name = $2`, "site-pg", "units_sold").Scan(&metricLabel); err != nil {
		t.Fatal("read inserted PostgreSQL custom metric:", err)
	}
	if metricLabel != "Units Sold" {
		t.Fatalf("stored PostgreSQL custom metric label = %q, want Units Sold", metricLabel)
	}
}

func postgresConfigVersionContext(recorder *httptest.ResponseRecorder, database *sql.DB, registry *doctype.Registry, method string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/api/system/config/versions", nil)
	ctx.Set("site_db", database)
	ctx.Set("site_registry", registry)
	ctx.Set("site_name", "site-pg")
	ctx.Set("site_db_type", "postgres")
	ctx.Set("user", "test-user")
	ctx.Set("user_roles", []string{"Administrator"})
	return ctx
}
