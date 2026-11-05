package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
)

func TestAuthenticateExtension_LoadsPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow(
		"test-bot", `[{"doctype":"Work Order","read":true,"create":true}]`,
	)
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("test.local", HashExtensionAccessToken("valid-token")).WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "test.local")
	c.Request = httptest.NewRequest("GET", "/api/v1/resource/Work%20Order", nil)
	c.Request.Header.Set("Authorization", "Bearer valid-token")

	guard := &SiteGuard{}
	ok := guard.authenticateExtension(c, "valid-token")

	if !ok {
		t.Errorf("expected authentication to succeed")
	}
	if c.GetString("extension_name") != "test-bot" {
		t.Errorf("expected extension_name 'test-bot', got %q", c.GetString("extension_name"))
	}
	perms, exists := c.Get("extension_permissions")
	if !exists {
		t.Errorf("expected extension_permissions in context")
	}
	p, ok := perms.([]doctype.Permission)
	if !ok {
		t.Fatalf("extension_permissions is not []doctype.Permission")
	}
	if len(p) != 1 {
		t.Errorf("expected 1 permission entry, got %d", len(p))
	}
	if p[0].Doctype != "Work Order" {
		t.Errorf("expected doctype Work Order, got %q", p[0].Doctype)
	}
	if !p[0].Read {
		t.Errorf("expected Read=true")
	}
	if !p[0].Create {
		t.Errorf("expected Create=true")
	}
	if p[0].Write {
		t.Errorf("expected Write=false")
	}
	verified, exists := c.Get("policy_execution")
	execution, executionOK := verified.(policy.Execution)
	if !exists || !executionOK || !execution.Valid() || !execution.Allows("Work Order", "read") || !execution.Allows("Work Order", "create") || execution.Allows("Work Order", "write") {
		t.Errorf("extension execution did not preserve its verified scoped grants: %#v", verified)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestDelegatedExecutionRetainsLegalEntityGrantScope(t *testing.T) {
	scope := contract.BusinessScope{TenantID: "tenant-a", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	execution, err := delegatedExecution("tenant-a", "tenant-a.example.test", contract.ActorContext{PrincipalID: "extension:books", PrincipalType: contract.PrincipalService}, []doctype.Permission{{Doctype: "Invoice", Read: true, Scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	if execution.Allows("Invoice", "read") {
		t.Fatal("entity-scoped credential grant became tenant-wide")
	}
	if _, ok := execution.GrantForScope("Invoice", "read", scope); !ok {
		t.Fatal("entity-scoped credential grant was lost")
	}
}

func TestAuthenticateExtensionExecutionUsesDialectAndExactOwnerGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dbType string
		query  string
	}{
		{name: "mysql", dbType: "mysql", query: `SELECT name, api_permissions FROM _kora_extension WHERE site = ? AND access_token_hash = ? AND is_active = 1`},
		{name: "postgres", dbType: "postgres", query: `SELECT name, api_permissions FROM _kora_extension WHERE site = $1 AND access_token_hash = $2 AND is_active = 1`},
		{name: "libsql", dbType: "libsql", query: `SELECT name, api_permissions FROM _kora_extension WHERE site = ? AND access_token_hash = ? AND is_active = 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer database.Close()

			mock.ExpectQuery(regexp.QuoteMeta(tc.query)).
				WithArgs("tenant.example", HashExtensionAccessToken("extension-token")).
				WillReturnRows(sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow(
					"invoice-reader", `[{"doctype":"Invoice","read":true,"if_owner":true}]`,
				))
			execution, permissions, err := AuthenticateExtensionExecution(context.Background(), database, kdb.Resolve(tc.dbType), "stable-tenant-id", "tenant.example", "extension-token")
			if err != nil {
				t.Fatalf("AuthenticateExtensionExecution: %v", err)
			}
			if execution.Tenant() != "stable-tenant-id" {
				t.Fatalf("execution tenant = %q, want stable tenant identity", execution.Tenant())
			}
			actor := execution.Actor()
			if actor.PrincipalID != "extension:invoice-reader" || actor.PrincipalType != contract.PrincipalService || actor.Site != "tenant.example" {
				t.Fatalf("execution actor = %#v", actor)
			}
			if len(permissions) != 1 || permissions[0].Doctype != "Invoice" || !permissions[0].Read || !permissions[0].IfOwner {
				t.Fatalf("extension permissions = %#v", permissions)
			}
			grant, ok := execution.GrantFor("Invoice", "read")
			if !ok || !grant.OwnerOnly || execution.Allows("Invoice", "write") {
				t.Fatalf("extension grants = %#v, read=%t write=%t", grant, ok, execution.Allows("Invoice", "write"))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSiteGuardExtensionAuthenticationUsesPostgresPlaceholders(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer database.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT name, api_permissions FROM _kora_extension WHERE site = $1 AND access_token_hash = $2 AND is_active = 1`)).
		WithArgs("tenant.example", HashExtensionAccessToken("extension-token")).
		WillReturnRows(sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("reader", `[{"doctype":"Invoice","read":true}]`))

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("site_db", database)
	ctx.Set("site_db_type", "postgres")
	ctx.Set("site_id", "stable-tenant-id")
	ctx.Set("site_name", "tenant.example")
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if !(&SiteGuard{}).authenticateExtension(ctx, "extension-token") {
		t.Fatal("SiteGuard rejected a valid PostgreSQL extension credential")
	}
	verified, exists := ctx.Get("policy_execution")
	execution, ok := verified.(policy.Execution)
	if !exists || !ok || execution.Tenant() != "stable-tenant-id" || !execution.Allows("Invoice", "read") {
		t.Fatalf("SiteGuard execution = %#v", verified)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("unmet expectations:", err)
	}
}

func TestConfigServiceTokenCreatesTenantBoundVerifiedExecution(t *testing.T) {
	t.Setenv("KORA_ENGINE_CONFIG_TOKEN", "config-secret")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_id", "tenant-stable-1")
		c.Set("site_name", "tenant-route")
		c.Next()
	})
	router.Use((&SiteGuard{}).Middleware(true))
	router.POST("/api/system/config/drafts", func(c *gin.Context) {
		value, exists := c.Get("policy_execution")
		execution, ok := value.(policy.Execution)
		if !exists || !ok || !execution.Valid() || execution.Tenant() != contract.TenantID("tenant-stable-1") {
			c.Status(http.StatusUnauthorized)
			return
		}
		actor := execution.Actor()
		if actor.PrincipalID != "cloud-proposal" || actor.PrincipalType != contract.PrincipalService || len(actor.Roles) != 1 || actor.Roles[0] != doctype.AdminRole {
			c.Status(http.StatusForbidden)
			return
		}
		c.Status(http.StatusNoContent)
	})
	router.GET("/api/system/config/versions/head/snapshot", func(c *gin.Context) {
		value, exists := c.Get("policy_execution")
		execution, ok := value.(policy.Execution)
		if !exists || !ok || !execution.Valid() || execution.Tenant() != contract.TenantID("tenant-stable-1") {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.Status(http.StatusNoContent)
	})
	router.GET("/api/system/config/versions/active/snapshot", func(c *gin.Context) {
		value, exists := c.Get("policy_execution")
		execution, ok := value.(policy.Execution)
		if !exists || !ok || !execution.Valid() || execution.Tenant() != contract.TenantID("tenant-stable-1") {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/api/system/config/drafts", nil)
	request.Header.Set("Authorization", "Bearer config-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("config service identity status=%d body=%s, want 204", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/system/config/versions/head/snapshot", nil)
	request.Header.Set("Authorization", "Bearer config-secret")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("config snapshot service identity status=%d body=%s, want 204", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/api/system/config/versions/active/snapshot", nil)
	request.Header.Set("Authorization", "Bearer config-secret")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("active config snapshot service identity status=%d body=%s, want 204", recorder.Code, recorder.Body.String())
	}
}

func TestRevalidateRealtimeIdentityReloadsSessionRoles(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	manager := NewOwnedSessionManager(db)
	defer manager.Close()

	userJSON := `{"name":"alice","email":"alice@example.test","roles":["CurrentReader"]}`
	expires := time.Now().Add(time.Hour).Format("2006-01-02 15:04:05")
	created := time.Now().Add(-time.Minute).Format("2006-01-02 15:04:05")
	mock.ExpectQuery(`SELECT data, expires_at, created_at FROM _kora_session WHERE site = \? AND sid = \?`).
		WithArgs("tenant-1", "sid-1").
		WillReturnRows(sqlmock.NewRows([]string{"data", "expires_at", "created_at"}).AddRow(userJSON, expires, created))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/realtime", nil)
	c.Set("site_db", db)
	c.Set("site_session_manager", manager)
	c.Set("site_name", "tenant-1")
	c.Set("site_id", "tenant-1")
	c.Set("session_sid", "sid-1")
	c.Set("user", "alice")
	c.Set("user_roles", []string{"CachedReader"})

	valid, err := RevalidateRealtimeIdentity(c)
	if err != nil || !valid {
		t.Fatalf("RevalidateRealtimeIdentity() = (%t, %v), want (true, nil)", valid, err)
	}
	if got := c.GetStringSlice("user_roles"); len(got) != 1 || got[0] != "CurrentReader" {
		t.Fatalf("refreshed roles = %#v, want CurrentReader", got)
	}
	if _, ok := c.Get("policy_execution"); !ok {
		t.Fatal("refreshed execution identity was not installed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSiteGuardUsesExplicitlyOwnedSessionManager(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal("sqlmock.New:", err)
	}
	defer db.Close()
	manager := NewOwnedSessionManager(db)
	guard := NewSiteGuardWithSessionManager(manager)
	if guard.sessionMgr != manager {
		t.Fatal("SiteGuard did not retain the explicitly owned session manager")
	}
	if err := manager.Close(); err != nil {
		t.Fatal("close session manager:", err)
	}
}

func TestRevalidateRealtimeIdentityClosesRevokedSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	manager := NewOwnedSessionManager(db)
	defer manager.Close()
	mock.ExpectQuery(`SELECT data, expires_at, created_at FROM _kora_session WHERE site = \? AND sid = \?`).
		WithArgs("tenant-1", "sid-revoked").
		WillReturnError(sql.ErrNoRows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/realtime", nil)
	c.Set("site_db", db)
	c.Set("site_session_manager", manager)
	c.Set("site_name", "tenant-1")
	c.Set("session_sid", "sid-revoked")

	valid, err := RevalidateRealtimeIdentity(c)
	if err == nil || valid {
		t.Fatalf("RevalidateRealtimeIdentity() = (%t, %v), want revoked", valid, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRevalidateRealtimeIdentityRefreshesExtensionPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("tenant-1", HashExtensionAccessToken("extension-token")).
		WillReturnRows(sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("reader", `[{"doctype":"Invoice","read":true}]`))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/realtime", nil)
	c.Request.Header.Set("Authorization", "Bearer extension-token")
	c.Set("site_db", db)
	c.Set("site_name", "tenant-1")
	c.Set("auth_type", "extension")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "Invoice", Read: false}})

	valid, err := RevalidateRealtimeIdentity(c)
	if err != nil || !valid {
		t.Fatalf("RevalidateRealtimeIdentity() = (%t, %v), want (true, nil)", valid, err)
	}
	permissions, ok := c.Get("extension_permissions")
	if !ok || len(permissions.([]doctype.Permission)) != 1 || !permissions.([]doctype.Permission)[0].Read {
		t.Fatalf("refreshed extension permissions = %#v", permissions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProvisioningTokenIsRestrictedToManagedCredentialOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "provisioning-secret")
	guardOwner := NewSiteGuardWithSessionManager(nil)
	defer guardOwner.Close()
	guard := guardOwner.Middleware(true)
	for _, tc := range []struct {
		method     string
		path       string
		wantStatus int
	}{
		{method: http.MethodPost, path: "/api/internal/channel/managed-client/rotate", wantStatus: 200},
		{method: http.MethodPost, path: "/api/internal/channel/managed-client/revoke", wantStatus: 200},
		{method: http.MethodGet, path: "/api/internal/site/identity", wantStatus: 200},
		{method: http.MethodGet, path: "/api/internal/channel/tools", wantStatus: 401},
	} {
		router := gin.New()
		router.Use(guard)
		router.Handle(tc.method, tc.path, func(c *gin.Context) { c.Status(http.StatusOK) })
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Authorization", "Bearer provisioning-secret")
		router.ServeHTTP(recorder, request)
		if recorder.Code != tc.wantStatus {
			t.Errorf("path %s returned %d, want %d", tc.path, recorder.Code, tc.wantStatus)
		}
	}
}

func TestAuthenticateExtension_EmptyPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("test-bot", "[]")
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("test.local", HashExtensionAccessToken("token")).WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "demo")
	c.Set("site_id", "stable-demo-id")
	c.Set("site_name", "test.local")
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer token")

	guard := &SiteGuard{}
	ok := guard.authenticateExtension(c, "token")
	if !ok {
		t.Errorf("expected auth success with empty permissions")
	}
	perms, _ := c.Get("extension_permissions")
	p, ok := perms.([]doctype.Permission)
	if !ok {
		t.Fatalf("extension_permissions is not []doctype.Permission")
	}
	if len(p) != 0 {
		t.Errorf("expected 0 permissions for empty JSON, got %d", len(p))
	}
}

func TestAuthenticateExtension_NullPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("test-bot", nil)
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("test.local", HashExtensionAccessToken("token")).WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "test.local")
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer token")

	guard := &SiteGuard{}
	ok := guard.authenticateExtension(c, "token")
	if !ok {
		t.Errorf("expected auth success with NULL permissions")
	}
}

func TestAuthenticateExtension_InactiveExtension(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("test.local", HashExtensionAccessToken("token")).WillReturnError(sql.ErrNoRows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "test.local")
	c.Request = httptest.NewRequest("GET", "/", nil)

	guard := &SiteGuard{}
	ok := guard.authenticateExtension(c, "token")
	if ok {
		t.Errorf("expected auth failure for inactive/missing extension")
	}
}

func TestAuthenticateExtension_MalformedJSON(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("bad-bot", `{this is not json`)
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("test.local", HashExtensionAccessToken("token")).WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "test.local")
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer token")

	guard := &SiteGuard{}
	ok := guard.authenticateExtension(c, "token")
	// Should not panic. Should still authenticate but with empty permissions.
	if !ok {
		t.Errorf("expected auth success even with malformed JSON (secure default: empty perms)")
	}
	perms, _ := c.Get("extension_permissions")
	p, ok := perms.([]doctype.Permission)
	if !ok {
		t.Fatalf("extension_permissions is not []doctype.Permission")
	}
	if len(p) != 0 {
		t.Errorf("expected 0 permissions for malformed JSON, got %d", len(p))
	}
}

func TestAuthenticateExtension_RejectsCrossSiteTokenReuse(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token_hash = \\? AND is_active = 1").
		WithArgs("site-b", HashExtensionAccessToken("token")).WillReturnError(sql.ErrNoRows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "site-b")
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.Header.Set("Authorization", "Bearer token")

	guard := &SiteGuard{}
	if ok := guard.authenticateExtension(c, "token"); ok {
		t.Fatal("expected cross-site extension token reuse to be rejected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestAuthenticateChannelSession_LoadsPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	token := "channel-token"
	rows := sqlmock.NewRows([]string{
		"id", "site", "client_name", "conversation_key", "provider", "sender_address", "token_hash", "api_permissions",
		"trusted_until", "sensitive_until", "revoked_at", "revoked_reason", "created_at", "last_used_at",
	}).AddRow(
		"chs_1", "demo", "kora-cloud", "twilio:+1:+2", "twilio_whatsapp", "+254700000001", channelTokenHash(token),
		`[{"doctype":"Customer","read":true}]`, time.Now().Add(10*time.Minute), nil, nil, "", time.Now(), nil,
	)
	mock.ExpectQuery("SELECT id, site, client_name, conversation_key, provider, sender_address, token_hash, api_permissions").
		WithArgs(channelTokenHash(token)).WillReturnRows(rows)
	mock.ExpectExec("UPDATE _kora_channel_session SET last_used_at = .*WHERE id = .*").
		WithArgs(sqlmock.AnyArg(), "chs_1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
	c.Set("site_name", "demo")
	c.Set("site_id", "stable-demo-id")

	guard := &SiteGuard{}
	ok := guard.authenticateChannelSession(c, token)
	if !ok {
		t.Fatalf("expected channel session auth to succeed")
	}
	if c.GetString("auth_type") != "channel_session" {
		t.Fatalf("expected auth_type channel_session, got %q", c.GetString("auth_type"))
	}
	permsVal, exists := c.Get("channel_permissions")
	if !exists {
		t.Fatalf("expected channel_permissions in context")
	}
	perms := permsVal.([]doctype.Permission)
	if len(perms) != 1 || perms[0].Doctype != "Customer" || !perms[0].Read {
		t.Fatalf("unexpected channel permissions: %#v", perms)
	}
	verified, exists := c.Get("policy_execution")
	execution, ok := verified.(policy.Execution)
	if !exists || !ok || !execution.Valid() || execution.Tenant() != "stable-demo-id" || !execution.Allows("Customer", "read") || execution.Allows("Customer", "write") {
		t.Fatalf("channel session execution did not preserve its verified scoped grant: %#v", verified)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
