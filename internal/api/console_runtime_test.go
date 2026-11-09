package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/script"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

func TestOnboardRateLimiterUsesFixedWindowWithoutSweeper(t *testing.T) {
	limiter := newOnboardRateLimiter(2)
	first := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	if !limiter.allow("192.0.2.1", first) || !limiter.allow("192.0.2.1", first.Add(10*time.Minute)) {
		t.Fatal("requests within the limit were rejected")
	}
	if limiter.allow("192.0.2.1", first.Add(20*time.Minute)) {
		t.Fatal("request exceeded the hourly limit")
	}
	if !limiter.allow("192.0.2.1", first.Add(time.Hour)) {
		t.Fatal("request in the next fixed window was rejected")
	}
	if !limiter.allow("192.0.2.2", first.Add(time.Hour)) {
		t.Fatal("rate limit was not scoped by client IP")
	}
}

func TestHandleUpdateSiteRefreshesRuntimeAfterDirectoryCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	platformDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	expectSiteConfigUpdate(mock, "refresh.example.test", "site-config-refresh", ` ["refresh.example.test","files.example.test"] `)

	old := &net.LoadedSite{
		SiteID: "site-config-refresh", Name: "refresh.example.test", ConfigRevision: 2, Status: "active",
		Config:   net.SiteRouterConfig{Hostname: "refresh.example.test", Domains: []string{"refresh.example.test"}, FileStorage: "local"},
		Registry: doctype.NewRegistry(),
	}
	router := net.NewSiteRouter([]*net.LoadedSite{old})
	platformAdmission := &capacityOneConsoleWorkAdmission{}
	reloaded := false
	handler := &ConsoleHandler{
		SiteRouter: router, PlatformDB: platformDB, PlatformDBType: "mysql", PlatformWorkAdmission: platformAdmission,
		ReloadRuntime: func(ctx context.Context, name string) error {
			reloaded = true
			if active := platformAdmission.active.Load(); active != 0 {
				return errors.New("platform work permit was held while tenant runtime reload began")
			}
			if name != old.Name {
				return errors.New("reload used an unexpected tenant name")
			}
			current := router.SnapshotByID(old.SiteID)
			if current == nil || current.Config.FileStorage != "local" {
				return errors.New("new generation was published before durable directory update")
			}
			next := &net.LoadedSite{
				SiteID: old.SiteID, Name: old.Name, ConfigRevision: 3, DirectoryRevision: 1, Status: "active",
				Config:   net.SiteRouterConfig{Hostname: old.Name, Domains: []string{old.Name, "files.example.test"}, FileStorage: "s3", StorageBucket: "refresh-example-test"},
				Registry: old.Registry,
			}
			retired, err := router.ReplaceSiteContext(ctx, next)
			if err != nil {
				return err
			}
			if retired != old {
				return errors.New("replacement did not retire the pinned generation")
			}
			if err := retired.Close(ctx); err != nil {
				return err
			}
			router.FinishRetirement(retired)
			return nil
		},
	}
	request := httptest.NewRequest(http.MethodPut, "/api/console/sites/refresh.example.test", strings.NewReader(`{"domains":["refresh.example.test","files.example.test"],"file_storage":"s3"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	route := gin.New()
	route.PUT("/api/console/sites/:name", handler.RequirePlatformWork, handler.HandleUpdateSite)
	route.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !reloaded {
		t.Fatalf("update status=%d reloaded=%v body=%s", response.Code, reloaded, response.Body.String())
	}
	current := router.SnapshotByID(old.SiteID)
	if current == nil || current.ConfigRevision != 3 || current.Config.FileStorage != "s3" || current.Config.StorageBucket != "refresh-example-test" {
		t.Fatalf("new runtime snapshot = %#v", current)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if active := platformAdmission.active.Load(); active != 0 || platformAdmission.releases.Load() != 1 {
		t.Fatalf("platform permit after reload: active=%d releases=%d", active, platformAdmission.releases.Load())
	}
}

func TestHandleUpdateSiteReportsPersistedConfigWhenRuntimeRefreshFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	platformDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer platformDB.Close()
	expectSiteConfigUpdate(mock, "refresh-fails.example.test", "site-config-refresh", ` ["refresh-fails.example.test"] `)
	old := &net.LoadedSite{
		SiteID: "site-config-refresh-fails", Name: "refresh-fails.example.test", ConfigRevision: 2, Status: "active",
		Config:   net.SiteRouterConfig{Hostname: "refresh-fails.example.test", Domains: []string{"refresh-fails.example.test"}, FileStorage: "local"},
		Registry: doctype.NewRegistry(),
	}
	router := net.NewSiteRouter([]*net.LoadedSite{old})
	handler := &ConsoleHandler{
		SiteRouter: router, PlatformDB: platformDB, PlatformDBType: "mysql",
		ReloadRuntime: func(context.Context, string) error { return errors.New("tenant database unavailable") },
	}
	request := httptest.NewRequest(http.MethodPut, "/api/console/sites/refresh-fails.example.test", strings.NewReader(`{"file_storage":"s3"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	ctx.Params = gin.Params{{Key: "name", Value: old.Name}}
	handler.HandleUpdateSite(ctx)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "runtime_refresh_pending") {
		t.Fatalf("refresh failure status=%d body=%s", response.Code, response.Body.String())
	}
	current := router.SnapshotByID(old.SiteID)
	if current == nil || current.ConfigRevision != 2 || current.Config.FileStorage != "local" {
		t.Fatalf("failed refresh published unprepared runtime: %#v", current)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectSiteConfigUpdate(mock sqlmock.Sqlmock, hostname, siteID, domains string) {
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_registry
				 SET file_storage = ?, storage_bucket = ?, domains_json = ?, status = 'active', config_revision = config_revision + 1, updated_at = ?
				 WHERE site = ?`)).
		WithArgs("s3", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), hostname).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id FROM _kora_site_registry WHERE site = ?`)).
		WithArgs(hostname).WillReturnRows(sqlmock.NewRows([]string{"site_id"}).AddRow(siteID))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM _kora_site_alias WHERE site_id = ?`)).
		WithArgs(siteID).WillReturnResult(sqlmock.NewResult(0, 1))
	aliases := []string{hostname}
	if hostname == "refresh.example.test" {
		aliases = append(aliases, "files.example.test")
	}
	for _, alias := range aliases {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_alias (alias, site_id) VALUES (?, ?)`)).
			WithArgs(alias, siteID).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_alias_meta (id, initialized) VALUES (1, ?) ON DUPLICATE KEY UPDATE initialized = VALUES(initialized)`)).
		WithArgs(1).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT site_id, COALESCE(runtime_cell_id, ''), site, COALESCE(domains_json, '[]'), status, config_revision FROM _kora_site_registry WHERE site = ?`)).
		WithArgs(hostname).WillReturnRows(sqlmock.NewRows([]string{"site_id", "runtime_cell_id", "site", "domains_json", "status", "config_revision"}).AddRow(siteID, "", hostname, domains, "active", 3))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE _kora_site_directory_meta SET directory_revision = directory_revision + 1 WHERE id = 1`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT directory_revision FROM _kora_site_directory_meta WHERE id = 1`)).
		WillReturnRows(sqlmock.NewRows([]string{"directory_revision"}).AddRow(1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO _kora_site_directory_outbox (id, directory_revision, site_id, config_revision, payload) VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(sqlmock.AnyArg(), uint64(1), siteID, uint64(3), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
}

func TestPersistOnboardingPropagatesDurableStateWriteFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
	}{
		{name: "job state"},
		{name: "checkpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			errWrite := errors.New("disk/database unavailable")
			if tc.name == "checkpoint" {
				mock.ExpectExec(`INSERT INTO _kora_provisioning_job`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO _kora_provisioning_checkpoint`).WillReturnError(errWrite)
			} else {
				mock.ExpectExec(`INSERT INTO _kora_provisioning_job`).WillReturnError(errWrite)
			}
			handler := &ConsoleHandler{ProvisioningStore: site.NewOnboardingStore(database, "mysql")}
			job := site.OnboardingJob{ID: "job-1", Site: "tenant-a", Resource: "tenant-a", OperationID: "op-1", IdempotencyKey: "key", InputFingerprint: "fingerprint"}
			if err := handler.persistOnboarding(job, "requested"); err == nil {
				t.Fatal("persistence failure was reported as success")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareCreatedRuntimeLoadsDirectoryRevisionBeforePublication(t *testing.T) {
	cfg := &site.Config{
		Hostname: "new-site.example", DBType: "postgres", DBHost: "db.internal", DBPort: 5432,
		DBName: "new_site", DBUser: "engine", DBPassword: "secret", DomainsList: []string{"new-site.example", "alias.example"},
		FileStorage: "local",
	}
	loaded := &net.LoadedSite{Name: cfg.Hostname}
	called := false
	handler := &ConsoleHandler{PrepareRuntime: func(info site.DBSiteInfo, runtime *net.LoadedSite) error {
		called = true
		if info.SiteID != "site-created" || info.ConfigRevision != 1 || info.Status != "active" {
			t.Fatalf("prepared with incomplete directory info: %#v", info)
		}
		if info.DBPassword != "secret" {
			t.Fatal("internal runtime descriptor should contain credentials needed to construct its DB config")
		}
		services := SiteRuntimeService{ScriptStore: &script.Store{}}
		runtime.RuntimeServices = &services
		return nil
	}}
	if err := handler.prepareCreatedRuntime(context.Background(), loaded, "site-created", cfg); err != nil {
		t.Fatal(err)
	}
	if !called || loaded.ConfigRevision != 1 || loaded.Status != "active" || loaded.RuntimeServiceSnapshot().ScriptStore == nil {
		t.Fatalf("runtime was not prepared before publication: called=%v loaded=%#v", called, loaded)
	}
}

func TestPrepareCreatedRuntimeReleasesDirectoryAdmissionBeforeTenantInstaller(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal("create platform database:", err)
	}
	defer database.Close()
	columns := []string{"site_id", "runtime_cell_id", "site", "db_type", "db_host", "db_port", "db_name", "db_user", "db_password", "db_password_encrypted", "domains_json", "file_storage", "storage_bucket", "status", "config_revision"}
	mock.ExpectQuery("SELECT site_id, COALESCE\\(runtime_cell_id").WithArgs("site-created").WillReturnRows(
		sqlmock.NewRows(columns).AddRow("site-created", "", "new-site.example", "mysql", "localhost", 3306, "new_site", "engine", "", 0, `["new-site.example"]`, "local", "", "active", 1),
	)
	admission := &capacityOneConsoleWorkAdmission{}
	loaded := &net.LoadedSite{Name: "new-site.example"}
	handler := &ConsoleHandler{
		PlatformDB: database, PlatformDBType: "mysql", PlatformWorkAdmission: admission,
		PrepareRuntime: func(site.DBSiteInfo, *net.LoadedSite) error {
			release, err := admission.Acquire(context.Background())
			if err != nil {
				return err
			}
			release()
			return nil
		},
	}
	if err := handler.prepareCreatedRuntime(context.Background(), loaded, "site-created", &site.Config{Hostname: loaded.Name}); err != nil {
		t.Fatal("prepare created runtime with one shared work slot:", err)
	}
	if admission.calls.Load() != 2 || admission.releases.Load() != 2 || admission.active.Load() != 0 {
		t.Fatalf("platform lookup/tenant install admission calls=%d releases=%d active=%d", admission.calls.Load(), admission.releases.Load(), admission.active.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal("created-site directory lookup:", err)
	}
}

func TestPrepareCreatedRuntimeDiscardsPartiallyInstalledServicesOnFailure(t *testing.T) {
	loaded := &net.LoadedSite{Name: "new-site.example"}
	discarded := false
	handler := &ConsoleHandler{
		PrepareRuntime: func(site.DBSiteInfo, *net.LoadedSite) error { return errors.New("sidecar failed") },
		DiscardRuntime: func(runtime *net.LoadedSite) {
			discarded = runtime == loaded
		},
	}
	if err := handler.prepareCreatedRuntime(context.Background(), loaded, "site-created", &site.Config{Hostname: loaded.Name}); err == nil {
		t.Fatal("expected runtime preparation failure")
	}
	if !discarded {
		t.Fatal("partially installed services were not discarded")
	}
}
