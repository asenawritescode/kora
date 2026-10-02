package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("test.local", "valid-token").WillReturnRows(rows)

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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestProvisioningTokenIsRestrictedToManagedCredentialRotation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "provisioning-secret")
	guard := (&SiteGuard{}).Middleware(true)
	for _, tc := range []struct {
		method     string
		path       string
		wantStatus int
	}{
		{method: http.MethodPost, path: "/api/internal/channel/managed-client/rotate", wantStatus: 200},
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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("test.local", "token").WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("test.local", "token").WillReturnRows(rows)

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

	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("test.local", "token").WillReturnError(sql.ErrNoRows)

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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("test.local", "token").WillReturnRows(rows)

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

	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension WHERE site = \\? AND access_token = \\? AND is_active = 1").
		WithArgs("site-b", "token").WillReturnError(sql.ErrNoRows)

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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
