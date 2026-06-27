package auth

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/doctype"
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
		if path == "/api/auth/login" || path == "/api/v1/auth/login" || path == "/api/ping" || path == "/api/v1/ping" || path == "/workspace/login" {
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
	var permsJSON sql.NullString
	err := sqlDB.QueryRow(
		`SELECT name, api_permissions FROM _kora_extension WHERE access_token = ? AND is_active = 1`, token,
	).Scan(&extName, &permsJSON)
	if err != nil {
		return false
	}

	perms := parseExtensionPermissions(permsJSON.String)

	c.Set("auth_type", "extension")
	c.Set("extension_name", extName)
	c.Set("extension_permissions", perms)
	return true
}

// parseExtensionPermissions parses the api_permissions JSON from the database.
// Handles both boolean-flag format: [{"doctype":"X","read":true,"create":true}]
// and operations-array format: [{"doctype":"X","operations":["read","create"]}].
// Returns an empty slice on empty/null/malformed input.
func parseExtensionPermissions(raw string) []doctype.Permission {
	if raw == "" || raw == "null" || raw == "[]" {
		return []doctype.Permission{}
	}

	// Try operations-array format: [{"doctype":"X","operations":["read","create"]}]
	var opsPerms []struct {
		Doctype    string   `json:"doctype"`
		Operations []string `json:"operations"`
	}
	if err := json.Unmarshal([]byte(raw), &opsPerms); err == nil && len(opsPerms) > 0 {
		hasOps := false
		for _, op := range opsPerms {
			if len(op.Operations) > 0 {
				hasOps = true
				break
			}
		}
		if hasOps {
			perms := make([]doctype.Permission, len(opsPerms))
			for i, op := range opsPerms {
				opSet := make(map[string]bool, len(op.Operations))
				for _, o := range op.Operations {
					opSet[o] = true
				}
				perms[i] = doctype.Permission{
					Doctype: op.Doctype,
					Read:    opSet["read"],
					Write:   opSet["write"],
					Create:  opSet["create"],
					Delete:  opSet["delete"],
					Submit:  opSet["submit"],
					Cancel:  opSet["cancel"],
					Amend:   opSet["amend"],
					Export:  opSet["export"],
					Import:  opSet["import"],
					Report:  opSet["report"],
				}
			}
			return perms
		}
	}

	// Fall back to boolean-flag format (doctype.Permission JSON tags).
	var perms []doctype.Permission
	if err := json.Unmarshal([]byte(raw), &perms); err != nil {
		slog.Warn("extension has malformed api_permissions",
			"api_permissions", raw, "error", err)
		return []doctype.Permission{}
	}
	return perms
}

// HasExtensionPermission checks whether the extension's scoped permissions
// grant the requested operation on the given doctype.
// Returns false for empty/nil permissions (secure by default).
func HasExtensionPermission(perms []doctype.Permission, doctype, operation string) bool {
	for _, p := range perms {
		if p.Doctype != doctype {
			continue
		}
		switch operation {
		case "read":
			return p.Read
		case "write":
			return p.Write
		case "create":
			return p.Create
		case "delete":
			return p.Delete
		case "submit":
			return p.Submit
		case "cancel":
			return p.Cancel
		case "amend":
			return p.Amend
		case "export":
			return p.Export
		case "import":
			return p.Import
		case "report":
			return p.Report
		}
	}
	return false
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
