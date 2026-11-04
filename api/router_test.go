package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/contract"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/policy"
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

func TestFallbackStorageConcurrentInitialization(t *testing.T) {
	const callers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			backend, err := (&Handler{}).siteStorage(nil)
			if err == nil && backend == nil {
				err = errors.New("fallback storage is nil")
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("initialize fallback storage: %v", err)
		}
	}
}

func TestSiteStorageResolvesGenerationSnapshotWithoutCompatibilityMaps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := fakePublicStorage{}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("site_name", "tenant.example")
	ctx.Set("site_runtime_services", SiteRuntimeService{Storage: backend})

	got, err := (&Handler{}).siteStorage(ctx)
	if err != nil {
		t.Fatalf("siteStorage: %v", err)
	}
	if got != backend {
		t.Fatal("site storage did not resolve from the routed tenant snapshot")
	}
}

// injectContext sets standard test context values.
func injectContext(c *gin.Context) {
	c.Set("site_name", "test.local")
	c.Set("site_id", "test-tenant")
	c.Set("user", "admin")
	c.Set("user_role", "Administrator")
	c.Set("user_roles", []string{"Administrator"})
	c.Set("policy_execution", routerTestExecution([]string{"Administrator"}, nil))
}

func routerTestExecution(roles []string, grants []policy.Grant) policy.Execution {
	execution, err := policy.NewVerifiedExecutionWithGrants("test-tenant", contract.ActorContext{
		PrincipalID: "admin", PrincipalType: contract.PrincipalHuman, SubjectUserID: "admin",
		Site: "test.local", Roles: roles, AuthenticatedAt: time.Now().UTC(),
	}, grants)
	if err != nil {
		panic(err)
	}
	return execution
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

func TestSiteTxCarriesDurableHookIdentityFromVerifiedRuntimeSnapshot(t *testing.T) {
	handler, reg, _, database := setupTestHandler(t)
	defer database.Close()

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ai/chat", nil)
	injectContext(ctx)
	injectDB(ctx, database, reg)
	ctx.Set("site_name", "test.local")
	ctx.Set("site_config_revision", uint64(73))
	store := &script.Store{DB: database, Dialect: db.Resolve("mysql")}
	ctx.Set("site_runtime_services", SiteRuntimeService{
		ScriptStore: store, AsyncHookInvocations: store,
	})

	tx := handler.siteTx(ctx)
	if tx.AsyncHookTenantID != "test-tenant" {
		t.Fatalf("async hook tenant ID = %q, want verified tenant ID", tx.AsyncHookTenantID)
	}
	if tx.AsyncHookConfigRevision != 73 {
		t.Fatalf("async hook config revision = %d, want pinned runtime revision 73", tx.AsyncHookConfigRevision)
	}
	if tx.AsyncHookInvocations != store || tx.ScriptStore != store {
		t.Fatal("site transaction did not retain the runtime's durable hook store")
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
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM _kora_role WHERE name = \$1`).
		WithArgs("Store Manager").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM _kora_permission WHERE role = \$1`).
		WithArgs("Store Manager").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_auth_revision SET policy_revision = policy_revision \+ 1 WHERE id = 1`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE _kora_config_revision SET revision = revision \+ 1 WHERE id = 1 AND revision < 9223372036854775807`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
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

func TestHandleListRejectsSearchOver128Characters(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc?search="+strings.Repeat("a", 129), nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleList(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected database query: %v", err)
	}
}

func TestHandleListPushesLiteralPrefixSearchIntoDatabaseBeforePaging(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	defer sqlDB.Close()
	dt := reg.Get("TestDoc")
	dt.SearchFields = "title"
	dt.Fields[0].SearchIndex = true

	searchWhere := "1=1 AND (`title` LIKE ? ESCAPE '!' OR `name` = ?)"
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `tabTestDoc` WHERE "+searchWhere)).
		WithArgs("Acme!%!_%", "Acme%_").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `title`, `name`, `owner`, `creation`, `modified`, `modified_by`, `doc_status` FROM `tabTestDoc` WHERE "+searchWhere+" ORDER BY `modified` DESC LIMIT ? OFFSET ?")).
		WithArgs("Acme!%!_%", "Acme%_", 10, 20).
		WillReturnRows(sqlmock.NewRows([]string{"title", "name", "owner", "creation", "modified", "modified_by", "doc_status"}).AddRow("Acme%_ Systems", "TESTDOC-001", "admin", time.Now(), time.Now(), "admin", 0))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc?search=Acme%25_&limit=10&offset=20", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	injectContext(c)

	handler.HandleList(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("database search was not applied before paging: %v", err)
	}
}

func TestHandleListRequiresVerifiedExecution(t *testing.T) {
	handler, reg, mock, sqlDB := setupTestHandler(t)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, reg)
	c.Set("site_name", "test.local")
	c.Set("user", "admin")
	c.Set("user_roles", []string{"Administrator"})
	handler.HandleList(c)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusForbidden, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("permission rejection reached storage: %v", err)
	}
}

func TestGenericHTTPReadsDenyModuleOwnedRecordsBeforeStorage(t *testing.T) {
	handler, registry, mock, database := setupTestHandler(t)
	defer database.Close()
	docType := registry.Get("TestDoc")
	docType.ModuleOwnerID = "finance.ar"
	docType.ModuleAccess = contract.ModulePrivate
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Administrator", Read: true})

	for _, method := range []string{"list", "get", "history"} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		target := "/api/resource/TestDoc"
		switch method {
		case "get":
			target += "/INV-1"
		case "history":
			target += "/INV-1/history"
		}
		ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
		ctx.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
		if method != "list" {
			ctx.Params = append(ctx.Params, gin.Param{Key: "name", Value: "INV-1"})
		}
		injectDB(ctx, database, registry)
		injectContext(ctx)
		switch method {
		case "list":
			handler.HandleList(ctx)
		case "get":
			handler.HandleGet(ctx)
		case "history":
			handler.HandleRecordHistory(ctx)
		}
		if recorder.Code != http.StatusForbidden {
			t.Errorf("generic %s of module-owned record status=%d, want forbidden: %s", method, recorder.Code, recorder.Body.String())
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("denied generic module reads reached storage: %v", err)
	}
}

func TestHandleListUsesRoutedTenantPermissions(t *testing.T) {
	handler, _, mock, sqlDB := setupTestHandler(t)
	siteRegistry := doctype.NewRegistry()
	siteRegistry.Register(&doctype.DocType{Name: "TestDoc", SortField: "modified", SortOrder: "DESC", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}})
	siteRegistry.Permissions.SetPermission(&doctype.Permission{Doctype: "TestDoc", Role: "Sales Rep", Read: true})
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM `tabTestDoc` WHERE 1=1").WillReturnRows(sqlmock.NewRows([]string{"count(*)"}).AddRow(0))
	mock.ExpectQuery("SELECT .+ FROM `tabTestDoc` WHERE 1=1 ORDER BY `modified` DESC LIMIT \\? OFFSET \\?").
		WithArgs(50, 0).
		WillReturnRows(sqlmock.NewRows([]string{"name", "owner", "creation", "modified", "modified_by", "doc_status", "title"}))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/resource/TestDoc", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "TestDoc"}}
	injectDB(c, sqlDB, siteRegistry)
	c.Set("policy_execution", routerTestExecutionForUser("test-tenant", "test.local", "sales-rep", []string{"Sales Rep"}))
	c.Set("user", "sales-rep")
	handler.HandleList(c)
	if w.Code != http.StatusOK {
		t.Fatalf("routed tenant read status=%d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("routed tenant read did not query records: %v", err)
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

func TestHandlePublicListRejectsModuleOwnedResourceEvenWhenExplicitlyPublic(t *testing.T) {
	handler, registry, _, _ := setupTestHandler(t)
	registry.Register(&doctype.DocType{Name: "ModuleRecord", ModuleOwnerID: "billing.ar", ModuleAccess: contract.ModulePublished, PublicAccess: &doctype.PublicAccess{Enabled: true, List: true, Read: true, Fields: []string{"title"}}})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/public/resource/ModuleRecord", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "ModuleRecord"}}
	handler.HandlePublicList(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("public route exposed module resource: status=%d body=%s", w.Code, w.Body.String())
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

func TestHandleGetAllowsOnlyCurrentAssignee(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Task", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "assigned_to", Fieldtype: "User"}}})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Task", Role: "Assignee", Read: true, IfAssigned: []string{"assigned_to"}})
	handler := NewHandler(registry, &orm.TxManager{DB: database, Registry: registry, Dialect: db.Resolve("mysql")})
	mock.ExpectQuery("SELECT title, assigned_to, name, owner, creation, modified, modified_by, doc_status, revision FROM .* WHERE name = \\? AND \\(`assigned_to` = \\?\\)").
		WithArgs("TASK-1", "alice").WillReturnRows(sqlmock.NewRows([]string{"title", "assigned_to", "name", "owner", "creation", "modified", "modified_by", "doc_status", "revision"}).AddRow("Call", "alice", "TASK-1", "creator", nil, nil, nil, 0, 1))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/resource/Task/TASK-1", nil)
	c.Params = gin.Params{{Key: "doctype", Value: "Task"}, {Key: "name", Value: "TASK-1"}}
	c.Set("site_name", "tenant.test")
	c.Set("site_id", "tenant-id")
	c.Set("user", "alice")
	c.Set("user_roles", []string{"Assignee"})
	c.Set("policy_execution", routerTestExecutionForUser("tenant-id", "tenant.test", "alice", []string{"Assignee"}))
	handler.HandleGet(c)
	if w.Code != http.StatusOK {
		t.Fatalf("assigned get status=%d body=%s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPermResolvesResourceAliasForScopedRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Lead"})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Lead", Role: "Sales Rep", Create: true, Read: true, IfOwner: true})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/resource/lead", nil)
	c.Set("site_name", "tenant.test")
	c.Set("site_id", "tenant-id")
	c.Set("user", "rep-1")
	c.Set("user_roles", []string{"Sales Rep"})
	c.Set("policy_execution", routerTestExecutionForUser("tenant-id", "tenant.test", "rep-1", []string{"Sales Rep"}))
	if _, forbidden := checkPerm(c, registry, "lead", "create"); forbidden {
		t.Fatalf("resource alias denied despite canonical Sales Rep create permission: %s", w.Body.String())
	}
}

func TestCheckPermRequiresExactEntityGrantRegardlessOfTenantRole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", BusinessScopePolicy: contract.ScopeEntityOwned})
	registry.Permissions.SetPermission(&doctype.Permission{Doctype: "Invoice", Role: "Administrator", Read: true, Create: true, Write: true})
	actor := contract.ActorContext{PrincipalID: "admin", SubjectUserID: "admin", PrincipalType: contract.PrincipalHuman, Site: "tenant.test", Roles: []string{"Administrator"}, AuthenticatedAt: time.Now().UTC()}
	scopeA := contract.BusinessScope{TenantID: "tenant-id", Kind: contract.ScopeKindLegalEntity, LegalEntityID: "entity-a"}
	for _, tc := range []struct {
		entity string
		grants []policy.Grant
		want   bool
	}{{entity: "entity-a", want: false}, {entity: "entity-a", grants: []policy.Grant{{Resource: "Invoice", Operation: "read", Scope: scopeA}}, want: true}, {entity: "entity-b", grants: []policy.Grant{{Resource: "Invoice", Operation: "read", Scope: scopeA}}, want: false}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/resource/Invoice", nil)
		c.Request.Header.Set("X-Kora-Legal-Entity-ID", tc.entity)
		execution, err := policy.NewVerifiedExecutionWithGrants("tenant-id", actor, tc.grants)
		if err != nil {
			t.Fatal(err)
		}
		c.Set("site_name", "tenant.test")
		c.Set("policy_execution", execution)
		_, forbidden := checkPerm(c, registry, "Invoice", "read")
		if gotAllowed := !forbidden; gotAllowed != tc.want {
			t.Fatalf("entity %q allowed=%t, want %t; response=%s", tc.entity, gotAllowed, tc.want, w.Body.String())
		}
	}
}

func routerTestExecutionForUser(tenant, site, user string, roles []string) policy.Execution {
	execution, err := policy.NewVerifiedExecution(contract.TenantID(tenant), contract.ActorContext{PrincipalID: user, SubjectUserID: user, PrincipalType: contract.PrincipalHuman, Site: site, Roles: roles, AuthenticatedAt: time.Now().UTC()})
	if err != nil {
		panic(err)
	}
	return execution
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
	c.Set("policy_execution", routerTestExecution([]string{"Guest"}, nil))

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
	permissions := []doctype.Permission{{Doctype: "TestDoc", Read: true}}
	c.Set("extension_permissions", permissions)
	c.Set("policy_execution", routerTestExecution(nil, []policy.Grant{{Resource: "TestDoc", Operation: "read"}}))

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
	c.Set("policy_execution", routerTestExecution(nil, []policy.Grant{{Resource: "TestDoc", Operation: "read"}}))

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
