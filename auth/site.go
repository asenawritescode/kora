package auth

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// SiteGuard is a unified middleware that enforces authentication, CSRF,
// and site context resolution for workspace and API routes.
// It wraps AuthMiddleware + CSRFMiddleware + site context injection.
type SiteGuard struct {
	sessionMgr *SessionManager
}

// NewSiteGuard creates a SiteGuard.
func NewSiteGuard(db *sql.DB) *SiteGuard {
	return &SiteGuard{
		sessionMgr: NewSessionManager(db),
	}
}

// Middleware returns the combined site guard middleware.
// It runs: Bearer (extension) auth → Session auth → CSRF → handler.
func (g *SiteGuard) Middleware(skipCSRF bool) gin.HandlerFunc {
	csrf := CSRFMiddleware()

	return func(c *gin.Context) {
		// Skip auth for login endpoint and health check.
		path := c.Request.URL.Path
		if path == "/api/auth/login" || path == "/api/ping" || path == "/workspace/login" {
			c.Next()
			return
		}

		// Check Bearer token for extension API auth.
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if g.authenticateExtension(c, token) {
				// Extension-authenticated — skip session and CSRF checks.
				c.Next()
				return
			}
			// Invalid Bearer token — reject.
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}

		// Session auth check — validates session without calling c.Next().
		if !validateSession(c, g.sessionMgr) {
			return
		}

		// Inject site context from SiteRouter into handlers.
		if db, exists := c.Get("site_db"); exists {
			if siteDB, ok := db.(*sql.DB); ok {
				c.Set("db", siteDB)
			}
		}
		if reg, exists := c.Get("site_registry"); exists {
			c.Set("registry", reg)
		}

		// CSRF check runs BEFORE the handler — abort if token is missing/invalid.
		if !skipCSRF {
			csrf(c)
			if c.IsAborted() {
				return
			}
		}

		c.Next()
	}
}

// authenticateExtension verifies a Bearer access token against the _kora_extension table.
// On success, it sets auth_type=extension and extension_name in context.
// The site_db must already be set in context by SiteRouter.
func (g *SiteGuard) authenticateExtension(c *gin.Context, token string) bool {
	db, exists := c.Get("site_db")
	if !exists {
		return false
	}
	sqlDB, ok := db.(*sql.DB)
	if !ok || sqlDB == nil {
		return false
	}

	var extName string
	err := sqlDB.QueryRow(
		`SELECT name FROM _kora_extension WHERE access_token = ? AND is_active = 1`, token,
	).Scan(&extName)
	if err != nil {
		return false
	}

	c.Set("auth_type", "extension")
	c.Set("extension_name", extName)
	return true
}

// SiteDB returns the site's database from the request context.
func SiteDB(c *gin.Context) *sql.DB {
	db, _ := c.Get("site_db")
	if db == nil {
		return nil
	}
	return db.(*sql.DB)
}

// SiteRegistry returns the site's DocType registry from the request context.
func SiteRegistry(c *gin.Context) interface{} {
	reg, _ := c.Get("site_registry")
	return reg
}
