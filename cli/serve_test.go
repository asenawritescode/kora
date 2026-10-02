package cli

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/asenawritescode/kora/doctype"
	knet "github.com/asenawritescode/kora/net"
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
	knet.RegisterPathSiteRoutes(router, siteRouter, nil)

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
