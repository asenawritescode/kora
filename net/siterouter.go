package net

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/asenawritescode/kora/doctype"
)

// LoadedSite holds the runtime state for a single site.
type LoadedSite struct {
	Name     string
	Config   SiteRouterConfig
	DB       *sql.DB
	Registry *doctype.Registry
}

// AllSites returns all loaded sites (for console, path-based routing, etc.).
func (sr *SiteRouter) AllSites() []*LoadedSite {
	return sr.allSites
}

// AddSite hot-adds a site to the running router without a restart.
// Used by the console when creating a new site via API.
func (sr *SiteRouter) AddSite(s *LoadedSite) {
	domains := s.Config.Domains
	if len(domains) == 0 {
		domains = []string{s.Config.Hostname}
	}
	for _, d := range domains {
		sr.sites[strings.ToLower(d)] = s
	}
	sr.allSites = append(sr.allSites, s)
	if sr.defaultSite == nil {
		sr.defaultSite = s
	}
}

// SiteByName returns a site by its name or short name.
// E.g., both "airtime.local" and "airtime" match the airtime site.
func (sr *SiteRouter) SiteByName(name string) *LoadedSite {
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
	Hostname string
	Domains  []string
}

// SiteRouter maps Host headers to loaded sites.
type SiteRouter struct {
	sites       map[string]*LoadedSite
	allSites    []*LoadedSite
	defaultSite *LoadedSite
}

// NewSiteRouter creates a site router from loaded sites.
func NewSiteRouter(sites []*LoadedSite) *SiteRouter {
	sr := &SiteRouter{
		sites:    make(map[string]*LoadedSite),
		allSites: sites,
	}
	for _, s := range sites {
		domains := s.Config.Domains
		if len(domains) == 0 {
			domains = []string{s.Config.Hostname}
		}
		for _, d := range domains {
			sr.sites[strings.ToLower(d)] = s
		}
	}
	if len(sites) > 0 {
		sr.defaultSite = sites[0]
	}
	return sr
}

// Middleware returns a Gin middleware that resolves the Host header to a site.
func (sr *SiteRouter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip site resolution for console, health, and other system paths.
		path := c.Request.URL.Path
		if path == "/health" || strings.HasPrefix(path, "/api/console") || strings.HasPrefix(path, "/console") || path == "/api/ping" {
			c.Next()
			return
		}

		// If already set by path-based routing, skip.
		if _, exists := c.Get("site_db"); exists {
			c.Next()
			return
		}

		host := stripPort(strings.ToLower(c.Request.Host))

		// Check site cookie (set by /s/:site/... redirect).
		// Validate that the request Host is trusted when using this cookie to prevent
		// cross-site access via cookie injection on shared networks.
		if siteName, _ := c.Cookie("kora_site"); siteName != "" {
			if s := sr.SiteByName(siteName); s != nil {
				if sr.isHostAllowedForSite(host, s) {
					c.Set("site_name", s.Name)
					c.Set("site_db", s.DB)
					c.Set("site_registry", s.Registry)
					c.Next()
					return
				}
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

		if site == nil {
			c.AbortWithStatusJSON(404, gin.H{
				"error":   "site_not_found",
				"message": fmt.Sprintf("No site configured for host: %s", host),
			})
			return
		}

		c.Set("site_name", site.Name)
		c.Set("site_db", site.DB)
		c.Set("site_registry", site.Registry)
		c.Next()
	}
}

// isHostAllowedForSite checks if the given host is allowed to access a site via the kora_site cookie.
// Allowed: localhost, loopback IPs, any parsed IP (dev/testing), or a domain configured for the site.
func (sr *SiteRouter) isHostAllowedForSite(host string, site *LoadedSite) bool {
	if net.ParseIP(host) != nil || host == "localhost" || host == "127.0.0.1" {
		return true
	}
	for _, d := range site.Config.Domains {
		if strings.EqualFold(d, host) {
			return true
		}
	}
	return false
}

// AllDomains returns every domain across all sites (for autocert).
func (sr *SiteRouter) AllDomains() []string {
	var domains []string
	seen := make(map[string]bool)
	for _, s := range sr.allSites {
		for _, d := range s.Config.Domains {
			d = strings.ToLower(d)
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

		site := sr.SiteByName(siteName)
		if site == nil {
			c.JSON(404, gin.H{"error": "site_not_found", "message": "No site: " + siteName})
			return
		}

		// Inject site context.
		c.Set("site_name", site.Name)
		c.Set("site_db", site.DB)
		c.Set("site_registry", site.Registry)

		// Handle API and workspace paths.
		if strings.HasPrefix(rest, "/api/") || rest == "/api" {
			// API requests: rewrite path and let Gin re-dispatch.
			// We need to re-enter the router for API routes to match.
			c.Request.URL.Path = rest
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
