package net

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/doctype"
)

// testSite builds a minimal LoadedSite with the given name and domains.
func testSite(name string, domains ...string) *LoadedSite {
	cfg := SiteRouterConfig{Hostname: name}
	if len(domains) > 0 {
		cfg.Domains = domains
	}
	return &LoadedSite{
		Name:     name,
		Config:   cfg,
		DB:       nil, // not used in routing tests
		Registry: doctype.NewRegistry(),
	}
}

func benchmarkSites(count int) []*LoadedSite {
	sites := make([]*LoadedSite, count)
	for index := range sites {
		name := fmt.Sprintf("site-%05d.example.test", index)
		sites[index] = &LoadedSite{SiteID: fmt.Sprintf("site_%032x", index+1), Name: name, Config: SiteRouterConfig{Hostname: name}}
	}
	return sites
}

func BenchmarkSiteRouter10K(b *testing.B) {
	for _, count := range []int{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("sites-%d/build-eager-index", count), func(b *testing.B) {
			sites := benchmarkSites(count)
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				_ = NewSiteRouter(sites)
			}
		})
		b.Run(fmt.Sprintf("sites-%d/lookup-by-id", count), func(b *testing.B) {
			sites := benchmarkSites(count)
			router := NewSiteRouter(sites)
			b.ReportAllocs()
			id := sites[len(sites)-1].SiteID
			for index := 0; index < b.N; index++ {
				if router.SiteByID(id) == nil {
					b.Fatal("site missing")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SiteRouter — Host-based resolution
// ---------------------------------------------------------------------------

func TestSiteRouter_ResolveByHost(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com"),
		testSite("admin.example.com"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) {
		name, _ := c.Get("site_name")
		c.String(http.StatusOK, name.(string))
	})

	tests := []struct {
		name   string
		host   string
		want   string
		status int
	}{
		{"first host", "app.example.com", "app.example.com", http.StatusOK},
		{"second host", "admin.example.com", "admin.example.com", http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/test", nil)
			req.Host = tt.host
			r.ServeHTTP(w, req)

			if w.Code != tt.status {
				t.Errorf("status = %d, want %d; body=%s", w.Code, tt.status, w.Body.String())
			}
			if tt.status == http.StatusOK && w.Body.String() != tt.want {
				t.Errorf("body = %q, want %q", w.Body.String(), tt.want)
			}
		})
	}
}

func TestSiteRouterMiddlewareCarriesPerSiteDatabaseType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	loaded := testSite("tenant.example.test")
	loaded.DBType = "postgres"
	router := gin.New()
	router.Use(NewSiteRouter([]*LoadedSite{loaded}).Middleware())
	router.GET("/db-type", func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString("site_db_type"))
	})
	request := httptest.NewRequest(http.MethodGet, "/db-type", nil)
	request.Host = loaded.Name
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "postgres" {
		t.Fatalf("site DB type context = %d %q, want 200 postgres", response.Code, response.Body.String())
	}
}

func TestSiteRouterMiddlewarePublishesLoadedSiteRevision(t *testing.T) {
	site := testSite("revision.example.com")
	site.SiteID = "site_revision"
	site.ConfigRevision = 17
	router := NewSiteRouter([]*LoadedSite{site})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(router.Middleware())
	r.GET("/revision", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"site_id": c.GetString("site_id"), "config_revision": c.GetUint64("site_config_revision")})
	})
	request := httptest.NewRequest(http.MethodGet, "/revision", nil)
	request.Host = "revision.example.com"
	response := httptest.NewRecorder()
	r.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"config_revision":17,"site_id":"site_revision"}` {
		t.Fatalf("expected loaded site identity and revision in request context, got status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSiteRouterResolveByCanonicalIDAndRemove(t *testing.T) {
	site := testSite("id.example.com")
	site.SiteID = "site_0123456789abcdef0123456789abcdef"
	router := NewSiteRouter([]*LoadedSite{site})
	if got := router.SiteByID(site.SiteID); got != site {
		t.Fatalf("SiteByID returned %p, want %p", got, site)
	}
	if removed := router.RemoveSite(site.Name); removed != site {
		t.Fatalf("RemoveSite returned %p, want %p", removed, site)
	}
	if got := router.SiteByID(site.SiteID); got != nil {
		t.Fatalf("SiteByID after removal = %p, want nil", got)
	}
}

func TestSiteRouterAppliesRevisionedStatusBeforeServingTraffic(t *testing.T) {
	site := testSite("site-status.example")
	site.SiteID = "site-status-id"
	site.ConfigRevision = 4
	router := NewSiteRouter([]*LoadedSite{site})
	if err := router.ApplySiteStatus(site.SiteID, "suspended", 5); err != nil {
		t.Fatal(err)
	}
	if err := router.ApplySiteStatus(site.SiteID, "active", 4); err == nil {
		t.Fatal("stale revision was accepted")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(router.Middleware())
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "site-status.example"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestSiteRouterAppliesDirectoryDescriptorInPlace(t *testing.T) {
	loaded := testSite("descriptor.example")
	loaded.SiteID = "site-descriptor-id"
	loaded.ConfigRevision = 1
	router := NewSiteRouter([]*LoadedSite{loaded})
	descriptor := contract.SiteDescriptor{Version: contract.SiteDirectoryVersion, SiteID: loaded.SiteID, Aliases: []string{"descriptor.example", "new-alias.example"}, Status: "suspended", ConfigRevision: 2}
	if err := router.ApplySiteDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	if router.SiteByID(loaded.SiteID) != loaded || loaded.ConfigRevision != 2 || loaded.Status != "suspended" {
		t.Fatalf("directory update replaced runtime or failed to apply: %+v", loaded)
	}
	if router.sites["new-alias.example"] != loaded || router.sites["descriptor.example"] != loaded {
		t.Fatal("directory aliases were not reindexed")
	}
	if err := router.ApplySiteDescriptor(descriptor); err != nil {
		t.Fatalf("identical replay should be idempotent: %v", err)
	}
	descriptor.ConfigRevision = 1
	if err := router.ApplySiteDescriptor(descriptor); err == nil {
		t.Fatal("stale directory revision was accepted")
	}
}

func TestSiteRouter_LocalhostDefaultsToFirst(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com"),
		testSite("backup.example.com"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) {
		name, _ := c.Get("site_name")
		c.String(http.StatusOK, name.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "localhost"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != "app.example.com" {
		t.Errorf("body = %q, want %q", w.Body.String(), "app.example.com")
	}
}

func TestSiteRouter_UnknownHostReturns404(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) {
		name, _ := c.Get("site_name")
		c.String(http.StatusOK, name.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "unknown.example.com"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SiteRouter — Path-based routing
// ---------------------------------------------------------------------------

func TestSiteRouter_PathBasedRouting(t *testing.T) {
	sites := []*LoadedSite{
		testSite("airtime.local"),
		testSite("fieldwork.local"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()

	// Register path-based site routes (no-route handler).
	RegisterPathSiteRoutes(r, sr, nil)

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"known site", "/s/airtime.local/workspace", http.StatusNotFound}, // nil spaFS → "SPA not available" but we get status… wait
		{"unknown site", "/s/unknown/workspace", http.StatusNotFound},
		// Path-based with API → re-dispatch but no api routes registered → 404
		{"known site API", "/s/airtime.local/api/resource/Test", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", tt.path, nil)
			r.ServeHTTP(w, req)

			if w.Code != tt.status {
				t.Errorf("%s: status = %d, want %d; body=%s", tt.name, w.Code, tt.status, w.Body.String())
			}
		})
	}
}

func TestSiteRouter_PathBasedRoutingAcceptsCanonicalSiteID(t *testing.T) {
	loaded := testSite("tenant.example.test")
	loaded.SiteID = "site_engine_0123456789abcdef"
	sr := NewSiteRouter([]*LoadedSite{loaded})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/api/v1/ping", func(c *gin.Context) {
		siteID, exists := c.Get("site_id")
		if !exists {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.String(http.StatusOK, siteID.(string))
	})
	RegisterPathSiteRoutes(r, sr, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/site_engine_0123456789abcdef/api/v1/ping", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != loaded.SiteID {
		t.Fatalf("canonical site ID path response = %d %q, want 200 %q", w.Code, w.Body.String(), loaded.SiteID)
	}
}

func TestSiteRouter_PathBasedRouting_UnknownSite(t *testing.T) {
	sites := []*LoadedSite{
		testSite("known.local"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterPathSiteRoutes(r, sr, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/s/nonexistent/workspace", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusNotFound, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Middleware — SecurityHeaders
// ---------------------------------------------------------------------------

func TestSecurityHeaders_Present(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(SecurityHeadersMiddleware(false))
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	for _, h := range []string{"X-Frame-Options", "X-Content-Type-Options", "X-Xss-Protection"} {
		if w.Header().Get(h) == "" {
			t.Errorf("missing security header: %s", h)
		}
	}
}

func TestSecurityHeaders_StsOnlyWithTls(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Without TLS: STS should not be set.
	r1 := gin.New()
	r1.Use(SecurityHeadersMiddleware(false))
	r1.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	w1 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r1.ServeHTTP(w1, req)

	if v := w1.Header().Get("Strict-Transport-Security"); v != "" {
		t.Errorf("STS should be empty without TLS, got %q", v)
	}

	// With TLS: STS should be set.
	r2 := gin.New()
	r2.Use(SecurityHeadersMiddleware(true))
	r2.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/test", nil)
	req2.TLS = &tls.ConnectionState{} // minimal non-nil TLS
	r2.ServeHTTP(w2, req2)

	if v := w2.Header().Get("Strict-Transport-Security"); v == "" {
		t.Error("STS should be set when TLS is enabled")
	}
}

// ---------------------------------------------------------------------------
// Middleware — RequestID
// ---------------------------------------------------------------------------

func TestRequestID_Generated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/test", func(c *gin.Context) {
		id, _ := c.Get("request_id")
		c.String(http.StatusOK, id.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	id := w.Body.String()
	if len(id) == 0 {
		t.Fatal("request_id should not be empty")
	}
	// ULID is 26 chars.
	if len(id) != 26 {
		t.Errorf("request_id length = %d, want 26", len(id))
	}
}

func TestRequestID_PreservesExisting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(RequestIDMiddleware())
	r.GET("/test", func(c *gin.Context) {
		id, _ := c.Get("request_id")
		c.String(http.StatusOK, id.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-Id", "client-provided-id")
	r.ServeHTTP(w, req)

	if w.Body.String() != "client-provided-id" {
		t.Errorf("body = %q, want %q", w.Body.String(), "client-provided-id")
	}
	if w.Header().Get("X-Request-Id") != "client-provided-id" {
		t.Errorf("X-Request-Id header = %q, want %q", w.Header().Get("X-Request-Id"), "client-provided-id")
	}
}

// ---------------------------------------------------------------------------
// Middleware — CORS
// ---------------------------------------------------------------------------

func TestCORSMiddleware_AllowOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CORSMiddleware([]string{"https://trusted.example.com"}))
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "https://trusted.example.com")
	r.ServeHTTP(w, req)

	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "https://trusted.example.com" {
		t.Errorf("Allow-Origin = %q, want %q", v, "https://trusted.example.com")
	}
}

func TestCORSMiddleware_DevModeReflectsOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(CORSMiddleware(nil)) // empty → dev mode
	r.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	r.ServeHTTP(w, req)

	if v := w.Header().Get("Access-Control-Allow-Origin"); v != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want %q", v, "http://localhost:5173")
	}
}

// ---------------------------------------------------------------------------
// SiteRouter — domain aliases and path skipping
// ---------------------------------------------------------------------------

func TestSiteRouter_ResolvesDomainAlias(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com", "app.example.com", "alias.example.com"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) {
		name, _ := c.Get("site_name")
		c.String(http.StatusOK, name.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "alias.example.com"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != "app.example.com" {
		t.Errorf("body = %q, want %q", w.Body.String(), "app.example.com")
	}
}

func TestSiteRouter_SkipsHealthAndSystemPaths(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com"),
	}
	sr := NewSiteRouter(sites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "healthy")
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	req.Host = "unknown.example.com" // would 404 if not skipped
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SiteRouter — collection methods
// ---------------------------------------------------------------------------

func TestSiteRouter_AllSites(t *testing.T) {
	sites := []*LoadedSite{
		testSite("site-a"),
		testSite("site-b"),
	}
	sr := NewSiteRouter(sites)

	all := sr.AllSites()
	if len(all) != 2 {
		t.Fatalf("AllSites len = %d, want 2", len(all))
	}
	if all[0].Name != "site-a" {
		t.Errorf("all[0].Name = %q, want %q", all[0].Name, "site-a")
	}
}

func TestSiteRouter_SiteByName(t *testing.T) {
	sites := []*LoadedSite{
		testSite("app.example.com"),
	}
	sr := NewSiteRouter(sites)

	s := sr.SiteByName("app.example.com")
	if s == nil {
		t.Fatal("SiteByName returned nil for exact match")
	}
	if s.Name != "app.example.com" {
		t.Errorf("Name = %q, want %q", s.Name, "app.example.com")
	}

	// Short name match (remove .com suffix).
	s2 := sr.SiteByName("app.example")
	if s2 == nil {
		t.Fatal("SiteByName returned nil for short name 'app.example'")
	}

	// Unknown name.
	s3 := sr.SiteByName("unknown")
	if s3 != nil {
		t.Errorf("SiteByName should return nil for unknown, got %v", s3)
	}
}

func TestSiteRouter_AllDomains(t *testing.T) {
	sites := []*LoadedSite{
		testSite("site-a.com", "site-a.com", "alias-a.com"),
		testSite("site-b.com", "site-b.com"),
	}
	sr := NewSiteRouter(sites)

	domains := sr.AllDomains()
	if len(domains) != 3 {
		t.Fatalf("AllDomains len = %d, want 3; got %v", len(domains), domains)
	}
}

func TestSiteRouter_AddSite(t *testing.T) {
	sr := NewSiteRouter(nil)

	s := testSite("new-site.com")
	sr.AddSite(s)

	if len(sr.AllSites()) != 1 {
		t.Fatalf("AllSites len = %d, want 1", len(sr.AllSites()))
	}

	// Should resolve by host.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) {
		name, _ := c.Get("site_name")
		c.String(http.StatusOK, name.(string))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "new-site.com"
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if w.Body.String() != "new-site.com" {
		t.Errorf("body = %q, want %q", w.Body.String(), "new-site.com")
	}
}

func TestSiteRouter_AddSiteReplacesExistingSite(t *testing.T) {
	sr := NewSiteRouter(nil)

	first := testSite("app.example.com")
	second := testSite("app.example.com", "app.example.com", "alias.example.com")

	sr.AddSite(first)
	sr.AddSite(second)

	sites := sr.AllSites()
	if len(sites) != 1 {
		t.Fatalf("AllSites len = %d, want 1", len(sites))
	}
	if sites[0] != second {
		t.Fatalf("AllSites[0] = %#v, want %#v", sites[0], second)
	}
	if got := sr.SiteByName("app.example.com"); got != second {
		t.Fatalf("SiteByName() = %#v, want %#v", got, second)
	}
}

func TestSiteRouter_RejectsAliasConflictAndNormalizesHosts(t *testing.T) {
	first := testSite("one.example", "shared.example.")
	second := testSite("two.example", "shared.example")
	sr := NewSiteRouter([]*LoadedSite{first})
	if err := sr.AddSite(second); err == nil {
		t.Fatal("AddSite accepted a normalized alias collision")
	}
	if got := sr.SiteByID("missing"); got != nil {
		t.Fatalf("unexpected site: %v", got)
	}
	if got := sr.AllSites(); len(got) != 1 || got[0] != first {
		t.Fatalf("failed add changed router: %#v", got)
	}
}

func TestSiteRouter_AmbiguousStartupAliasNeverRoutesArbitrarily(t *testing.T) {
	first := testSite("one.example", "shared.example")
	second := testSite("two.example", "shared.example.")
	sr := NewSiteRouter([]*LoadedSite{first, second})
	if err := sr.ValidateAliases(); err == nil {
		t.Fatal("expected startup alias diagnostic")
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(sr.Middleware())
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Host = "shared.example"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d", w.Code, http.StatusConflict)
	}
}

func TestReplaceSiteDrainsRequestsAgainstRetiredRuntime(t *testing.T) {
	old := &LoadedSite{SiteID: "site-refresh", ConfigRevision: 1, Status: "active", Name: "refresh.example", Config: SiteRouterConfig{Hostname: "refresh.example"}}
	router := NewSiteRouter([]*LoadedSite{old})
	if !old.acquireRequest() {
		t.Fatal("failed to acquire request on current runtime")
	}
	next := &LoadedSite{SiteID: old.SiteID, ConfigRevision: 2, Status: "active", Name: old.Name, Config: SiteRouterConfig{Hostname: old.Name, Domains: []string{"new.refresh.example"}}}
	retired, err := router.ReplaceSite(next)
	if err != nil {
		t.Fatal("replace runtime:", err)
	}
	if retired != old || router.SiteByID(old.SiteID) != next {
		t.Fatal("replacement did not atomically publish the new runtime")
	}
	if old.acquireRequest() {
		old.releaseRequest()
		t.Fatal("retired runtime accepted a new request")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := old.WaitForRequests(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForRequests before release = %v, want deadline", err)
	}
	old.releaseRequest()
	if err := old.WaitForRequests(context.Background()); err != nil {
		t.Fatalf("WaitForRequests after release: %v", err)
	}
}
