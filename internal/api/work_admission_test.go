package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/gin-gonic/gin"
)

func TestWorkAdmissionMiddlewareBoundsRequestAndReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("tenant", SiteRuntimeService{WorkAdmission: admission})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", "tenant")
		c.Next()
	}, WorkAdmissionMiddleware(services))
	called := false
	router.GET("/api/v1/test", func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})

	release, err := admission.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	blocked := httptest.NewRecorder()
	router.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/api/v1/test", nil))
	if blocked.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("saturated request status=%d handler_called=%v", blocked.Code, called)
	}
	if got := blocked.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	release()

	accepted := httptest.NewRecorder()
	router.ServeHTTP(accepted, httptest.NewRequest(http.MethodGet, "/api/v1/test", nil))
	if accepted.Code != http.StatusNoContent || !called {
		t.Fatalf("available request status=%d handler_called=%v", accepted.Code, called)
	}
	if got := admission.Snapshot(); got.Active != 0 || got.Waiters != 0 || got.Rejected != 1 {
		t.Fatalf("admission leaked after request: %#v", got)
	}
}

func TestWorkAdmissionMiddlewareIsIdempotentAcrossRouterAndGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("tenant", SiteRuntimeService{WorkAdmission: admission})
	router := gin.New()
	setTenant := func(c *gin.Context) {
		c.Set("site_name", "tenant")
		c.Set("site_runtime_services", mustGetServices(t, services))
		c.Next()
	}
	router.Use(setTenant, WorkAdmissionMiddleware(nil))
	group := router.Group("/api")
	group.Use(WorkAdmissionMiddleware(services))
	group.GET("/work", func(c *gin.Context) {
		if got := admission.Snapshot().Active; got != 1 {
			t.Errorf("active permits during handler = %d, want one", got)
		}
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/work", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("double admission rejected request: status=%d body=%s", response.Code, response.Body.String())
	}
	if got := admission.Snapshot().Active; got != 0 {
		t.Fatalf("request leaked active permit: %d", got)
	}
}

func mustGetServices(t *testing.T, services *SiteRuntimeServices) SiteRuntimeService {
	t.Helper()
	value, ok := services.Get("tenant")
	if !ok {
		t.Fatal("runtime service snapshot was not installed")
	}
	return value
}

func TestWorkAdmissionMiddlewareAdmitsStreamAuthenticationThenReleases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	services := NewSiteRuntimeServices()
	services.Replace("tenant", SiteRuntimeService{WorkAdmission: admission})
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("site_name", "tenant"); c.Next() }, WorkAdmissionMiddleware(services))
	streamHandler := func(c *gin.Context) {
		ReleaseTenantStreamAuthWork(c)
		if got := admission.Snapshot().Active; got != 0 {
			t.Errorf("stream active work after authentication = %d, want 0", got)
		}
		c.Status(http.StatusNoContent)
	}
	router.GET("/api/v1/system/realtime", streamHandler)
	router.HEAD("/api/v1/files/*path", streamHandler)
	router.GET("/api/v1/public/files/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.DELETE("/api/v1/files/*path", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	release, err := admission.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/system/realtime", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated realtime auth status=%d, want 503", response.Code)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/public/files/image.png", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("public file stream status=%d, want 204", response.Code)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/v1/files/video.mp4", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated file deletion status=%d, want 503", response.Code)
	}
	release()
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/system/realtime", nil),
		httptest.NewRequest(http.MethodHead, "/api/v1/files/video.mp4", nil),
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("stream request %s %s status=%d, want 204", request.Method, request.URL.Path, response.Code)
		}
	}
}
