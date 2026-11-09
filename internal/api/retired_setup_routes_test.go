package api

import (
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/orm"
)

// The AI workspace-setup interview (tenant /cloud/conversations) was retired
// with its onboarding screen. Its routes must stay unregistered; Cloud owns the
// onboarding conversation on its own host.
func TestRetiredWorkspaceSetupConversationRoutesAreNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	for _, prefix := range []string{"/api/v1", "/api"} {
		RegisterRoutesOnGroup(router.Group(prefix), nil, &orm.TxManager{}, RouteOptions{})
	}
	for _, route := range router.Routes() {
		if strings.Contains(route.Path, "/cloud/conversations") {
			t.Errorf("retired route still registered: %s %s", route.Method, route.Path)
		}
	}
}

// A zero RouteOptions must register the same routes as a fully populated one:
// every dependency is optional and omitting it only drops the optional feature.
func TestRouteOptionsZeroValueRegistersFullRouteSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	routeSet := func(opts RouteOptions) map[string]bool {
		router := gin.New()
		RegisterRoutesOnGroup(router.Group("/api/v1"), nil, &orm.TxManager{}, opts)
		set := map[string]bool{}
		for _, route := range router.Routes() {
			set[route.Method+" "+route.Path] = true
		}
		return set
	}
	zero := routeSet(RouteOptions{})
	for _, want := range []string{"POST /api/v1/upload", "GET /api/v1/resource/:doctype", "GET /api/v1/analytics/status", "POST /api/v1/ai/chat"} {
		if !zero[want] {
			t.Errorf("zero RouteOptions did not register %s", want)
		}
	}
	tests := []struct {
		name string
		opts RouteOptions
	}{
		{"runtime services", RouteOptions{RuntimeServices: NewSiteRuntimeServices()}},
		{"analytics cache", RouteOptions{AnalyticsCache: NewAnalyticsQueryCache()}},
		{"allowlist and realtime owner", RouteOptions{HTTPAllowlist: []string{"example.com"}, RealtimeConnections: &RealtimeConnectionOwner{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := routeSet(tt.opts)
			if len(got) != len(zero) {
				t.Fatalf("registered %d routes, zero value registers %d", len(got), len(zero))
			}
			for route := range zero {
				if !got[route] {
					t.Errorf("missing route %s", route)
				}
			}
		})
	}
}
