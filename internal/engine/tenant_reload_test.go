package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
	"github.com/gin-gonic/gin"
)

func TestTenantReloadRouteRequiresDedicatedToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controller := &TenantReloadController{}
	router := gin.New()
	if err := RegisterTenantReloadRoute(router, "reload-secret", controller); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/_kora/admin/reload-site", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing-token response = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	request := httptest.NewRequest(http.MethodPost, "/_kora/admin/reload-site", nil)
	request.Header.Set("Authorization", "Bearer reload-secret")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("malformed-body response = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestTenantReloadAdmissionRejectsBeforeDirectoryRead(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	admission, err := dbaccess.NewAdmission(1, 0, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &knet.LoadedSite{SiteID: "tenant-reload", Name: "reload.example"}
	controller := &TenantReloadController{
		Tenants: knet.NewSiteRouter([]*knet.LoadedSite{runtime}), Directory: site.NewSQLSiteRegistry(database, "mysql"),
		PlatformDB: database, Coordinator: &TenantRuntimeCoordinator{}, WorkAdmission: admission,
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if err := RegisterTenantReloadRoute(router, "reload-secret", controller); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/_kora/admin/reload-site", strings.NewReader(`{"site":"reload.example"}`))
	request.Header.Set("Authorization", "Bearer reload-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("capacity-rejected reload = %d, Retry-After %q, body %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
	release()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("reload queried the directory without admission: %v", err)
	}
}

func TestTenantReloadRetainsCandidateWhenCleanupFails(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("candidate database close failed")
	mock.ExpectClose().WillReturnError(closeErr)
	runtime := &knet.LoadedSite{Name: "candidate.example", DB: database}
	retained := false
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: knet.NewSiteRouter(nil), Common: &site.CommonConfig{},
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(candidate *knet.LoadedSite) { retained = candidate == runtime },
	}, func(context.Context, site.DBSiteInfo, *site.CommonConfig, *knet.LoadedSite, string) (*knet.LoadedSite, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("runtime publication failed")
	got := coordinator.discard(context.Background(), runtime, false, cause)
	if !errors.Is(got, cause) || !errors.Is(got, closeErr) {
		t.Fatalf("discardCandidate error = %v, want cause and close failure", got)
	}
	if !retained {
		t.Fatal("candidate with failed cleanup was not retained by the process owner")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTenantReloadControllerReplacesExistingRuntime(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registry := site.NewSQLSiteRegistry(database, "mysql")
	columns := []string{"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision"}
	query := `SELECT site_id, COALESCE(runtime_cell_id, ''), site, db_type, db_host, db_port, db_name, db_user, COALESCE(db_password, ''), db_password_encrypted, COALESCE(domains_json, '[]'), COALESCE(file_storage, 'local'), COALESCE(storage_bucket, ''), status, config_revision FROM _kora_site_registry WHERE site_id = ?`
	mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs("tenant-reload").WillReturnRows(
		sqlmock.NewRows(columns).AddRow("tenant-reload", "", "reload.example", "mysql", "127.0.0.1", 3306, "tenant_reload", "user", "", 0, "[]", "local", "", "active", 2),
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1")).WillReturnRows(
		sqlmock.NewRows([]string{"directory_revision"}).AddRow(42),
	)
	runtime := &knet.LoadedSite{
		SiteID: "tenant-reload", Name: "reload.example",
		Config: knet.SiteRouterConfig{Hostname: "reload.example", Domains: []string{"reload.example"}},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	replaced := false
	coordinator, err := newTenantRuntimeCoordinator(TenantRuntimeCoordinatorConfig{
		Tenants: router, Common: &site.CommonConfig{},
		Installer:     func(site.DBSiteInfo, *knet.LoadedSite) error { return nil },
		RetainRuntime: func(*knet.LoadedSite) {},
	}, func(_ context.Context, info site.DBSiteInfo, _ *site.CommonConfig, _ *knet.LoadedSite, _ string) (*knet.LoadedSite, error) {
		replaced = info.SiteID == "tenant-reload"
		return &knet.LoadedSite{SiteID: info.SiteID, Name: info.Name, Config: knet.SiteRouterConfig{Hostname: info.Name}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer coordinator.WaitRetired(context.Background())
	controller := &TenantReloadController{
		Tenants: router, Directory: registry, PlatformDB: database, Coordinator: coordinator,
	}
	gin.SetMode(gin.TestMode)
	httpRouter := gin.New()
	if err := RegisterTenantReloadRoute(httpRouter, "reload-secret", controller); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/_kora/admin/reload-site", strings.NewReader(`{"site":" reload.example "}`))
	request.Header.Set("Authorization", "Bearer reload-secret")
	response := httptest.NewRecorder()
	httpRouter.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("reload response = %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Site   string `json:"site"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !replaced || body.Status != "reloaded" || body.Site != "reload.example" {
		t.Fatalf("reload result = %+v, replaced=%t", body, replaced)
	}
	active := router.SnapshotByID("tenant-reload")
	if active == nil || active.Activation.DirectoryRevision != 42 {
		t.Fatalf("replacement activation = %#v, want directory revision 42", active)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
