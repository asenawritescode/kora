package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

func TestRegisterPlatformRoutesPreservesHealthAndPingContracts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if err := RegisterPlatformRoutes(PlatformRouteConfig{Router: router, BinaryVersion: "test-build"}); err != nil {
		t.Fatalf("RegisterPlatformRoutes: %v", err)
	}
	registered := make(map[string]bool)
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{"GET /api/v1/ping", "GET /api/ping", "GET /health", "GET /readyz"} {
		if !registered[route] {
			t.Errorf("route %q was not registered", route)
		}
	}

	ping := httptest.NewRecorder()
	router.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/api/ping", nil))
	if ping.Code != http.StatusOK {
		t.Fatalf("ping status = %d, body %s", ping.Code, ping.Body.String())
	}
	var pingBody map[string]any
	if err := json.Unmarshal(ping.Body.Bytes(), &pingBody); err != nil {
		t.Fatalf("decode ping response: %v", err)
	}
	if pingBody["message"] != "pong" || pingBody["version"] != "test-build" {
		t.Fatalf("ping response = %#v", pingBody)
	}

	health := httptest.NewRecorder()
	router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	var healthBody map[string]any
	if err := json.Unmarshal(health.Body.Bytes(), &healthBody); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health.Code != http.StatusOK || healthBody["status"] != "degraded" || healthBody["db"] != "unknown" {
		t.Fatalf("health = (%d, %#v), want 200/degraded/unknown", health.Code, healthBody)
	}
	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", ready.Code)
	}
}

func TestRegisterPlatformRoutesRequiresRouter(t *testing.T) {
	if err := RegisterPlatformRoutes(PlatformRouteConfig{}); err == nil {
		t.Fatal("RegisterPlatformRoutes without router succeeded")
	}
}

// The shared integration token alone must not turn on (or break) the management
// API: an Engine that uses Cloud only for, say, the AI gateway has no deployment ID.
func TestManagementRoutesNeedTheTokenAndADeploymentID(t *testing.T) {
	tests := []struct {
		name       string
		token, id  string
		wantStatus int
	}{
		{name: "token without deployment ID stays off", token: "shared", wantStatus: http.StatusNotFound},
		{name: "token and deployment ID turn it on", token: "shared", id: "default", wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			if err := RegisterPlatformRoutes(PlatformRouteConfig{Router: router, Directory: managementTestDirectory{}, ManagementToken: tt.token, DeploymentID: tt.id}); err != nil {
				t.Fatalf("RegisterPlatformRoutes: %v", err)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/management/v2/tenants/tenant-a", nil))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
		})
	}
}

type managementTestDirectory struct{}

func (managementTestDirectory) GetDescriptorByIDContext(context.Context, string) (site.Descriptor, error) {
	return site.Descriptor{}, errors.New("not found")
}

func (managementTestDirectory) ResolveDescriptorByAliasContext(context.Context, string) (site.Descriptor, error) {
	return site.Descriptor{}, errors.New("not found")
}

func (managementTestDirectory) CurrentDirectoryRevisionContext(context.Context) (uint64, error) {
	return 0, nil
}
