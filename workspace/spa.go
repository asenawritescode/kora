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

//go:embed all:dist
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
			serveFileOptimized(c, sub, reqPath)
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

		// 4. Serve /console and /console/* (SPA client-side routing).
		if reqPath == "/console" || strings.HasPrefix(reqPath, "/console/") {
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

	serveFileOptimized(c, sub, "/"+cleanPath)
}

func serveFileOptimized(c *gin.Context, sub fs.FS, reqPath string) {
	cleanPath := strings.TrimPrefix(reqPath, "/")

	// Determine if this is a hashed asset (immutable caching) or entry file.
	isHashedAsset := strings.HasPrefix(cleanPath, "assets/")

	// Try to serve a pre-compressed variant based on Accept-Encoding.
	acceptEnc := c.GetHeader("Accept-Encoding")
	supportsBrotli := strings.Contains(acceptEnc, "br")
	supportsGzip := strings.Contains(acceptEnc, "gzip")

	var data []byte
	var contentType string
	var contentEncoding string

	// Try brotli first (best compression), then gzip, then uncompressed.
	if supportsBrotli {
		if brData, err := fs.ReadFile(sub, cleanPath+".br"); err == nil {
			data = brData
			contentEncoding = "br"
		}
	}
	if data == nil && supportsGzip {
		if gzData, err := fs.ReadFile(sub, cleanPath+".gz"); err == nil {
			data = gzData
			contentEncoding = "gzip"
		}
	}
	if data == nil {
		// Fall back to uncompressed.
		var err error
		data, err = fs.ReadFile(sub, cleanPath)
		if err != nil {
			c.String(http.StatusNotFound, "Not found")
			return
		}
	}

	// Determine Content-Type from file extension.
	ext := filepath.Ext(cleanPath)
	contentType = mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Set caching headers.
	if isHashedAsset {
		// Content-hashed assets are immutable — cache for 1 year.
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Entry files (index.html) should always be revalidated.
		c.Header("Cache-Control", "no-cache")
	}

	// Set encoding header.
	if contentEncoding != "" {
		c.Header("Content-Encoding", contentEncoding)
	}
	c.Header("Vary", "Accept-Encoding")
	c.Header("Content-Type", contentType)

	// Write bytes directly — no string conversion.
	c.Data(http.StatusOK, contentType, data)
}
