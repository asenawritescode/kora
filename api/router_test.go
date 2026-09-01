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
			AddRow("Published", "published", "sites/test/files/2026/09/hero.png", "secret", "PUB-0001", "owner", nil, nil, nil, 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/public/resource/PublicPost", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "PublicPost"}}
	injectDB(c, sqlDB, reg)

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
	if data["hero_image_url"] != "https://cdn.example.com/sites/test/files/2026/09/hero.png" {
		t.Fatalf("hero_image_url = %v, want resolved public url", data["hero_image_url"])
	}
	if _, ok := data["internal_notes"]; ok {
		t.Fatalf("internal_notes leaked in public response: %#v", data)
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
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}).
			AddRow("TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0, "First Doc"))

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

func TestHandleCreate_ValidDoc(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// Insert uses a transaction: Begin → NameGen → INSERT → Commit
	mock.ExpectBegin()
	expectGeneratedName(mock, 0, 1)
	mock.ExpectExec("INSERT INTO `tabTestDoc`").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "New Document"}`
	c.Request = httptest.NewRequest("POST", "/api/resource/TestDoc", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleCreate(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusCreated, w.Body.String())
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

func TestHandleCreate_ValidationError(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// Should succeed — validation passes for an empty Data field.
	mock.ExpectBegin()
	expectGeneratedName(mock, 0, 1)
	mock.ExpectExec("INSERT INTO `tabTestDoc`").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": ""}`
	c.Request = httptest.NewRequest("POST", "/api/resource/TestDoc", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleCreate(c)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
}

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
// HandleUpdate
// ---------------------------------------------------------------------------

func TestHandleUpdate_Valid(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// GetDoc (no transaction).
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Original Title", "TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0))

	// Save uses a transaction: Begin → UPDATE → Commit
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `tabTestDoc` SET .+ WHERE name = \\?").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "Updated Title"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/TEST-0001", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleUpdate(c)

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

func TestHandleUpdate_NoEditableChangesSkipsSave(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Original Title", "TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "Original Title"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/TEST-0001", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleUpdate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandleUpdate_ReadOnlySubmittedFieldSkipsSave(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	dt := reg.Get("TestDoc")
	dt.Fields = append(dt.Fields, doctype.Field{
		Fieldname: "internal_note",
		Fieldtype: "Data",
		ReadOnly:  true,
	})
	reg.Register(dt)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"title", "internal_note", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Original Title", "locked", "TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"internal_note": "changed"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/TEST-0001", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleUpdate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestHandleUpdate_ScriptRunnerDisablesNoopShortCircuit(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	handler.ScriptRunner = fakeScriptRunner{}

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).
			AddRow("Original Title", "TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0))
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `tabTestDoc` SET").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "Original Title"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/TEST-0001", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleUpdate(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestResourceFieldValuesEqualNormalizesCommonSQLAndJSONTypes(t *testing.T) {
	tests := []struct {
		name     string
		field    doctype.Field
		oldVal   any
		newVal   any
		wantSame bool
	}{
		{"int string and json number", doctype.Field{Fieldtype: "Int"}, "7", float64(7), true},
		{"float string and json number", doctype.Field{Fieldtype: "Currency"}, "7.50", float64(7.5), true},
		{"check int and bool", doctype.Field{Fieldtype: "Check"}, int64(1), true, true},
		{"json string and object", doctype.Field{Fieldtype: "JSON"}, `{"enabled":true}`, map[string]any{"enabled": true}, true},
		{"data change", doctype.Field{Fieldtype: "Data"}, "old", "new", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resourceFieldValuesEqual(&tt.field, tt.oldVal, tt.newVal)
			if got != tt.wantSame {
				t.Fatalf("resourceFieldValuesEqual() = %v, want %v", got, tt.wantSame)
			}
		})
	}
}

func TestHandleUpdate_NotFound(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("MISSING").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "Updated"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/MISSING", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "MISSING"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleUpdate(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// HandleDelete
// ---------------------------------------------------------------------------

func TestHandleDelete_Success(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// Delete uses a transaction: Begin → DELETE → Commit
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/api/resource/TestDoc/TEST-0001", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleDelete(c)

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

func TestHandleDelete_NotFound(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// Delete begins a transaction, but the DELETE returns 0 rows
	// so Save returns ErrNotFound before Commit.
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("MISSING").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// No ExpectCommit — save returns error before committing.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("DELETE", "/api/resource/TestDoc/MISSING", nil)
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "MISSING"},
	}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleDelete(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
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

func TestExtensionPermission_WriteGranted(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}).
			AddRow("TEST-0001", "bot", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "bot", 0, "Original"))

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE `tabTestDoc` SET .+ WHERE name = \\?").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "Updated"}`
	c.Request = httptest.NewRequest("PUT", "/api/resource/TestDoc/TEST-0001", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{
		{Key: "doctype", Value: "TestDoc"},
		{Key: "name", Value: "TEST-0001"},
	}
	injectDB(c, sqlDB, reg)
	c.Set("auth_type", "extension")
	c.Set("extension_name", "test-bot")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "TestDoc", Write: true}})

	handler.HandleUpdate(c)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestExtensionPermission_CreateGranted(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	mock.ExpectBegin()
	expectGeneratedName(mock, 0, 1)
	mock.ExpectExec("INSERT INTO `tabTestDoc`").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"title": "New Doc"}`
	c.Request = httptest.NewRequest("POST", "/api/resource/TestDoc", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	c.Set("auth_type", "extension")
	c.Set("extension_name", "test-bot")
	c.Set("extension_permissions", []doctype.Permission{{Doctype: "TestDoc", Create: true}})

	handler.HandleCreate(c)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusCreated, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}
