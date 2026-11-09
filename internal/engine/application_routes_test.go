package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/doctype"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

type routeWorkAdmissionFunc func(context.Context) (func(), error)

func (f routeWorkAdmissionFunc) Acquire(ctx context.Context) (func(), error) { return f(ctx) }

func TestRegisterApplicationRoutesRequiresRouter(t *testing.T) {
	if manager, err := RegisterApplicationRoutes(ApplicationRouteConfig{}); err == nil || manager != nil {
		t.Fatalf("RegisterApplicationRoutes = (%v, %v), want missing-router error", manager, err)
	}
}

func TestBuildApplicationOwnsRouterAndDependencies(t *testing.T) {
	application, err := BuildApplication(ApplicationConfig{
		Common: &site.CommonConfig{
			DBType: "mysql", AdminRole: "System Manager", SessionLifetimeHours: 1,
			APIDefaultLimit: 20, APIMaxLimit: 100,
		},
		Tenants: knet.NewSiteRouter(nil), BinaryVersion: "test",
	})
	if err != nil {
		t.Fatalf("BuildApplication: %v", err)
	}
	if application == nil || application.Router == nil || application.Registry == nil || application.Transactions == nil || application.SessionManager == nil || application.AnalyticsCache == nil {
		t.Fatalf("BuildApplication returned incomplete application: %#v", application)
	}
	if err := application.SessionManager.Close(); err != nil {
		t.Fatalf("close application session manager: %v", err)
	}
	paths := make(map[string]bool)
	for _, route := range application.Router.Routes() {
		paths[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		http.MethodGet + " /api/v1/openapi.json",
		http.MethodGet + " /api/v2/requests",
	} {
		if !paths[route] {
			t.Errorf("Engine application route %q is missing", route)
		}
	}
}

func TestBuildApplicationRestrictsCredentialedCORSToConfiguredOrigins(t *testing.T) {
	application, err := BuildApplication(ApplicationConfig{
		Common:  &site.CommonConfig{DBType: "mysql", AdminRole: "System Manager", SessionLifetimeHours: 1, APIDefaultLimit: 20, APIMaxLimit: 100},
		Tenants: knet.NewSiteRouter(nil), CORSAllowedOrigins: []string{"https://cloud.example.test"},
	})
	if err != nil {
		t.Fatalf("BuildApplication: %v", err)
	}
	defer application.SessionManager.Close()
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/openapi.json", nil)
	request.Header.Set("Origin", "https://cloud.example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	response := httptest.NewRecorder()
	application.Router.ServeHTTP(response, request)
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "https://cloud.example.test" {
		t.Fatalf("allowed origin header = %q", got)
	}
	if response.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("credentialed handoff origin was not allowed")
	}

	request = httptest.NewRequest(http.MethodOptions, "/api/v1/openapi.json", nil)
	request.Header.Set("Origin", "https://untrusted.example.test")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	response = httptest.NewRecorder()
	application.Router.ServeHTTP(response, request)
	if response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("untrusted origin was reflected: %q", response.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestRegisterConsoleRoutesKeepsAuthenticationBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &api.ConsoleHandler{PlatformWorkAdmission: routeWorkAdmissionFunc(func(context.Context) (func(), error) {
		return nil, errors.New("platform admission exhausted")
	})}
	if err := RegisterConsoleRoutes(router, handler); err != nil {
		t.Fatalf("RegisterConsoleRoutes: %v", err)
	}
	want := map[string]bool{
		http.MethodGet + " /api/console/sites": false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("console route %q was not registered", route)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/console/sites", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated console site list status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/console/sites/onboard", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("retired password bootstrap route status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestRegisterApplicationRoutesKeepsAuthAndVersionedAPIPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := doctype.NewRegistry()
	router := gin.New()
	manager, err := RegisterApplicationRoutes(ApplicationRouteConfig{
		Router: router, Tenants: knet.NewSiteRouter(nil), Registry: registry,
		Transactions:    &orm.TxManager{Registry: registry},
		SessionLifetime: time.Hour, AdminRole: "System Manager", APIDefaultLimit: 20,
		APIMaxLimit: 100, BinaryVersion: "test",
	})
	if err != nil {
		t.Fatalf("RegisterApplicationRoutes: %v", err)
	}
	if manager == nil {
		t.Fatal("application route composition returned no process session manager")
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("close route session manager: %v", err)
	}

	want := map[string]bool{
		http.MethodGet + " /api/v1/openapi.json":                           false,
		http.MethodGet + " /api/openapi.json":                              false,
		http.MethodGet + " /api/v2/requests":                               false,
		http.MethodGet + " /api/v2/modules/catalog":                        false,
		http.MethodPost + " /api/v2/modules/operations/:operation/execute": false,
		http.MethodPost + " /api/v2/module-executions/lookup":              false,
		http.MethodGet + " /api/v2/module-executions/:execution":           false,
		http.MethodGet + " /api/v2/offline/capabilities":                   false,
		http.MethodPost + " /api/v2/offline/intents":                       false,
		http.MethodPost + " /api/v2/offline/bootstrap":                     false,
		http.MethodPost + " /api/v2/offline/pull":                          false,
	}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Errorf("route %q was not registered", route)
		}
	}
}
