package auth

import (
	"database/sql"
	"net/http/httptest"
	"testing"

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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension").
		WithArgs("valid-token").WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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

func TestAuthenticateExtension_EmptyPermissions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"name", "api_permissions"}).AddRow("test-bot", "[]")
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension").
		WithArgs("token").WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension").
		WithArgs("token").WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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

	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension").
		WithArgs("token").WillReturnError(sql.ErrNoRows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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
	mock.ExpectQuery("SELECT name, api_permissions FROM _kora_extension").
		WithArgs("token").WillReturnRows(rows)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("site_db", db)
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
