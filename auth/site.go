package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
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
	return NewSiteGuardWithSessionManager(NewSessionManager(db))
}

// NewSiteGuardWithSessionManager binds request authentication to an explicitly
// owned session manager. The caller remains responsible for closing it.
func NewSiteGuardWithSessionManager(manager *SessionManager) *SiteGuard {
	if manager == nil {
		manager = newRequestSessionManager(nil)
	}
	return &SiteGuard{sessionMgr: manager}
}

// Close joins the session cache worker owned by this compatibility guard.
// Guards created with NewSiteGuardWithSessionManager do not close the supplied
// manager; its tenant/process owner remains responsible for that manager.
func (g *SiteGuard) Close() error {
	if g == nil || g.sessionMgr == nil {
		return nil
	}
	return g.sessionMgr.Close()
}

// Middleware returns the combined site guard middleware.
// It runs: Bearer (extension) auth → Session auth → CSRF → handler.
func (g *SiteGuard) Middleware(skipCSRF bool) gin.HandlerFunc {
	csrf := CSRFMiddleware()

	return func(c *gin.Context) {
		// Skip auth for login endpoint and health check.
		path := c.Request.URL.Path
		if path == "/api/ping" ||
			path == "/api/v1/ping" ||
			path == "/workspace/auth/login" {
			c.Next()
			return
		}
		// Check Bearer token for channel-session or extension API auth.
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != "" && token == os.Getenv("KORA_ENGINE_PROVISIONING_TOKEN") && ((c.Request.Method == http.MethodPost && (path == "/api/internal/channel/managed-client/rotate" || path == "/api/internal/channel/managed-client/revoke")) || (c.Request.Method == http.MethodGet && path == "/api/internal/site/identity")) {
				c.Set("auth_type", "engine_provisioner")
				c.Set("user", "kora-cloud-provisioner")
				c.Next()
				return
			}
			if token != "" && token == os.Getenv("KORA_ENGINE_CONFIG_TOKEN") && (path == "/api/system/config/drafts" || path == "/api/v1/system/config/drafts" || path == "/api/system/config/validate" || path == "/api/system/config/dry-run" || (c.Request.Method == http.MethodGet && (path == "/api/system/config/versions/head/snapshot" || path == "/api/v1/system/config/versions/head/snapshot" || path == "/api/system/config/versions/active/snapshot" || path == "/api/v1/system/config/versions/active/snapshot"))) {
				tenantID := c.GetString("site_id")
				if tenantID == "" {
					tenantID = c.GetString("site_name")
				}
				actor := contract.ActorContext{PrincipalID: "cloud-proposal", PrincipalType: contract.PrincipalService, Site: c.GetString("site_name"), Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now().UTC()}
				execution, err := policy.NewVerifiedExecution(contract.TenantID(tenantID), actor)
				if err != nil {
					c.AbortWithStatus(http.StatusUnauthorized)
					return
				}
				c.Set("auth_type", "engine_service")
				c.Set("user", "cloud-proposal")
				c.Set("user_role", "Administrator")
				c.Set("user_roles", []string{"Administrator"})
				c.Set("policy_execution", execution)
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
			// A session ID is also a valid bearer credential for first-party CLI
			// clients. validateSession applies the same tenant binding and role
			// policy as cookie-based browser sessions.
			if validateSession(c, g.sessionMgr) {
				c.Next()
			}
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
	actor := contract.ActorContext{
		PrincipalID: "channel:" + session.ID, PrincipalType: contract.PrincipalAgent,
		Site: c.GetString("site_name"), Channel: session.Provider, DeviceID: session.ClientName,
		AuthSessionID: session.ID, AuthenticatedAt: time.Now().UTC(),
	}
	execution, err := verifiedDelegatedExecution(c, actor, session.Permissions)
	if err != nil {
		return false
	}
	c.Set("policy_execution", execution)
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
	siteName := c.GetString("site_name")
	tenantID := c.GetString("site_id")
	if tenantID == "" {
		tenantID = siteName
	}
	execution, perms, err := AuthenticateExtensionExecution(c.Request.Context(), sqlDB, kdb.Resolve(c.GetString("site_db_type")), contract.TenantID(tenantID), siteName, token)
	if err != nil {
		return false
	}
	actor := execution.Actor()
	c.Set("auth_type", "extension")
	c.Set("extension_name", actor.DeviceID)
	c.Set("extension_permissions", perms)
	c.Set("policy_execution", execution)
	return true
}

// AuthenticateExtensionExecution verifies an active tenant extension token and
// returns its current, exact resource-operation grants. Call it per operation
// when a long-lived adapter must observe token revocation and permission
// changes. Credentials are never included in returned errors.
func AuthenticateExtensionExecution(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect, tenantID contract.TenantID, siteName, token string) (policy.Execution, []doctype.Permission, error) {
	if ctx == nil || database == nil || dialect == nil {
		return policy.Execution{}, nil, contract.NewError(contract.CodeUnauthenticated, "extension authentication is unavailable")
	}
	parsedTenant, err := contract.ParseTenantID(string(tenantID))
	if err != nil || strings.TrimSpace(siteName) == "" || siteName != strings.TrimSpace(siteName) || token == "" || token != strings.TrimSpace(token) || len(token) > 4096 {
		return policy.Execution{}, nil, contract.NewError(contract.CodeUnauthenticated, "invalid extension credential")
	}

	var extensionName string
	var permissionsJSON sql.NullString
	query := kdb.Rebind(dialect, `SELECT name, api_permissions FROM _kora_extension WHERE site = ? AND access_token = ? AND is_active = 1`)
	err = database.QueryRowContext(ctx, query, siteName, token).Scan(&extensionName, &permissionsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return policy.Execution{}, nil, contract.NewError(contract.CodeUnauthenticated, "invalid extension credential")
	}
	if err != nil {
		return policy.Execution{}, nil, fmt.Errorf("verify extension credential: %w", err)
	}
	if strings.TrimSpace(extensionName) == "" {
		return policy.Execution{}, nil, contract.NewError(contract.CodeUnauthenticated, "invalid extension credential")
	}

	permissions := parseExtensionPermissions(permissionsJSON.String)
	actor := contract.ActorContext{
		PrincipalID: "extension:" + extensionName, PrincipalType: contract.PrincipalService,
		Site: siteName, DeviceID: extensionName, AuthenticatedAt: time.Now().UTC(),
	}
	execution, err := delegatedExecution(parsedTenant, siteName, actor, permissions)
	if err != nil {
		return policy.Execution{}, nil, err
	}
	return execution, permissions, nil
}

// RevalidateRealtimeIdentity refreshes the credential and policy context for a
// long-lived realtime connection. It performs no response writes; false means
// the stream must close, while err reports a human-session storage failure.
func RevalidateRealtimeIdentity(c *gin.Context) (bool, error) {
	if c == nil {
		return false, nil
	}
	guard := &SiteGuard{}
	authType := c.GetString("auth_type")
	if authType == "extension" || authType == "channel_session" {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			return false, nil
		}
		token := strings.TrimPrefix(header, "Bearer ")
		if token == "" {
			return false, nil
		}
		if authType == "channel_session" {
			return guard.authenticateChannelSession(c, token), nil
		}
		return guard.authenticateExtension(c, token), nil
	}
	if authType != "" {
		return false, nil
	}

	sid := c.GetString("session_sid")
	dbValue, exists := c.Get("site_db")
	if sid == "" || !exists {
		return false, nil
	}
	database, ok := dbValue.(*sql.DB)
	if !ok || database == nil {
		return false, nil
	}
	manager := sessionManagerForRequest(c, database)
	if manager == nil {
		return false, nil
	}
	manager.InvalidateSession(sid)
	user, createdAt, err := manager.GetSession(c.GetString("site_name"), sid)
	if err != nil {
		return false, err
	}
	c.Set("user", user.Name)
	c.Set("user_obj", user)
	c.Set("session_created_at", createdAt)
	c.Set("user_roles", append([]string(nil), user.Roles...))
	userRole := ""
	if len(user.Roles) > 0 {
		userRole = user.Roles[0]
	}
	c.Set("user_role", userRole)
	tenantID := c.GetString("site_id")
	if tenantID == "" {
		tenantID = c.GetString("site_name")
	}
	actor := contract.ActorContext{
		PrincipalID: user.Name, PrincipalType: contract.PrincipalHuman,
		SubjectUserID: user.Name, Site: c.GetString("site_name"),
		Roles: append([]string(nil), user.Roles...), AuthenticatedAt: createdAt,
	}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(tenantID), actor)
	if err != nil {
		return false, fmt.Errorf("refresh realtime execution identity: %w", err)
	}
	c.Set("policy_execution", execution)
	return true, nil
}

func verifiedDelegatedExecution(c *gin.Context, actor contract.ActorContext, permissions []doctype.Permission) (policy.Execution, error) {
	tenantID := c.GetString("site_id")
	if tenantID == "" {
		tenantID = c.GetString("site_name")
	}
	return delegatedExecution(contract.TenantID(tenantID), c.GetString("site_name"), actor, permissions)
}

func delegatedExecution(tenantID contract.TenantID, siteName string, actor contract.ActorContext, permissions []doctype.Permission) (policy.Execution, error) {
	actor.Site = siteName
	grants := make([]policy.Grant, 0, len(permissions)*10)
	for _, permission := range permissions {
		for _, operation := range []string{"read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report"} {
			if HasExtensionPermission([]doctype.Permission{permission}, permission.Doctype, operation) {
				grants = append(grants, policy.Grant{Resource: permission.Doctype, Operation: operation, OwnerOnly: permission.IfOwner})
			}
		}
	}
	return policy.NewVerifiedExecutionWithGrants(tenantID, actor, grants)
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
