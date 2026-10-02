package auth

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
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
		if path == "/api/auth/login" ||
			path == "/api/auth/providers" ||
			path == "/api/auth/magic-link/request" ||
			path == "/api/auth/magic-link/verify" ||
			path == "/api/v1/auth/login" ||
			path == "/api/ping" ||
			path == "/api/v1/ping" ||
			path == "/workspace/login" ||
			path == "/workspace/auth/login" {
			c.Next()
			return
		}
		// Starting a web conversation is the public first step of onboarding.
		// SiteRouter has already resolved the tenant before this middleware runs;
		// subsequent reads and writes require the normal workspace session.
		if c.Request.Method == http.MethodPost && (path == "/api/v1/cloud/conversations" || path == "/api/cloud/conversations") {
			c.Next()
			return
		}

		// Check Bearer token for channel-session or extension API auth.
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != "" && token == os.Getenv("KORA_ENGINE_PROVISIONING_TOKEN") && ((c.Request.Method == http.MethodPost && path == "/api/internal/channel/managed-client/rotate") || (c.Request.Method == http.MethodGet && path == "/api/internal/site/identity")) {
				c.Set("auth_type", "engine_provisioner")
				c.Set("user", "kora-cloud-provisioner")
				c.Next()
				return
			}
			if token != "" && token == os.Getenv("KORA_ENGINE_CONFIG_TOKEN") && (path == "/api/system/config/drafts" || path == "/api/v1/system/config/drafts" || path == "/api/system/config/validate" || path == "/api/system/config/dry-run") {
				c.Set("auth_type", "engine_service")
				c.Set("user", "cloud-proposal")
				c.Set("user_role", "Administrator")
				c.Set("user_roles", []string{"Administrator"})
				c.Next()
				return
			}
			if g.authenticateChannelSession(c, token) {
				c.Next()
				return
			}
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

// authenticateChannelSession verifies a delegated channel session Bearer token.
// On success, it sets auth_type=channel_session and channel_permissions in context.
func (g *SiteGuard) authenticateChannelSession(c *gin.Context, token string) bool {
	db, exists := c.Get("site_db")
	if !exists {
		return false
	}
	sqlDB, ok := db.(*sql.DB)
	if !ok || sqlDB == nil {
		return false
	}

	session, err := AuthenticateChannelSession(sqlDB, token)
	if err != nil {
		return false
	}

	c.Set("auth_type", "channel_session")
	c.Set("channel_session_id", session.ID)
	c.Set("channel_client_name", session.ClientName)
	c.Set("channel_permissions", session.Permissions)
	c.Set("channel_conversation_key", session.ConversationKey)
	c.Set("channel_sender_address", session.SenderAddress)
	return true
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
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)

	var extName string
	var permsJSON sql.NullString
	err := sqlDB.QueryRow(
		`SELECT name, api_permissions FROM _kora_extension WHERE site = ? AND access_token = ? AND is_active = 1`, siteNameStr, token,
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
