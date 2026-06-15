package workspace

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	knet "github.com/asenawritescode/kora/net"
)

//go:embed dist/*
var spaFS embed.FS

// SPAFS returns the embedded filesystem containing the built SPA.
func SPAFS() fs.FS {
	sub, err := fs.Sub(spaFS, "dist")
	if err != nil {
		return nil
	}
	return sub
}

// RegisterSPARoutes serves the SPA at /workspace, /assets, and /s/:site/*.
// Uses a single NoRoute handler to avoid conflicts.
func RegisterSPARoutes(router *gin.Engine, siteRouter *knet.SiteRouter) {
	router.RedirectTrailingSlash = false

	sub, err := fs.Sub(spaFS, "dist")
	if err != nil {
		panic("SPA build not found in workspace/dist")
	}

	router.NoRoute(func(c *gin.Context) {
		reqPath := c.Request.URL.Path

		// 1. Serve /assets/* for SPA static files.
		if strings.HasPrefix(reqPath, "/assets/") {
			serveFile(c, sub, reqPath)
			return
		}

		// 2. Path-based site access: /s/:site/...
		if strings.HasPrefix(reqPath, "/s/") {
			handlePathSite(c, sub, siteRouter, router, reqPath)
			return
		}

		// 3. Serve /workspace and /workspace/* (SPA client-side routing).
		if reqPath == "/workspace" || strings.HasPrefix(reqPath, "/workspace/") {
			serveSPA(c, sub, reqPath)
			return
		}

		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	})
}

// handlePathSite handles /s/:site/* requests for multi-site access.
func handlePathSite(c *gin.Context, sub fs.FS, sr *knet.SiteRouter, router *gin.Engine, path string) {
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
		c.JSON(http.StatusNotFound, gin.H{"error": "site_not_found", "message": "No site: " + siteName})
		return
	}

	// Inject site context and set persistent cookie for API calls.
	c.Set("site_name", site.Name)
	c.Set("site_db", site.DB)
	c.Set("site_registry", site.Registry)
	knet.SetSecureCookie(c, "kora_site", site.Name, 86400, "/", false)

	// API requests: re-dispatch through router so /api/* routes match.
	if strings.HasPrefix(rest, "/api/") || rest == "/api" {
		c.Request.URL.Path = rest
		router.HandleContext(c)
		return
	}

	// Serve SPA for /workspace paths.
	serveSPA(c, sub, rest)
}

func serveSPA(c *gin.Context, sub fs.FS, reqPath string) {
	servePath := reqPath
	if servePath == "" || servePath == "/" {
		servePath = "/index.html"
	}

	cleanPath := strings.TrimPrefix(servePath, "/")
	if _, err := sub.Open(cleanPath); err != nil {
		cleanPath = "index.html"
	}

	serveFile(c, sub, "/"+cleanPath)
}

func serveFile(c *gin.Context, sub fs.FS, reqPath string) {
	cleanPath := strings.TrimPrefix(reqPath, "/")
	data, err := fs.ReadFile(sub, cleanPath)
	if err != nil {
		c.String(http.StatusNotFound, "Not found")
		return
	}

	ext := filepath.Ext(cleanPath)
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "application/octet-stream"
	}
	c.Header("Content-Type", ct)
	c.String(http.StatusOK, "%s", string(data))
}
