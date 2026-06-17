package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/auth"
	sqlDialect "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

// ConsoleHandler holds dependencies for console API endpoints.
type ConsoleHandler struct {
	SystemGuard         *auth.SystemGuard
	SiteRouter          *net.SiteRouter
	PlatformDBType      string
	PlatformDBHost      string
	PlatformDBPort      int
	PlatformDBUser      string
	PlatformDBPassword  string
	PlatformDB          *sql.DB // Existing platform DB connection (for LibSQL reuse)
}

// NewConsoleHandler creates a console API handler.
func NewConsoleHandler(guard *auth.SystemGuard, sr *net.SiteRouter, dbType, dbHost, dbUser, dbPassword string, dbPort int, platformDB *sql.DB) *ConsoleHandler {
	return &ConsoleHandler{
		SystemGuard:        guard,
		SiteRouter:         sr,
		PlatformDBType:     dbType,
		PlatformDBHost:     dbHost,
		PlatformDBPort:     dbPort,
		PlatformDBUser:     dbUser,
		PlatformDBPassword: dbPassword,
		PlatformDB:         platformDB,
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

// HandleLogin authenticates a console super-admin.
// POST /api/console/login
func (h *ConsoleHandler) HandleLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}

	valid, needsChange := h.SystemGuard.ValidateWithChangeCheck(req.Email, req.Password)
	if !valid {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: map[string]string{"message": "Invalid credentials"}})
		return
	}

	token := h.SystemGuard.CreateSession(req.Email)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"token":                 token,
			"email":                 req.Email,
			"needs_password_change": needsChange,
		},
	})
}

// HandleChangePassword forces a password change (required on first login with default creds).
// POST /api/console/change-password
func (h *ConsoleHandler) HandleChangePassword(c *gin.Context) {
	token := c.GetHeader("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")

	if !h.SystemGuard.ValidateSessionBool(token) {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: map[string]string{"message": "Invalid session"}})
		return
	}

	var req struct {
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.NewPassword == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "New password required"}})
		return
	}

	h.SystemGuard.UpdatePassword(req.NewPassword)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"message": "Password changed"}})
}

// ---------------------------------------------------------------------------
// Auth middleware for console API routes
// ---------------------------------------------------------------------------

// RequireConsoleAuth is middleware that validates the console session.
// Accepts Authorization: Bearer <token> header OR kora_console_sid cookie.
func (h *ConsoleHandler) RequireConsoleAuth(c *gin.Context) {
	token := c.GetHeader("Authorization")
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		// Fallback: check the console session cookie.
		if sid, err := c.Cookie("kora_console_sid"); err == nil && sid != "" {
			token = sid
		}
	}
	if token == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: map[string]string{"message": "Authentication required"}})
		return
	}
	if !h.SystemGuard.ValidateSessionBool(token) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: map[string]string{"message": "Invalid or expired session"}})
		return
	}
	c.Next()
}

// ---------------------------------------------------------------------------
// Site Management
// ---------------------------------------------------------------------------

// HandleListSites returns all loaded sites with status.
// Falls back to querying the database directly when no sites are loaded (e.g. after
// container redeploy where site_config.yaml files were lost but DB data persists).
// GET /api/console/sites
func (h *ConsoleHandler) HandleListSites(c *gin.Context) {
	sites := h.SiteRouter.AllSites()

	// If no sites in memory, try the database — sites survive redeploys there.
	if len(sites) == 0 && h.PlatformDB != nil {
		if dbSites, err := site.DiscoverSitesFromDB(h.PlatformDB); err == nil {
			for _, info := range dbSites {
				sites = append(sites, &net.LoadedSite{
					Name:   info.Name,
					Config: net.SiteRouterConfig{Hostname: info.Name, Domains: info.Domains},
					DB:     h.PlatformDB,
				})
			}
		}
	}

	type SiteEntry struct {
		Name     string   `json:"name"`
		Domains  []string `json:"domains"`
		DocTypes int      `json:"doctypes"`
		Status   string   `json:"status"`
	}
	var result []SiteEntry
	for _, s := range sites {
		status := "active"
		if s.DB != nil {
			if err := s.DB.Ping(); err != nil {
				status = "error"
			}
		} else {
			status = "unknown"
		}
		result = append(result, SiteEntry{
			Name:     s.Name,
			Domains:  s.Config.Domains,
			DocTypes: len(s.Registry.All()),
			Status:   status,
		})
	}
	if result == nil {
		result = []SiteEntry{} // return empty array, not null
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

// HandleCreateSite creates a new site: database, config, bootstrap, admin user.
// POST /api/console/sites
// Only hostname, admin_email, and admin_password are required.
// DB fields are optional — platform defaults from env vars are used when empty.
func (h *ConsoleHandler) HandleCreateSite(c *gin.Context) {
	var req struct {
		Hostname      string `json:"hostname"`
		DBType        string `json:"db_type"`
		DBHost        string `json:"db_host"`
		DBPort        int    `json:"db_port"`
		DBName        string `json:"db_name"`
		DBUser        string `json:"db_user"`
		DBPassword    string `json:"db_password"`
		Domains       string `json:"domains"` // comma-separated extra domains
		AdminEmail    string `json:"admin_email"`
		AdminPassword string `json:"admin_password"`
		AdminFullName string `json:"admin_full_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request: " + err.Error()}})
		return
	}
	if req.Hostname == "" || req.AdminEmail == "" || req.AdminPassword == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "hostname, admin_email, and admin_password are required"}})
		return
	}

	slog.Info("creating site via console", "hostname", req.Hostname)

	// Resolve platform DB credentials: handler fields (from common config) → env vars.
	platformType := h.PlatformDBType
	if platformType == "" {
		platformType = os.Getenv("KORA_DB_TYPE")
	}
	if platformType == "" {
		platformType = "mysql"
	}
	platformHost := h.PlatformDBHost
	if platformHost == "" {
		platformHost = os.Getenv("KORA_DB_HOST")
	}
	platformPort := h.PlatformDBPort
	if platformPort == 0 {
		platformPort = envConsoleInt("KORA_DB_PORT")
	}
	platformUser := h.PlatformDBUser
	if platformUser == "" {
		platformUser = os.Getenv("KORA_DB_USER")
	}
	platformPass := h.PlatformDBPassword
	if platformPass == "" {
		platformPass = os.Getenv("KORA_DB_PASSWORD")
	}

	// Parse comma-separated extra domains.
	var extraDomains []string
	if req.Domains != "" {
		for _, d := range strings.Split(req.Domains, ",") {
			d = strings.TrimSpace(d)
			if d != "" && d != req.Hostname {
				extraDomains = append(extraDomains, d)
			}
		}
	}

	result, err := site.CreateSite(site.CreateSiteInput{
		Hostname:            req.Hostname,
		DBType:              req.DBType,
		DBHost:              req.DBHost,
		DBPort:              req.DBPort,
		DBName:              req.DBName,
		DBUser:              req.DBUser,
		DBPassword:          req.DBPassword,
		AdminEmail:          req.AdminEmail,
		AdminPassword:       req.AdminPassword,
		AdminFullName:       req.AdminFullName,
		ExtraDomains:         extraDomains,
		PlatformDBType:      platformType,
		PlatformDBHost:      platformHost,
		PlatformDBPort:      platformPort,
		PlatformDBUser:      platformUser,
		PlatformDBPassword:  platformPass,
		PlatformDB:          h.PlatformDB,
	})
	if err != nil {
		slog.Error("creating site failed", "hostname", req.Hostname, "error", err)
		errMsg := err.Error()
		// Map known errors to user-friendly messages.
		switch {
		case strings.Contains(errMsg, "connection refused"):
			errMsg = "Cannot connect to MySQL server. Is MySQL running?"
		case strings.Contains(errMsg, "Access denied"):
			errMsg = "Invalid database credentials. Check your DB user and password."
		case strings.Contains(errMsg, "Unknown database"):
			errMsg = "Cannot access the database server. Check your DB host and port."
		default:
			errMsg = "Failed to create site: " + errMsg
		}
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": errMsg}})
		return
	}

	// Hot-add site to the running router.
	// Collect all domains: hostname + explicit extra domains + request host (for sslip.io etc.)
	domains := []string{req.Hostname}
	domains = append(domains, extraDomains...)
	requestHost := strings.ToLower(strings.Split(c.Request.Host, ":")[0])
	if requestHost != "" && requestHost != req.Hostname && requestHost != "localhost" && requestHost != "127.0.0.1" && requestHost != "::1" {
		domains = append(domains, requestHost)
	}
	loaded := &net.LoadedSite{
		Name: req.Hostname,
		Config: net.SiteRouterConfig{
			Hostname: req.Hostname,
			Domains:  domains,
		},
		DB:       result.DB,
		Registry: result.Registry,
	}
	h.SiteRouter.AddSite(loaded)

	slog.Info("site created via console", "hostname", req.Hostname, "db_name", result.Config.DBName)
	c.JSON(http.StatusCreated, Response{
		Data: map[string]any{
			"hostname": req.Hostname,
			"db_name":  result.Config.DBName,
			"status":   "active",
			"admin":    req.AdminEmail,
		},
	})
}

// HandleUpdateSite updates site metadata (domains).
// PUT /api/console/sites/:name
func (h *ConsoleHandler) HandleUpdateSite(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "site name required"}})
		return
	}

	var req struct {
		Domains []string `json:"domains"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}

	site := h.SiteRouter.SiteByName(siteName)
	if site == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Site not found: " + siteName}})
		return
	}

	// Update domains in the in-memory router.
	site.Config.Domains = req.Domains
	if len(site.Config.Domains) == 0 {
		site.Config.Domains = []string{site.Config.Hostname}
	}

	// Persist to DB if platform DB is available.
	if h.PlatformDB != nil {
		domainsJSON, _ := json.Marshal(req.Domains)
		h.PlatformDB.Exec(
			"UPDATE _kora_config_version SET config = ? WHERE site = ? AND status = 'Active'",
			fmt.Sprintf(`{"domains": %s}`, string(domainsJSON)), siteName,
		)
	}

	slog.Info("site updated via console", "hostname", siteName, "domains", req.Domains)
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"hostname": siteName,
		"domains":  site.Config.Domains,
	}})
}

// HandleDeleteSite deletes a site and all its data.
// DELETE /api/console/sites/:name
// Requires confirmation: {"confirm": "<hostname>"}
func (h *ConsoleHandler) HandleDeleteSite(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "site name required"}})
		return
	}

	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	if req.Confirm != siteName {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Type the site hostname to confirm deletion."}})
		return
	}

	loaded := h.SiteRouter.SiteByName(siteName)
	if loaded == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Site not found: " + siteName}})
		return
	}

	// Derive DB name from hostname.
	dbName := strings.ReplaceAll(siteName, ".", "_")

	slog.Info("deleting site via console", "hostname", siteName)

	if err := site.DeleteSite(site.DeleteSiteInput{
		DB:         loaded.DB,
		Dialect:    sqlDialect.Resolve(h.PlatformDBType),
		Hostname:   siteName,
		DBType:     h.PlatformDBType,
		DBName:     dbName,
		DBHost:     h.PlatformDBHost,
		DBPort:     h.PlatformDBPort,
		DBUser:     h.PlatformDBUser,
		DBPassword: h.PlatformDBPassword,
	}); err != nil {
		slog.Error("deleting site failed", "hostname", siteName, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to delete site: " + err.Error()}})
		return
	}

	// Remove from the in-memory router.
	h.SiteRouter.RemoveSite(siteName)

	slog.Info("site deleted via console", "hostname", siteName)
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"hostname": siteName,
		"deleted":  true,
	}})
}

// HandleResetSitePassword resets a site user's password.
// POST /api/console/sites/:name/reset-password
func (h *ConsoleHandler) HandleResetSitePassword(c *gin.Context) {
	siteName := c.Param("name")
	if siteName == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "site name required"}})
		return
	}

	var req struct {
		Email       string `json:"email"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	if req.Email == "" || req.NewPassword == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "email and new_password are required"}})
		return
	}

	loaded := h.SiteRouter.SiteByName(siteName)
	if loaded == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Site not found: " + siteName}})
		return
	}

	// Hash the new password.
	passwordHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		slog.Error("hashing password failed", "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to hash password."}})
		return
	}

	// Update the user's password in the site's database.
	result, err := loaded.DB.Exec(
		"UPDATE _kora_user SET password_hash = ?, modified = CURRENT_TIMESTAMP WHERE email = ?",
		passwordHash, req.Email,
	)
	if err != nil {
		slog.Error("resetting site password failed", "site", siteName, "email", req.Email, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to update password: " + err.Error()}})
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "No user found with email: " + req.Email}})
		return
	}

	// Invalidate all sessions for this user.
	loaded.DB.Exec("DELETE FROM _kora_session WHERE user = (SELECT name FROM _kora_user WHERE email = ?)", req.Email)

	slog.Info("site user password reset via console", "site", siteName, "email", req.Email)
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"message": "Password reset successfully. All existing sessions have been invalidated.",
	}})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// envConsoleInt reads an integer env var, returning 0 if empty or unparseable.
func envConsoleInt(key string) int {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n := 0
	for _, c := range v {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
