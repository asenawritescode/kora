package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
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
		Name:       "NoPermDoc",
		SortField:  "modified",
		SortOrder:  "DESC",
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
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
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
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
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

// ---------------------------------------------------------------------------
// HandleUpdate
// ---------------------------------------------------------------------------

func TestHandleUpdate_Valid(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)

	// GetDoc (no transaction).
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE name = \\?").
		WithArgs("TEST-0001").
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}).
			AddRow("TEST-0001", "admin", "2024-01-01 00:00:00", "2024-01-01 00:00:00", "admin", 0, "Original Title"))

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
	mock.ExpectQuery("SELECT COALESCE\\(MAX").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
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
