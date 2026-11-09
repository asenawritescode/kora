package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

type consoleWorkAdmission struct {
	err      error
	calls    atomic.Int32
	releases atomic.Int32
}

type capacityOneConsoleWorkAdmission struct {
	active   atomic.Int32
	calls    atomic.Int32
	releases atomic.Int32
}

func (a *capacityOneConsoleWorkAdmission) Acquire(context.Context) (func(), error) {
	a.calls.Add(1)
	if !a.active.CompareAndSwap(0, 1) {
		return nil, errors.New("work capacity exhausted")
	}
	var released atomic.Bool
	return func() {
		if released.CompareAndSwap(false, true) {
			a.active.Store(0)
			a.releases.Add(1)
		}
	}, nil
}

func (a *consoleWorkAdmission) Acquire(context.Context) (func(), error) {
	a.calls.Add(1)
	if a.err != nil {
		return nil, a.err
	}
	return func() { a.releases.Add(1) }, nil
}

func TestRequirePlatformWorkRejectsWhenCapacityIsExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission := &consoleWorkAdmission{err: errors.New("platform capacity exhausted")}
	handler := &ConsoleHandler{PlatformWorkAdmission: admission}
	called := false
	router := gin.New()
	router.GET("/console", handler.RequirePlatformWork, func(c *gin.Context) {
		called = true
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/console", nil))
	if response.Code != http.StatusServiceUnavailable || called || admission.calls.Load() != 1 || admission.releases.Load() != 0 {
		t.Fatalf("response=%d called=%t acquire=%d release=%d", response.Code, called, admission.calls.Load(), admission.releases.Load())
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", response.Header().Get("Retry-After"))
	}
}

func TestRequirePlatformWorkReleasesPermitAfterHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission := &consoleWorkAdmission{}
	handler := &ConsoleHandler{PlatformWorkAdmission: admission}
	router := gin.New()
	router.GET("/console", handler.RequirePlatformWork, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/console", nil))
	if response.Code != http.StatusNoContent || admission.calls.Load() != 1 || admission.releases.Load() != 1 {
		t.Fatalf("response=%d acquire=%d release=%d", response.Code, admission.calls.Load(), admission.releases.Load())
	}
}

func TestRequirePlatformWorkCanReleaseBeforeTenantAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	admission := &capacityOneConsoleWorkAdmission{}
	handler := &ConsoleHandler{PlatformWorkAdmission: admission}
	router := gin.New()
	router.POST("/console", handler.RequirePlatformWork, func(c *gin.Context) {
		releasePlatformWork(c)
		releaseTenantWork, err := handler.acquirePlatformWork(c.Request.Context())
		if err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		releaseTenantWork()
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/console", nil))
	if response.Code != http.StatusNoContent || admission.calls.Load() != 2 || admission.releases.Load() != 2 || admission.active.Load() != 0 {
		t.Fatalf("response=%d acquire=%d release=%d active=%d; want 204 and two nonnested permits", response.Code, admission.calls.Load(), admission.releases.Load(), admission.active.Load())
	}
}

func TestWithTenantSiteAdmissionGatesCreateAndReleasesPermit(t *testing.T) {
	config := &site.SiteConfig{DBType: "mysql", DBHost: "tenant.invalid", DBPort: 3306, DBName: "tenant"}
	var admitted, released atomic.Int32
	called := false
	handler := &ConsoleHandler{TenantSiteAdmission: func(context.Context, *site.SiteConfig) (func(), error) {
		admitted.Add(1)
		return func() { released.Add(1) }, nil
	}}
	if err := handler.withTenantSiteAdmission(context.Background(), config, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal("admitted tenant site operation:", err)
	}
	if !called || admitted.Load() != 1 || released.Load() != 1 {
		t.Fatalf("called=%t admitted=%d released=%d; want true/1/1", called, admitted.Load(), released.Load())
	}

	called = false
	handler.TenantSiteAdmission = func(context.Context, *site.SiteConfig) (func(), error) {
		return nil, errors.New("tenant capacity exhausted")
	}
	if err := handler.withTenantSiteAdmission(context.Background(), config, func() error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatalf("saturated tenant admission = (err %v, called %t), want failure before create", err, called)
	}
}

func TestHandleCreateSiteReturnsRetryableCapacityResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &ConsoleHandler{TenantSiteAdmission: func(context.Context, *site.SiteConfig) (func(), error) {
		return nil, ErrTenantProvisioningCapacity
	}}
	router := gin.New()
	router.POST("/sites", handler.HandleCreateSite)
	request := httptest.NewRequest(http.MethodPost, "/sites", strings.NewReader(`{"hostname":"tenant.example","admin_email":"admin@example.test","admin_password":"secret"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("site create response=(%d, retry-after=%q), want 503/1", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestOnboardingRetriesTemporaryTenantCapacityExhaustion(t *testing.T) {
	config := &site.SiteConfig{DBType: "mysql", DBHost: "tenant.invalid", DBPort: 3306, DBName: "tenant"}
	var attempts, created, released atomic.Int32
	handler := &ConsoleHandler{TenantSiteAdmission: func(context.Context, *site.SiteConfig) (func(), error) {
		if attempts.Add(1) == 1 {
			return nil, ErrTenantProvisioningCapacity
		}
		return func() { released.Add(1) }, nil
	}}
	err := handler.withTenantSiteAdmissionRetry(context.Background(), config, func() error {
		created.Add(1)
		return nil
	})
	if err != nil || attempts.Load() != 2 || created.Load() != 1 || released.Load() != 1 {
		t.Fatalf("retry result=(%v, attempts=%d, created=%d, released=%d), want nil/2/1/1", err, attempts.Load(), created.Load(), released.Load())
	}
}

func TestScheduleOnboardBoundsRunningProvisioningJobs(t *testing.T) {
	handler := &ConsoleHandler{queuedJobs: make(map[string]bool)}
	for i := range maxConcurrentOnboardingJobs {
		handler.queuedJobs[string(rune('a'+i))] = true
	}
	if handler.scheduleOnboard(site.OnboardingJob{ID: "overflow"}, onboardRequest{}) {
		t.Fatal("onboarding job was scheduled beyond the process capacity")
	}
	if len(handler.queuedJobs) != maxConcurrentOnboardingJobs {
		t.Fatalf("onboarding queue size = %d, want %d", len(handler.queuedJobs), maxConcurrentOnboardingJobs)
	}
	if err := handler.CloseOnboardingJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
}
