package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/storage"
)

// setupTestHandler creates a Handler with a mocked DB, a registry containing
// "TestDoc" (a simple Data-field doctype) and "NoPermDoc" (for permission-denied tests).
func setupTestHandler(t *testing.T) (*Handler, *doctype.Registry, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}

	reg := doctype.NewRegistry()

	dt := &doctype.DocType{
		Name:         "TestDoc",
		SortField:    "modified",
		SortOrder:    "DESC",
		IsChildTable: false,
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data", Label: "Title"},
		},
	}
	reg.Register(dt)

	// Register a doctype with no permission entries for permission-denied tests.
	reg.Register(&doctype.DocType{
		Name:      "NoPermDoc",
		SortField: "modified",
		SortOrder: "DESC",
		Fields: []doctype.Field{
			{Fieldname: "data", Fieldtype: "Data"},
		},
	})

	dialect := db.Resolve("mysql")
	txm := &orm.TxManager{DB: mockDB, Registry: reg, Dialect: dialect}

	handler := NewHandler(reg, txm)
	return handler, reg, mock, mockDB
}

// injectContext sets standard test context values.
func injectContext(c *gin.Context) {
	c.Set("site_name", "test.local")
	c.Set("user", "admin")
	c.Set("user_role", "Administrator")
	c.Set("user_roles", []string{"Administrator"})
}

// injectDB sets the database and registry on the Gin context (used by siteTx).
func injectDB(c *gin.Context, sqlDB *sql.DB, reg *doctype.Registry) {
	c.Set("site_db", sqlDB)
	c.Set("site_registry", reg)
}

func TestSiteDialectUsesPerRuntimeDatabaseType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, &orm.TxManager{Dialect: db.Resolve("mysql")})
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("site_db_type", "postgres")
	if got := handler.siteDialect(ctx).DriverName(); got != "postgres" {
		t.Fatalf("request site dialect = %q, want postgres", got)
	}
	ctxWithoutSite, _ := gin.CreateTestContext(httptest.NewRecorder())
	if got := handler.siteDialect(ctxWithoutSite).DriverName(); got != "mysql" {
		t.Fatalf("fallback site dialect = %q, want mysql", got)
	}
}

func TestTenantUserAndSettingsQueriesRebindForPostgres(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	gin.SetMode(gin.TestMode)

	userRecorder := httptest.NewRecorder()
	userContext, _ := gin.CreateTestContext(userRecorder)
	userContext.Request = httptest.NewRequest(http.MethodGet, "/api/system/users", nil)
	injectContext(userContext)
	injectDB(userContext, database, reg)
	userContext.Set("site_db_type", "postgres")
	mock.ExpectQuery(`SELECT name, email, full_name, enabled, roles, creation, modified FROM _kora_user WHERE site = \$1 ORDER BY name`).
		WithArgs("test.local").
		WillReturnRows(sqlmock.NewRows([]string{"name", "email", "full_name", "enabled", "roles", "creation", "modified"}))
	handler.HandleUserList(userContext)
	if userRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL user list returned HTTP %d: %s", userRecorder.Code, userRecorder.Body.String())
	}

	settingsRecorder := httptest.NewRecorder()
	settingsContext, _ := gin.CreateTestContext(settingsRecorder)
	settingsContext.Request = httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"currency":"USD"}`))
	settingsContext.Request.Header.Set("Content-Type", "application/json")
	injectContext(settingsContext)
	injectDB(settingsContext, database, reg)
	settingsContext.Set("site_db_type", "postgres")
	mock.ExpectExec(`INSERT INTO _kora_site_setting .*VALUES \(\$1, \$2, \$3\) ON CONFLICT`).
		WithArgs("test.local", "currency", "USD").
		WillReturnResult(sqlmock.NewResult(0, 1))
	handler.HandleSiteSettingsUpdate(settingsContext)
	if settingsRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL site settings update returned HTTP %d: %s", settingsRecorder.Code, settingsRecorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("PostgreSQL query expectations: %v", err)
	}
}

func TestConfigVersionEndpointsUsePostgresDialect(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/system/config/versions/draft-1/discard", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "draft-1"}}
	injectContext(ctx)
	injectDB(ctx, database, reg)
	ctx.Set("site_db_type", "postgres")
	mock.ExpectQuery(`SELECT status FROM _kora_config_version WHERE id = \$1`).
		WithArgs("draft-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("Draft"))
	mock.ExpectExec(`UPDATE _kora_config_version SET status = 'Superseded' WHERE id = \$1`).
		WithArgs("draft-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	handler.HandleConfigVersionDiscard(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL config-version discard returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/api/system/config/versions", nil)
	injectContext(listContext)
	injectDB(listContext, database, reg)
	listContext.Set("site_db_type", "postgres")
	mock.ExpectQuery(`COALESCE\(status, CASE WHEN is_active = 1 THEN 'Active' ELSE 'Superseded' END\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "site", "version", "created_at", "created_by", "label", "status"}))
	handler.HandleConfigVersions(listContext)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL config-version list returned HTTP %d: %s", listRecorder.Code, listRecorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("PostgreSQL config-version SQL expectations: %v", err)
	}
}

func TestRoleDeleteUsesPostgresMembershipExpressionAndBindings(t *testing.T) {
	handler, reg, mock, database := setupTestHandler(t)
	defer database.Close()
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/system/roles/Store%20Manager", nil)
	ctx.Params = gin.Params{{Key: "name", Value: "Store Manager"}}
	injectContext(ctx)
	injectDB(ctx, database, reg)
	ctx.Set("site_db_type", "postgres")
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM _kora_user WHERE POSITION\(',' \|\| \$1 \|\| ',' IN ',' \|\| REPLACE\(roles, ', ', ','\) \|\| ','\) > 0`).
		WithArgs("Store Manager").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectExec(`DELETE FROM _kora_role WHERE name = \$1`).
		WithArgs("Store Manager").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM _kora_permission WHERE role = \$1`).
		WithArgs("Store Manager").
		WillReturnResult(sqlmock.NewResult(0, 1))
	handler.HandleSystemRoleDelete(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PostgreSQL role delete returned HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("PostgreSQL role-delete SQL expectations: %v", err)
	}
}

func expectGeneratedName(mock sqlmock.Sqlmock, maxSuffix, allocated int64) {
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(maxSuffix))
	mock.ExpectExec("INSERT INTO _kora_naming_series").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT LAST_INSERT_ID\\(\\)").
		WillReturnRows(sqlmock.NewRows([]string{"last_insert_id"}).AddRow(allocated))
}

type fakeScriptRunner struct{}

func (fakeScriptRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	return &script.ExecuteResult{}, nil
}

func (fakeScriptRunner) Validate(string) error { return nil }

func (fakeScriptRunner) Close() error { return nil }

type fakePublicStorage struct{}

func (fakePublicStorage) Put(context.Context, string, io.Reader, int64, storage.FileMeta) (*storage.FileMeta, error) {
	return nil, nil
}

func (fakePublicStorage) EnsureBucket(context.Context) error { return nil }

func (fakePublicStorage) Head(context.Context, string) (*storage.FileMeta, error) {
	return nil, storage.ErrNotFound
}

func (fakePublicStorage) Open(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, storage.ErrNotFound
}

func (fakePublicStorage) Delete(context.Context, string) error { return nil }

func (fakePublicStorage) URL(_ context.Context, key string) (string, error) {
	return "https://cdn.example.com/" + key, nil
}

// ---------------------------------------------------------------------------
// HandleList
// ---------------------------------------------------------------------------

func TestHandleList_Empty(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTestDoc` WHERE 1=1").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(0))
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE 1=1 ORDER BY `modified` DESC LIMIT \\? OFFSET \\?").
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleList(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if resp.Meta == nil || resp.Meta.Total != 0 {
		t.Errorf("meta.total = %v, want 0", resp.Meta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandlePublicList_RequiresExplicitPublicAccess(t *testing.T) {
	handler, _, _, _ := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/public/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}

	handler.HandlePublicList(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

func TestHandlePublicList_AllowlistedFieldsAndServerFilters(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	reg.Register(&doctype.DocType{
		Name:      "PublicPost",
		SortField: "modified",
		SortOrder: "DESC",
		Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"},
			{Fieldname: "status", Fieldtype: "Data"},
			{Fieldname: "hero_image", Fieldtype: "Attach Image"},
			{Fieldname: "internal_notes", Fieldtype: "Text"},
		},
		PublicAccess: &doctype.PublicAccess{
			Enabled: true,
			List:    true,
			Read:    true,
			Fields:  []string{"title", "hero_image"},
			Filters: []doctype.PublicFilter{{Field: "status", Op: "equals", Value: "published"}},
		},
	})
	handler.Storage = fakePublicStorage{}

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabPublicPost` WHERE status = \\?").
		WithArgs("published").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .+ FROM `tabPublicPost` WHERE status = \\? ORDER BY `modified` DESC LIMIT \\? OFFSET \\?").
		WithArgs("published", 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"title", "status", "hero_image", "internal_notes", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Published", "published", "2026/09/hero.png", "secret", "PUB-0001", "owner", nil, nil, nil, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/public/resource/PublicPost", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "PublicPost"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandlePublicList(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	data := resp.Data.([]any)[0].(map[string]any)
	if data["title"] != "Published" {
		t.Fatalf("title = %v, want Published", data["title"])
	}
	if data["hero_image_url"] != "https://example.com/s/test.local/api/public/files/sites/test.local/files/2026/09/hero.png" {
		t.Fatalf("hero_image_url = %v, want resolved public url", data["hero_image_url"])
	}
	if _, ok := data["internal_notes"]; ok {
		t.Fatalf("internal_notes leaked in public response: %#v", data)
	}
}

func TestFileKeyForSiteReference_NormalizesLegacyAndRejectsOtherSite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		ref  string
		want string
		ok   bool
	}{
		{name: "legacy relative", ref: "2026/09/hero.png", want: "sites/test.local/files/2026/09/hero.png", ok: true},
		{name: "scoped current", ref: "sites/test.local/files/2026/09/hero.png", want: "sites/test.local/files/2026/09/hero.png", ok: true},
		{name: "other site", ref: "sites/other/files/hero.png", ok: false},
		{name: "traversal", ref: "../../secret.txt", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Set("site_name", "test.local")
			got, err := fileKeyForSiteReference(c, tc.ref)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, want success=%v", err, tc.ok)
			}
			if tc.ok && got != tc.want {
				t.Fatalf("key = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandleList_WithFilters(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTestDoc` WHERE 1=1").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(2))
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE 1=1 ORDER BY `modified` DESC LIMIT \\? OFFSET \\?").
		WithArgs(10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}).
			AddRow("TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0, "First").
			AddRow("TEST-0002", "admin", "2024-01-02 00:00:00", "2024-01-02 00:00:00", "admin", 0, "Second"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc?limit=10", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleList(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if resp.Meta == nil || resp.Meta.Total != 2 {
		t.Errorf("meta.total = %v, want 2", resp.Meta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandleList_DoctypeNotFound(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/UnknownDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "UnknownDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleList(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HandleGet
// ---------------------------------------------------------------------------

func TestHandleGet_Found(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "revision", "title"}).
			AddRow("TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0, 1, "First Doc"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc/TEST-0001", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleGet(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if resp.Meta == nil || resp.Meta.DocType != "TestDoc" {
		t.Errorf("meta.doctype = %v, want TestDoc", resp.Meta)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("MISSING").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc/MISSING", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "MISSING"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleGet(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandleGet_DoctypeNotFound(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/Unknown/name", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "Unknown"},
		{Key: "name", Value: "name"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleGet(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// HandleCreate
// ---------------------------------------------------------------------------

func TestHandleCreate_DoctypeNotFound(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/resource/Unknown", strings.NewReader(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "doctype", Value: "Unknown"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleCreate(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestPermissionTargetForTool_UsesExactRegisteredDoctypeName(t *testing.T) {
	reg := doctype.NewRegistry()
	reg.Register(&doctype.DocType{Name: "API Key"})
	reg.Register(&doctype.DocType{Name: "E-mail Template"})

	docType, operation, ok := permissionTargetForTool(reg, "api_key_create")
	if !ok {
		t.Fatal("expected API Key tool name to resolve")
	}
	if docType != "API Key" {
		t.Fatalf("doctype = %q, want %q", docType, "API Key")
	}
	if operation != "create" {
		t.Fatalf("operation = %q, want %q", operation, "create")
	}

	docType, operation, ok = permissionTargetForTool(reg, "e_mail_template_list")
	if !ok {
		t.Fatal("expected E-mail Template tool name to resolve")
	}
	if docType != "E-mail Template" {
		t.Fatalf("doctype = %q, want %q", docType, "E-mail Template")
	}
	if operation != "read" {
		t.Fatalf("operation = %q, want %q", operation, "read")
	}
}

// ---------------------------------------------------------------------------
// Permission Denied
// ---------------------------------------------------------------------------

func TestHandleList_PermissionDenied(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/NoPermDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "NoPermDoc"}}
	injectDB(c, sqlDB, reg)
	c.Set("user", "admin")
	// Set a role that has no permissions configured, triggering denial.
	c.Set("user_role", "Guest")
	c.Set("user_roles", []string{"Guest"})

	handler.HandleList(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Extension Permission Enforcement
// ---------------------------------------------------------------------------

func TestExtensionPermission_ReadGranted(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTestDoc` WHERE 1=1").
		WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(1))
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE 1=1 ORDER BY `modified` DESC LIMIT \\? OFFSET \\?").
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}).
			AddRow("TEST-0001", "bot", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "bot", 0, "Ext Doc"))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	c.Set("auth_type", "extension")
	c.Set("extension_name", "test-bot")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "TestDoc", Read: true}})

	handler.HandleList(c)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestExtensionPermission_DeleteDenied(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/api/resource/TestDoc/TEST-0001", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)
	c.Set("auth_type", "extension")
	c.Set("extension_name", "test-bot")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "TestDoc", Read: true}})

	handler.HandleDelete(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}
}

func TestExtensionPermission_UnconfiguredDoctype(t *testing.T) {
	handler, reg, _, sqlDB := setupTestHandler(t)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	c.Set("auth_type", "extension")
	c.Set("extension_name", "test-bot")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "OtherDoc", Read: true}})

	handler.HandleList(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}
}
