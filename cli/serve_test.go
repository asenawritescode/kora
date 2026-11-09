package cli

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/internal/engine"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
	"github.com/gin-gonic/gin"
)

func TestFallbackPathSiteRoutingStillDispatchesAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	siteRouter := knet.NewSiteRouter([]*knet.LoadedSite{
		{
			Name:     "live-demo",
			Config:   knet.SiteRouterConfig{Hostname: "live-demo"},
			DB:       &sql.DB{},
			Registry: doctype.NewRegistry(),
		},
	})
	router.Use(siteRouter.Middleware())

	// This matches the non-SPA fallback branch in runServe.
	knet.RegisterPathSiteRoutes(router, siteRouter)

	router.GET("/api/test-site", func(c *gin.Context) {
		if _, ok := c.Get("site_db"); !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "site_db_missing"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"site": c.GetString("site_name")})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/s/live-demo/api/test-site", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if body := w.Body.String(); body != "{\"site\":\"live-demo\"}" {
		t.Fatalf("body = %s", body)
	}
}

func TestAsyncHookWorkerExitsAfterQueueClose(t *testing.T) {
	worker, err := engine.StartAsyncHookWorker(1, nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	worker.StopAccepting()
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("async hook worker did not stop after queue close")
	}
	worker.Cancel()
}

func TestAsyncHookLimitEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_ASYNC_HOOK_QUEUE_SIZE", "12")
	if got, err := envPositiveInt("KORA_ASYNC_HOOK_QUEUE_SIZE", engine.DefaultAsyncHookQueueSize); err != nil || got != 12 {
		t.Fatalf("positive int = (%d, %v)", got, err)
	}
	t.Setenv("KORA_ASYNC_HOOK_MAX_EVENT_BYTES", "0")
	if _, err := envPositiveUint64("KORA_ASYNC_HOOK_MAX_EVENT_BYTES", engine.DefaultAsyncHookMaxEventBytes); err == nil {
		t.Fatal("zero event-byte cap accepted")
	}
	t.Setenv("KORA_ASYNC_HOOK_QUEUE_SIZE", "-1")
	if _, err := envPositiveInt("KORA_ASYNC_HOOK_QUEUE_SIZE", engine.DefaultAsyncHookQueueSize); err == nil {
		t.Fatal("negative queue size accepted")
	}
}

func TestRealtimeStreamEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_REALTIME_MAX_STREAMS", "24")
	if got, err := envPositiveInt("KORA_REALTIME_MAX_STREAMS", engine.DefaultRealtimeMaxStreams); err != nil || got != 24 {
		t.Fatalf("positive stream cap = (%d, %v)", got, err)
	}
	for _, raw := range []string{"0", "-1", "4097"} {
		t.Setenv("KORA_REALTIME_MAX_STREAMS", raw)
		got, err := envPositiveInt("KORA_REALTIME_MAX_STREAMS", engine.DefaultRealtimeMaxStreams)
		if raw == "4097" {
			if err != nil || got <= engine.MaxRealtimeMaxStreams {
				t.Fatalf("oversized stream cap parsed as (%d, %v); expected later max-bound rejection", got, err)
			}
		} else if err == nil {
			t.Fatalf("invalid stream cap %q accepted", raw)
		}
	}
}

func TestSessionCacheProcessBudgetEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_SESSION_CACHE_MAX_BYTES", "1048576")
	if got, err := sessionCacheBudgetFromEnv(); err != nil || got != 1048576 {
		t.Fatalf("positive cache byte cap = (%d, %v)", got, err)
	}
	for _, raw := range []string{"0", "-1", "not-a-number"} {
		t.Setenv("KORA_SESSION_CACHE_MAX_BYTES", raw)
		if _, err := sessionCacheBudgetFromEnv(); err == nil {
			t.Fatalf("invalid cache byte cap %q accepted", raw)
		}
	}
	t.Setenv("KORA_SESSION_CACHE_MAX_BYTES", strconv.FormatUint(auth.MaxSessionCacheBudgetBytes+1, 10))
	if _, err := sessionCacheBudgetFromEnv(); err == nil {
		t.Fatal("aggregate cache byte cap above hard maximum accepted")
	}
}

func TestAnalyticsQueueBudgetEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", "2097152")
	t.Setenv("KORA_ANALYTICS_MAX_QUEUED_BYTES", "1048576")
	if got, err := analyticsQueueBudgetFromEnv(); err != nil || got != 2097152 {
		t.Fatalf("explicit process analytics queue budget = (%d, %v), want 2097152", got, err)
	}
	t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", "")
	if got, err := analyticsQueueBudgetFromEnv(); err != nil || got != 1048576 {
		t.Fatalf("analytics subscriber queue budget = (%d, %v)", got, err)
	}
	for _, raw := range []string{"0", "-1", "bad"} {
		t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", raw)
		if _, err := analyticsQueueBudgetFromEnv(); err == nil {
			t.Fatalf("invalid process subscriber queue byte cap %q accepted", raw)
		}
	}
	t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", "")
	for _, raw := range []string{"0", "-1", "bad"} {
		t.Setenv("KORA_ANALYTICS_MAX_QUEUED_BYTES", raw)
		if _, err := analyticsQueueBudgetFromEnv(); err == nil {
			t.Fatalf("invalid subscriber queue byte cap %q accepted", raw)
		}
	}
	t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", strconv.FormatUint(analytics.MaxAnalyticsQueueAdmissionBytes+1, 10))
	if _, err := analyticsQueueBudgetFromEnv(); err == nil {
		t.Fatal("process subscriber queue byte cap above hard maximum accepted")
	}
	t.Setenv("KORA_ANALYTICS_PROCESS_MAX_QUEUED_BYTES", "")
	t.Setenv("KORA_ANALYTICS_MAX_QUEUED_BYTES", strconv.FormatUint(analytics.MaxAnalyticsQueueAdmissionBytes+1, 10))
	if _, err := analyticsQueueBudgetFromEnv(); err == nil {
		t.Fatal("subscriber queue byte cap above hard maximum accepted")
	}
}

func TestAnalyticsSubscriberLimitEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_ANALYTICS_MAX_SUBSCRIBERS", "2048")
	if got, err := analyticsSubscriberLimitFromEnv(); err != nil || got != 2048 {
		t.Fatalf("analytics process subscriber limit = (%d, %v)", got, err)
	}
	for _, raw := range []string{"0", "-1", "bad"} {
		t.Setenv("KORA_ANALYTICS_MAX_SUBSCRIBERS", raw)
		if _, err := analyticsSubscriberLimitFromEnv(); err == nil {
			t.Fatalf("invalid analytics process subscriber limit %q accepted", raw)
		}
	}
	t.Setenv("KORA_ANALYTICS_MAX_SUBSCRIBERS", strconv.Itoa(analytics.MaxAnalyticsProcessListeners+1))
	if _, err := analyticsSubscriberLimitFromEnv(); err == nil {
		t.Fatal("analytics process subscriber limit above hard maximum accepted")
	}
}

func TestUploadAdmissionEnvironmentValidation(t *testing.T) {
	t.Setenv("KORA_UPLOAD_MAX_ACTIVE", "3")
	t.Setenv("KORA_UPLOAD_ACTIVE_BUDGET_BYTES", "1048576")
	active, bytes, err := uploadAdmissionFromEnv()
	if err != nil || active != 3 || bytes != 1048576 {
		t.Fatalf("upload admission config = (%d, %d, %v)", active, bytes, err)
	}
	t.Setenv("KORA_UPLOAD_MAX_ACTIVE", strconv.Itoa(engine.MaxUploadMaxActive+1))
	if _, _, err := uploadAdmissionFromEnv(); err == nil {
		t.Fatal("upload active count above hard maximum accepted")
	}
	t.Setenv("KORA_UPLOAD_MAX_ACTIVE", "2")
	t.Setenv("KORA_UPLOAD_ACTIVE_BUDGET_BYTES", strconv.FormatUint(engine.MaxUploadActiveBytes+1, 10))
	if _, _, err := uploadAdmissionFromEnv(); err == nil {
		t.Fatal("upload byte budget above hard maximum accepted")
	}
}

func TestAsyncHookWorkerPinsTenantRuntimeUntilExecutionFinishes(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	mock.ExpectClose()
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Thing"})
	runtime := &knet.LoadedSite{
		SiteID: "tenant-async-hook", Name: "async.example", DBType: "mysql", DB: database,
		Registry: registry, Config: knet.SiteRouterConfig{Hostname: "async.example", Domains: []string{"async.example"}},
	}
	router := knet.NewSiteRouter([]*knet.LoadedSite{runtime})
	entered, release := make(chan struct{}), make(chan struct{})
	worker, err := engine.StartAsyncHookWorker(1, blockingAsyncHookRunner{entered: entered, release: release}, router, "mysql", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Sink().Enqueue(context.Background(), orm.AsyncHookRequest{Site: runtime.Name, Doctype: "Thing", Rec: script.ScriptRecord{Name: "after_save"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("async hook execution did not start")
	}
	if retired := router.RemoveSiteByID(runtime.SiteID); retired != runtime {
		t.Fatal("runtime was not retired while hook was running")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close(context.Background()) }()
	select {
	case err := <-closeDone:
		t.Fatalf("runtime closed while its async hook held a handle: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := database.Ping(); err != nil {
		t.Fatalf("tenant DB closed while hook was running: %v", err)
	}
	close(release)
	worker.StopAccepting()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close runtime after hook completion: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime close did not finish after async hook released its handle")
	}
	select {
	case <-worker.Done():
	case <-time.After(time.Second):
		t.Fatal("async hook worker did not exit after queue close")
	}
	worker.Cancel()
	router.FinishRetirement(runtime)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type blockingAsyncHookRunner struct {
	entered chan struct{}
	release chan struct{}
}

func (r blockingAsyncHookRunner) Execute(context.Context, script.ExecuteRequest) (*script.ExecuteResult, error) {
	close(r.entered)
	<-r.release
	return nil, nil
}

func (blockingAsyncHookRunner) Validate(string) error { return nil }
func (blockingAsyncHookRunner) Close() error          { return nil }

func TestRouterOnlyTrustsForwardedClientIPFromConfiguredProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		proxies    []string
		remoteAddr string
		wantIP     string
	}{
		{name: "default ignores forwarded header", remoteAddr: "192.0.2.10:1234", wantIP: "192.0.2.10"},
		{name: "explicit empty list ignores forwarded header", proxies: []string{}, remoteAddr: "192.0.2.10:1234", wantIP: "192.0.2.10"},
		{name: "trusted proxy may forward client address", proxies: []string{"192.0.2.0/24"}, remoteAddr: "192.0.2.10:1234", wantIP: "203.0.113.7"},
		{name: "untrusted peer cannot forward client address", proxies: []string{"192.0.2.0/24"}, remoteAddr: "198.51.100.10:1234", wantIP: "198.51.100.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			if err := router.SetTrustedProxies(tt.proxies); err != nil {
				t.Fatalf("configure trusted proxies: %v", err)
			}
			router.GET("/", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = tt.remoteAddr
			request.Header.Set("X-Forwarded-For", "203.0.113.7")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Body.String() != tt.wantIP {
				t.Fatalf("ClientIP() = %q (status %d), want %q", response.Body.String(), response.Code, tt.wantIP)
			}
		})
	}
}

func TestRouterRejectsInvalidTrustedProxyConfiguration(t *testing.T) {
	router := gin.New()
	if err := router.SetTrustedProxies([]string{"not-a-cidr"}); err == nil {
		t.Fatal("invalid trusted proxy configuration was accepted")
	}
}

func TestOutboxEnabledByDefaultForLocalAnalytics(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		broker bool
		want   bool
	}{
		{name: "unset uses local transactional delivery", want: true},
		{name: "explicit true", value: "true", want: true},
		{name: "explicit false disables local delivery", value: "false", want: false},
		{name: "zero disables local delivery", value: "0", want: false},
		{name: "broker requires durable delivery", value: "false", broker: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := outboxEnabledFromEnv(tt.value, tt.broker); got != tt.want {
				t.Fatalf("outboxEnabledFromEnv(%q, %t) = %t, want %t", tt.value, tt.broker, got, tt.want)
			}
		})
	}
}
