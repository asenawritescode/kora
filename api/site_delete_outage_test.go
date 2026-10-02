package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/doctype"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/gin-gonic/gin"
)

func TestHandleDeleteSiteDoesNotDropTenantWhenDirectoryUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	platformDB, platformMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	expectDeleteSiteInfo(platformMock, "active", 4)
	platformMock.ExpectBegin().WillReturnError(errors.New("platform registry unavailable"))

	tenantDB, tenantMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	defer tenantDB.Close()
	tenantMock.ExpectPing()

	loaded := &knet.LoadedSite{
		SiteID:         "site-delete-test",
		Name:           "delete-test.example.invalid",
		Status:         "active",
		ConfigRevision: 4,
		Config:         knet.SiteRouterConfig{Hostname: "delete-test.example.invalid"},
		DB:             tenantDB,
		Registry:       doctype.NewRegistry(),
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
	discarded := false
	handler := &ConsoleHandler{
		SiteRouter:     router,
		PlatformDB:     platformDB,
		PlatformDBType: "mysql",
		PlatformDBHost: "127.0.0.1",
		PlatformDBPort: 1,
		PlatformDBUser: "unused",
		DiscardRuntime: func(*knet.LoadedSite) { discarded = true },
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/console/sites/"+loaded.Name, strings.NewReader(`{"confirm":"delete-test.example.invalid"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "name", Value: loaded.Name}}
	handler.HandleDeleteSite(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete response status = %d, body=%s; want 503 before tenant deletion", recorder.Code, recorder.Body.String())
	}
	if router.SiteByID(loaded.SiteID) != loaded || loaded.Status != "active" || loaded.ConfigRevision != 4 {
		t.Fatalf("directory outage changed local route: loaded=%#v route=%p", loaded, router.SiteByID(loaded.SiteID))
	}
	if discarded {
		t.Fatal("runtime sidecars were discarded even though directory deletion intent was not persisted")
	}
	if err := tenantDB.Ping(); err != nil {
		t.Fatalf("tenant DB was closed despite failed directory intent: %v", err)
	}
	if err := platformMock.ExpectationsWereMet(); err != nil {
		t.Fatal("platform DB expectations:", err)
	}
	if err := tenantMock.ExpectationsWereMet(); err != nil {
		t.Fatal("tenant DB expectations:", err)
	}
}

func TestHandleUpdateSiteDoesNotApplyChangesWhenDirectoryUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	loaded := &knet.LoadedSite{
		SiteID: "site-update-test", Name: "update-test.example.invalid", Status: "active", ConfigRevision: 2,
		Config:   knet.SiteRouterConfig{Hostname: "update-test.example.invalid", Domains: []string{"update-test.example.invalid"}, FileStorage: "local"},
		Registry: doctype.NewRegistry(),
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
	resolvedStorage := false
	handler := &ConsoleHandler{
		SiteRouter: router,
		ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) {
			resolvedStorage = true
			return nil, nil
		},
	}
	request := httptest.NewRequest(http.MethodPut, "/api/console/sites/"+loaded.Name, strings.NewReader(`{"domains":["new.example.invalid"],"file_storage":"s3"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "name", Value: loaded.Name}}
	handler.HandleUpdateSite(ctx)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("update response status = %d, body=%s; want registry-unavailable response", recorder.Code, recorder.Body.String())
	}
	if resolvedStorage {
		t.Fatal("storage resolution ran before checking registry availability")
	}
	if got := router.SiteByID(loaded.SiteID); got != loaded || len(got.Config.Domains) != 1 || got.Config.Domains[0] != loaded.Name || got.Config.FileStorage != "local" {
		t.Fatalf("directory outage changed local config: %#v", got)
	}
}

func TestHandleDeleteSiteKeepsRouteUnavailableWhenTenantCleanupFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	platformDB, platformMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	expectDeleteSiteInfo(platformMock, "active", 4)
	platformMock.ExpectBegin()
	platformMock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry SET status = ?, config_revision = config_revision + 1, updated_at = CURRENT_TIMESTAMP WHERE site_id = ?`)).
		WithArgs("deleting", "site-delete-test").WillReturnResult(sqlmock.NewResult(0, 1))
	platformMock.ExpectQuery(regexp.QuoteMeta(`SELECT site, COALESCE(runtime_cell_id, ''), COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site_id = ?`)).
		WithArgs("site-delete-test").WillReturnRows(sqlmock.NewRows([]string{"site", "runtime_cell_id", "domains_json", "status", "config_revision"}).AddRow("delete-test.example.invalid", "", `[]`, "deleting", 5))
	platformMock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).WillReturnResult(sqlmock.NewResult(0, 1))
	platformMock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(17))
	platformMock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(sqlmock.AnyArg(), uint64(17), "site-delete-test", uint64(5), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	platformMock.ExpectCommit()

	tenantDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer tenantDB.Close()
	loaded := &knet.LoadedSite{
		SiteID: "site-delete-test", Name: "delete-test.example.invalid", Status: "active", ConfigRevision: 4,
		Config: knet.SiteRouterConfig{Hostname: "delete-test.example.invalid"}, DB: tenantDB, Registry: doctype.NewRegistry(),
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{loaded})
	discarded := false
	handler := &ConsoleHandler{
		SiteRouter: router, PlatformDB: platformDB, PlatformDBType: "mysql",
		PlatformDBHost: "127.0.0.1", PlatformDBPort: 1, PlatformDBUser: "unused",
		DiscardRuntime: func(runtime *knet.LoadedSite) { discarded = runtime == loaded },
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/console/sites/"+loaded.Name, strings.NewReader(`{"confirm":"delete-test.example.invalid"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "name", Value: loaded.Name}}
	handler.HandleDeleteSite(ctx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("delete response status = %d, body=%s; want pending cleanup error", recorder.Code, recorder.Body.String())
	}
	if router.SiteByID(loaded.SiteID) != loaded || loaded.Status != "deleting" || loaded.ConfigRevision != 5 {
		t.Fatalf("failed cleanup did not preserve unavailable retry state: loaded=%#v", loaded)
	}
	if !discarded {
		t.Fatal("sidecars were left running after durable deletion intent")
	}
	traffic := gin.New()
	traffic.Use(router.Middleware())
	traffic.GET("/tenant", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	trafficRequest := httptest.NewRequest(http.MethodGet, "/tenant", nil)
	trafficRequest.Host = loaded.Name
	trafficResponse := httptest.NewRecorder()
	traffic.ServeHTTP(trafficResponse, trafficRequest)
	if trafficResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("tenant request during pending deletion = %d, want 503", trafficResponse.Code)
	}
	if err := platformMock.ExpectationsWereMet(); err != nil {
		t.Fatal("platform DB expectations:", err)
	}
}

func expectDeleteSiteInfo(mock sqlmock.Sqlmock, status string, revision uint64) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`)).
		WithArgs("site-delete-test").WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision"}).
		AddRow("site-delete-test", "", "delete-test.example.invalid", "mysql", "127.0.0.1", 1, "delete_test_db", "unused", "", 0, `[]`, "local", "", status, revision))
}
