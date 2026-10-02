package net

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/gin-gonic/gin"
)

// LoadedSite holds the runtime state for a single site.
type LoadedSite struct {
	SiteID         string
	ConfigRevision uint64
	Status         string
	DBType         string
	Name           string
	Config         SiteRouterConfig
	DB             *sql.DB
	Registry       *doctype.Registry

	// AnalyticsEventBus receives change events from the ORM layer.
	// nil if analytics is disabled for this site.
	AnalyticsEventBus analytics.EventBus

	// AnalyticsWorker processes change events into rollup tables.
	// nil if analytics is disabled for this site.
	AnalyticsWorker *analytics.Worker
	// RuntimeServices is an opaque immutable handler-service bundle. It is
	// copied into each request context with this runtime snapshot.
	RuntimeServices any
	requestMu       sync.Mutex
	requestRefs     int
	draining        bool
	requestsDrained chan struct{}
}

func (s *LoadedSite) acquireRequest() bool {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.draining {
		return false
	}
	s.requestRefs++
	return true
}

func (s *LoadedSite) releaseRequest() {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.requestRefs--
	if s.requestRefs == 0 && s.draining && s.requestsDrained != nil {
		close(s.requestsDrained)
		s.requestsDrained = nil
	}
}

func (s *LoadedSite) beginDrain() <-chan struct{} {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	s.draining = true
	if s.requestRefs == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if s.requestsDrained == nil {
		s.requestsDrained = make(chan struct{})
	}
	return s.requestsDrained
}

// WaitForRequests waits until all requests admitted against this runtime finish.
func (s *LoadedSite) WaitForRequests(ctx context.Context) error {
	if s == nil {
		return nil
	}
	ch := s.beginDrain()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AllSites returns all loaded sites (for console, path-based routing, etc.).
func (sr *SiteRouter) AllSites() []*LoadedSite {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return append([]*LoadedSite(nil), sr.allSites...)
}

// AddSite hot-adds a site to the running router without a restart.
// Used by the console when creating a new site via API.
func (sr *SiteRouter) AddSite(s *LoadedSite) error {
	_, err := sr.addOrReplaceSite(s, false)
	return err
}

// ReplaceSite atomically publishes a new runtime for an existing canonical
// site and marks the prior runtime draining so its DB can be closed only after
// requests admitted on the old snapshot have completed.
func (sr *SiteRouter) ReplaceSite(s *LoadedSite) (*LoadedSite, error) {
	return sr.addOrReplaceSite(s, true)
}

func (sr *SiteRouter) addOrReplaceSite(s *LoadedSite, drainExisting bool) (*LoadedSite, error) {
	if s == nil {
		return nil, fmt.Errorf("cannot add nil site")
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	existing := sr.siteByNameLocked(s.Name)
	if existing == nil && s.SiteID != "" && !sr.ambiguousIDs[s.SiteID] {
		existing = sr.byID[s.SiteID]
	}
	for _, alias := range siteAliases(s.Config) {
		normalized := normalizeSiteHost(alias)
		if normalized == "" {
			continue
		}
		for _, owner := range sr.allSites {
			if owner == existing {
				continue
			}
			for _, ownerAlias := range siteAliases(owner.Config) {
				if normalizeSiteHost(ownerAlias) == normalized {
					return nil, fmt.Errorf("site alias %q is already assigned to %q", normalized, owner.Name)
				}
			}
		}
	}
	if s.SiteID != "" {
		for _, owner := range sr.allSites {
			if owner != existing && owner.SiteID == s.SiteID {
				return nil, fmt.Errorf("canonical site id %q is already assigned to %q", s.SiteID, owner.Name)
			}
		}
	}
	var retired *LoadedSite
	if existing != nil {
		retired = existing
		if drainExisting {
			existing.beginDrain()
		}
		sr.removeSiteLocked(existing.Name)
	}
	sr.allSites = append(sr.allSites, s)
	sr.rebuildIndexesLocked()
	if sr.defaultSite == nil {
		sr.defaultSite = s
	}
	return retired, nil
}

// RemoveSiteByID removes a runtime from new routing decisions and returns its
// resources to the caller for graceful close. Existing database operations are
// allowed to finish by database/sql when DB.Close is called after removal.
func (sr *SiteRouter) RemoveSiteByID(siteID string) *LoadedSite {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	loaded := sr.byID[siteID]
	if loaded == nil || sr.ambiguousIDs[siteID] {
		return nil
	}
	loaded.beginDrain()
	kept := sr.allSites[:0]
	for _, candidate := range sr.allSites {
		if candidate != loaded {
			kept = append(kept, candidate)
		}
	}
	sr.allSites = kept
	if sr.defaultSite == loaded {
		sr.defaultSite = nil
		for _, candidate := range sr.allSites {
			if siteAcceptsTraffic(candidate) {
				sr.defaultSite = candidate
				break
			}
		}
	}
	sr.rebuildIndexesLocked()
	return loaded
}

func siteAliases(config SiteRouterConfig) []string {
	aliases := append([]string(nil), config.Domains...)
	aliases = append(aliases, config.Hostname)
	return aliases
}

// ValidateAliases refuses to start serving if eager-loaded site aliases are
// ambiguous. The router never lets ordering choose a tenant at runtime.
func (sr *SiteRouter) ValidateAliases() error {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	owners := make(map[string]string)
	ids := make(map[string]string)
	for _, loaded := range sr.allSites {
		if loaded.SiteID != "" {
			if owner, exists := ids[loaded.SiteID]; exists && owner != loaded.Name {
				return fmt.Errorf("canonical site id %q is assigned to both %q and %q", loaded.SiteID, owner, loaded.Name)
			}
			ids[loaded.SiteID] = loaded.Name
		}
		for _, alias := range siteAliases(loaded.Config) {
			alias = normalizeSiteHost(alias)
			if alias == "" {
				continue
			}
			if owner, exists := owners[alias]; exists && owner != loaded.Name {
				return fmt.Errorf("site alias %q is assigned to both %q and %q", alias, owner, loaded.Name)
			}
			owners[alias] = loaded.Name
		}
	}
	return nil
}

// UpdateSiteConfig updates the routing metadata for an already loaded site.
// It preserves the existing LoadedSite pointer so DB/registry handles remain valid.
func (sr *SiteRouter) UpdateSiteConfig(name string, cfg SiteRouterConfig) *LoadedSite {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	site := sr.siteByNameLocked(name)
	if site == nil {
		return nil
	}
	for _, alias := range siteAliases(cfg) {
		normalized := normalizeSiteHost(alias)
		for _, owner := range sr.allSites {
			if owner == site {
				continue
			}
			for _, ownerAlias := range siteAliases(owner.Config) {
				if normalized != "" && normalizeSiteHost(ownerAlias) == normalized {
					return nil
				}
			}
		}
	}
	site.Config = cfg
	sr.rebuildIndexesLocked()
	if sr.defaultSite == nil {
		sr.defaultSite = site
	}
	return site
}

// RemoveSite removes a site from the router by name. Returns the removed site
// or nil if not found. Updates the default site if the removed site was the default.
func (sr *SiteRouter) RemoveSite(name string) *LoadedSite {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.removeSiteLocked(name)
}

func (sr *SiteRouter) removeSiteLocked(name string) *LoadedSite {
	// Find the site in allSites.
	idx := -1
	for i, s := range sr.allSites {
		if s.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}
	site := sr.allSites[idx]
	// Remove from allSites slice.
	sr.allSites = append(sr.allSites[:idx], sr.allSites[idx+1:]...)
	sr.rebuildIndexesLocked()

	// Update default site if needed.
	if sr.defaultSite == site {
		if len(sr.allSites) > 0 {
			sr.defaultSite = sr.allSites[0]
		} else {
			sr.defaultSite = nil
		}
	}

	return site
}

// SiteByName returns a site by its name or short name.
// E.g., both "airtime.local" and "airtime" match the airtime site.
func (sr *SiteRouter) SiteByName(name string) *LoadedSite {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return sr.siteByNameLocked(name)
}

func (sr *SiteRouter) siteByNameLocked(name string) *LoadedSite {
	// Canonical IDs are stable path selectors for Cloud workers and generated
	// links. Keep hostname/short-name lookup for customer-facing compatibility.
	if sr.ambiguousIDs[name] {
		return nil
	}
	if site := sr.byID[name]; site != nil {
		return site
	}
	for _, s := range sr.allSites {
		if s.Name == name {
			return s
		}
	}
	for _, s := range sr.allSites {
		short := strings.TrimSuffix(s.Name, ".local")
		short = strings.TrimSuffix(short, ".com")
		short = strings.TrimSuffix(short, ".app")
		if short == name {
			return s
		}
	}
	return nil
}

// SiteRouterConfig is the minimal config needed for routing.
type SiteRouterConfig struct {
	Hostname      string
	Domains       []string
	FileStorage   string
	StorageBucket string
}

// SiteRouter maps Host headers to loaded sites.
type SiteRouter struct {
	mu               sync.RWMutex
	sites            map[string]*LoadedSite
	byID             map[string]*LoadedSite
	ambiguousAliases map[string]bool
	ambiguousIDs     map[string]bool
	allSites         []*LoadedSite
	defaultSite      *LoadedSite
}

// NewSiteRouter creates a site router from loaded sites.
func NewSiteRouter(sites []*LoadedSite) *SiteRouter {
	sr := &SiteRouter{
		sites:            make(map[string]*LoadedSite),
		byID:             make(map[string]*LoadedSite),
		ambiguousAliases: make(map[string]bool),
		ambiguousIDs:     make(map[string]bool),
		allSites:         sites,
	}
	sr.rebuildIndexesLocked()
	if len(sites) > 0 {
		sr.defaultSite = sites[0]
	}
	return sr
}

func (sr *SiteRouter) rebuildIndexesLocked() {
	sr.sites = make(map[string]*LoadedSite)
	sr.byID = make(map[string]*LoadedSite)
	sr.ambiguousAliases = make(map[string]bool)
	sr.ambiguousIDs = make(map[string]bool)
	for _, s := range sr.allSites {
		if s == nil {
			continue
		}
		if s.Status == "" {
			s.Status = "active"
		}
		if s.SiteID != "" {
			if prev := sr.byID[s.SiteID]; prev != nil && prev != s {
				delete(sr.byID, s.SiteID)
				sr.ambiguousIDs[s.SiteID] = true
			} else if !sr.ambiguousIDs[s.SiteID] {
				sr.byID[s.SiteID] = s
			}
		}
		for _, alias := range siteAliases(s.Config) {
			alias = normalizeSiteHost(alias)
			if alias == "" {
				continue
			}
			if prev := sr.sites[alias]; prev != nil && prev != s {
				delete(sr.sites, alias)
				sr.ambiguousAliases[alias] = true
			} else if !sr.ambiguousAliases[alias] {
				sr.sites[alias] = s
			}
		}
	}
}

// SiteByID resolves a canonical site identity from the eagerly loaded local
// runtime snapshot. It performs no database or control-plane request.
func (sr *SiteRouter) SiteByID(siteID string) *LoadedSite {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	if sr.ambiguousIDs[siteID] {
		return nil
	}
	return sr.byID[siteID]
}

// ApplySiteStatus applies a revisioned directory update to the local eager
// runtime. It is idempotent and refuses stale updates after a newer revision.
func (sr *SiteRouter) ApplySiteStatus(siteID, status string, revision uint64) error {
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return fmt.Errorf("unsupported site status %q", status)
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	site := sr.byID[siteID]
	if site == nil || sr.ambiguousIDs[siteID] {
		return fmt.Errorf("site id %q is not loaded", siteID)
	}
	if revision < site.ConfigRevision || (revision == site.ConfigRevision && status != site.Status) {
		return fmt.Errorf("stale or conflicting site revision %d; current revision is %d", revision, site.ConfigRevision)
	}
	site.Status = status
	site.ConfigRevision = revision
	return nil
}

// ApplySiteDescriptor applies the credential-free routing projection from the
// directory feed. It refreshes aliases and status in-place, preserving the
// tenant DB pool, schema registry, analytics worker, and other live handles.
func (sr *SiteRouter) ApplySiteDescriptor(descriptor contract.SiteDescriptor) error {
	if strings.TrimSpace(descriptor.SiteID) == "" {
		return fmt.Errorf("site descriptor id is required")
	}
	status := strings.ToLower(strings.TrimSpace(descriptor.Status))
	switch status {
	case "provisioning", "active", "degraded", "suspended", "deleting", "deleted":
	default:
		return fmt.Errorf("unsupported site status %q", status)
	}
	if len(descriptor.Aliases) == 0 {
		return fmt.Errorf("site descriptor aliases are required")
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	loaded := sr.byID[descriptor.SiteID]
	if loaded == nil || sr.ambiguousIDs[descriptor.SiteID] {
		return fmt.Errorf("site id %q is not loaded", descriptor.SiteID)
	}
	if descriptor.ConfigRevision < loaded.ConfigRevision || (descriptor.ConfigRevision == loaded.ConfigRevision && status != loaded.Status) {
		return fmt.Errorf("stale or conflicting site revision %d; current revision is %d", descriptor.ConfigRevision, loaded.ConfigRevision)
	}
	domains := make([]string, 0, len(descriptor.Aliases))
	seen := make(map[string]struct{}, len(descriptor.Aliases))
	for _, alias := range descriptor.Aliases {
		alias = normalizeSiteHost(alias)
		if alias == "" || alias == normalizeSiteHost(loaded.Name) {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		for _, owner := range sr.allSites {
			if owner == loaded {
				continue
			}
			for _, ownerAlias := range siteAliases(owner.Config) {
				if normalizeSiteHost(ownerAlias) == alias {
					return fmt.Errorf("site alias %q is already assigned to %q", alias, owner.Name)
				}
			}
		}
		domains = append(domains, alias)
	}
	loaded.Config.Domains = domains
	loaded.Status = status
	loaded.ConfigRevision = descriptor.ConfigRevision
	sr.rebuildIndexesLocked()
	return nil
}

func siteAcceptsTraffic(site *LoadedSite) bool {
	return site != nil && (site.Status == "" || site.Status == "active" || site.Status == "degraded")
}

func normalizeSiteHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

type pathRoutedSiteContextKey struct{}

func injectSiteRuntimeContext(c *gin.Context, site *LoadedSite) {
	c.Set("site_name", site.Name)
	c.Set("site_id", site.SiteID)
	c.Set("site_status", site.Status)
	c.Set("site_config_revision", site.ConfigRevision)
	c.Set("site_db_type", site.DBType)
	c.Set("site_db", site.DB)
	c.Set("site_registry", site.Registry)
	c.Set("site_analytics_worker", site.AnalyticsWorker)
	c.Set("site_runtime_services", site.RuntimeServices)
}

// Middleware returns a Gin middleware that resolves the Host header to a site.
func (sr *SiteRouter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Path-based routing has already resolved and pinned the runtime before
		// Gin re-dispatches a rewritten /s/<id>/api request. The typed context key
		// cannot be supplied by a client and deliberately bypasses Host checks for
		// the shared-ingress hostname while restoring tenant context.
		if routedSite, ok := c.Request.Context().Value(pathRoutedSiteContextKey{}).(*LoadedSite); ok && routedSite != nil {
			injectSiteRuntimeContext(c, routedSite)
			c.Next()
			return
		}

		// Skip site resolution for console, health, and other system paths.
		path := c.Request.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/_kora/") || strings.HasPrefix(path, "/api/console") || strings.HasPrefix(path, "/console") || strings.HasPrefix(path, "/assets/") || path == "/api/ping" || strings.HasPrefix(path, "/s/") {
			c.Next()
			return
		}

		// If already set by path-based routing, skip.
		if _, exists := c.Get("site_db"); exists {
			c.Next()
			return
		}

		host := normalizeSiteHost(stripPort(c.Request.Host))
		sr.mu.RLock()
		if sr.ambiguousAliases[host] {
			sr.mu.RUnlock()
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": "site_alias_conflict", "message": "Host maps to more than one site"})
			return
		}

		// Check site cookie (set by /s/:site/... redirect).
		// Validate that the request Host is trusted when using this cookie to prevent
		// cross-site access via cookie injection on shared networks.
		if siteName, _ := c.Cookie("kora_site"); siteName != "" {
			if s := sr.siteByNameLocked(siteName); s != nil {
				if sr.isHostAllowedForSite(host, s) {
					if !siteAcceptsTraffic(s) {
						sr.mu.RUnlock()
						c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
						return
					}
					if !s.acquireRequest() {
						sr.mu.RUnlock()
						c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
						return
					}
					injectSiteRuntimeContext(c, s)
					sr.mu.RUnlock()
					defer s.releaseRequest()
					c.Next()
					return
				}
				sr.mu.RUnlock()
				c.AbortWithStatusJSON(403, gin.H{
					"error":   "site_access_denied",
					"message": fmt.Sprintf("Site %q cannot be accessed via host %q", siteName, host),
				})
				return
			}
		}

		site, ok := sr.sites[host]
		if !ok {
			if net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1" {
				site = sr.defaultSite
			}
		}
		acceptsTraffic := siteAcceptsTraffic(site)
		acquired := site != nil && acceptsTraffic && site.acquireRequest()
		sr.mu.RUnlock()
		if site != nil && !acceptsTraffic {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
			return
		}

		if site == nil {
			c.AbortWithStatusJSON(404, gin.H{
				"error":   "site_not_found",
				"message": fmt.Sprintf("No site configured for host: %s", host),
			})
			return
		}
		if !acquired {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
			return
		}

		injectSiteRuntimeContext(c, site)

		// Set kora_site cookie so the frontend can read the site name.
		// Only set if not already present or value differs (avoid redundant Set-Cookie).
		if existing, _ := c.Cookie("kora_site"); existing != site.Name {
			SetSecureCookie(c, "kora_site", site.Name, 86400, "/", false)
		}

		defer site.releaseRequest()
		c.Next()
	}
}

// isHostAllowedForSite checks if the given host is allowed to access a site via the kora_site cookie.
// Allowed: localhost, loopback IPs, any parsed IP (dev/testing), the KORA_HOST env var, or a domain configured for the site.
func (sr *SiteRouter) isHostAllowedForSite(host string, site *LoadedSite) bool {
	if net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1" {
		return true
	}
	// Allow the configured app host (set via KORA_HOST env var).
	if appHost := os.Getenv("KORA_HOST"); appHost != "" && normalizeSiteHost(appHost) == host {
		return true
	}
	for _, d := range site.Config.Domains {
		if normalizeSiteHost(d) == host {
			return true
		}
	}
	return false
}

// AllDomains returns every domain across all sites (for autocert).
func (sr *SiteRouter) AllDomains() []string {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	var domains []string
	seen := make(map[string]bool)
	for _, s := range sr.allSites {
		for _, d := range s.Config.Domains {
			d = normalizeSiteHost(d)
			if !seen[d] {
				seen[d] = true
				domains = append(domains, d)
			}
		}
	}
	return domains
}

// RegisterPathSiteRoutes adds a NoRoute handler that intercepts /s/:site/*
// requests, injects site context, rewrites the path, and serves directly.
// This allows multi-site access without DNS or /etc/hosts.
//
//	localhost:8000/s/airtime/workspace → airtime site
//	localhost:8000/s/fieldwork/api/...  → fieldwork site
func RegisterPathSiteRoutes(router *gin.Engine, sr *SiteRouter, spaFS fs.FS) {
	router.NoRoute(func(c *gin.Context) {
		slog.Debug("path-based site route", "path", c.Request.URL.Path, "method", c.Request.Method)
		path := c.Request.URL.Path

		// Not a path-based site URL — return 404.
		if !strings.HasPrefix(path, "/s/") {
			c.JSON(404, gin.H{"error": "not_found"})
			return
		}

		// Parse /s/:site/...
		rest := strings.TrimPrefix(path, "/s/")
		slashIdx := strings.Index(rest, "/")
		var siteName string
		if slashIdx >= 0 {
			siteName = rest[:slashIdx]
			rest = rest[slashIdx:]
		} else {
			siteName = rest
			rest = "/"
		}

		sr.mu.RLock()
		site := sr.siteByNameLocked(siteName)
		acceptsTraffic := siteAcceptsTraffic(site)
		acquired := site != nil && acceptsTraffic && site.acquireRequest()
		sr.mu.RUnlock()
		if site == nil {
			c.JSON(404, gin.H{"error": "site_not_found", "message": "No site: " + siteName})
			return
		}
		if !acceptsTraffic {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site_unavailable", "message": "Site is not accepting traffic"})
			return
		}
		if !acquired {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "site_draining", "message": "Site runtime is being replaced"})
			return
		}
		defer site.releaseRequest()

		// Inject site context.
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), pathRoutedSiteContextKey{}, site))
		injectSiteRuntimeContext(c, site)

		// Handle API and workspace paths.
		if strings.HasPrefix(rest, "/api/") || rest == "/api" {
			// API requests: rewrite path and let Gin re-dispatch.
			// We need to re-enter the router for API routes to match.
			c.Request.AddCookie(&http.Cookie{Name: "kora_site", Value: site.Name, Path: "/"})
			c.Request.URL.Path = rest
			// Buffer the request body for re-dispatch — HandleContext
			// re-processes the middleware chain, so we need a fresh body reader.
			if c.Request.Body != nil && c.Request.ContentLength > 0 {
				bodyBytes, err := io.ReadAll(c.Request.Body)
				if err == nil {
					c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					slog.Info("body buffered for re-dispatch", "path", rest, "size", len(bodyBytes))
				} else {
					slog.Warn("body buffer read failed", "path", rest, "error", err)
				}
			} else if c.Request.ContentLength > 0 && c.Request.Body == nil {
				slog.Warn("body not buffered", "path", rest, "bodyNil", c.Request.Body == nil, "contentLength", c.Request.ContentLength)
			}
			// Manually call HandleContext — but this time site context is already set
			// and the SiteRouter middleware will skip (checks for existing site_db).
			router.HandleContext(c)
			return
		}

		if rest == "/workspace" || strings.HasPrefix(rest, "/workspace/") || rest == "/" {
			// Serve SPA directly at the /s/:site/workspace URL.
			// Set a site cookie so API calls know which site to use.
			SetSecureCookie(c, "kora_site", site.Name, 86400, "/", false)
			// Serve index.html from the SPA FS.
			if spaFS != nil {
				serveSPAFromFS(c, spaFS, "/index.html")
			} else {
				c.String(http.StatusNotFound, "SPA not available")
			}
			return
		}

		// Anything else under the site (assets, etc): serve from SPA FS or 404.
		if spaFS != nil {
			serveSPAFromFS(c, spaFS, rest)
			return
		}

		c.JSON(404, gin.H{"error": "not_found"})
	})
}

// serveSPAFromFS serves a file from the SPA's embedded filesystem.
// Falls back to index.html for client-side routing.
func serveSPAFromFS(c *gin.Context, spaFS fs.FS, reqPath string) {
	servePath := reqPath
	if servePath == "" || servePath == "/" {
		servePath = "/index.html"
	}

	cleanPath := strings.TrimPrefix(servePath, "/")
	f, err := spaFS.Open(cleanPath)
	if err != nil {
		servePath = "/index.html"
		cleanPath = "index.html"
	} else {
		f.Close()
	}

	data, err := fs.ReadFile(spaFS, cleanPath)
	if err != nil {
		c.String(http.StatusNotFound, "Not found")
		return
	}

	// Set content type.
	ct := "text/html; charset=utf-8"
	if strings.HasSuffix(cleanPath, ".js") {
		ct = "text/javascript; charset=utf-8"
	} else if strings.HasSuffix(cleanPath, ".css") {
		ct = "text/css; charset=utf-8"
	} else if strings.HasSuffix(cleanPath, ".svg") {
		ct = "image/svg+xml"
	}
	c.Header("Content-Type", ct)
	c.String(http.StatusOK, "%s", string(data))
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
