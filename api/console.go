package api

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"
	_ "github.com/go-sql-driver/mysql"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/site"
)

// ConsoleHandler holds dependencies for console API endpoints.
type ConsoleHandler struct {
	SystemGuard *auth.SystemGuard
	SiteRouter  *net.SiteRouter
}

// NewConsoleHandler creates a console API handler.
func NewConsoleHandler(guard *auth.SystemGuard, sr *net.SiteRouter) *ConsoleHandler {
	return &ConsoleHandler{SystemGuard: guard, SiteRouter: sr}
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
			"token":         token,
			"email":         req.Email,
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
// GET /api/console/sites
func (h *ConsoleHandler) HandleListSites(c *gin.Context) {
	sites := h.SiteRouter.AllSites()
	type SiteEntry struct {
		Name      string   `json:"name"`
		Domains   []string `json:"domains"`
		DocTypes  int      `json:"doctypes"`
		Status    string   `json:"status"`
	}
	var result []SiteEntry
	for _, s := range sites {
		status := "active"
		if err := s.DB.Ping(); err != nil {
			status = "error"
		}
		result = append(result, SiteEntry{
			Name:    s.Name,
			Domains: s.Config.Domains,
			DocTypes: len(s.Registry.All()),
			Status:  status,
		})
	}
	if result == nil {
		result = []SiteEntry{} // return empty array, not null
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

// HandleCreateSite creates a new site: database, config, bootstrap, admin user.
// POST /api/console/sites
func (h *ConsoleHandler) HandleCreateSite(c *gin.Context) {
	var req struct {
		Hostname        string `json:"hostname"`
		DBHost          string `json:"db_host"`
		DBPort          int    `json:"db_port"`
		DBName          string `json:"db_name"`
		DBUser          string `json:"db_user"`
		DBPassword      string `json:"db_password"`
		AdminEmail      string `json:"admin_email"`
		AdminPassword   string `json:"admin_password"`
		AdminFullName   string `json:"admin_full_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request: " + err.Error()}})
		return
	}
	if req.Hostname == "" || req.DBHost == "" || req.DBName == "" || req.AdminEmail == "" || req.AdminPassword == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "hostname, db_host, db_name, admin_email, and admin_password are required"}})
		return
	}
	if req.DBPort == 0 {
		req.DBPort = 3306
	}
	if req.DBUser == "" {
		req.DBUser = "root"
	}
	if req.AdminFullName == "" {
		req.AdminFullName = "Administrator"
	}

	slog.Info("creating site via console", "hostname", req.Hostname, "db_name", req.DBName)

	// Step 1: Create database.
	siteCfg := &site.SiteConfig{
		DBHost:      req.DBHost,
		DBPort:      req.DBPort,
		DBUser:      req.DBUser,
		DBPassword:  req.DBPassword,
		DBName:      req.DBName,
		Hostname:    req.Hostname,
		FileStorage: "local",
		FilesPath:   fmt.Sprintf("sites/%s/files", req.Hostname),
		Apps:        []string{"core"},
		DomainsList: []string{req.Hostname},
	}

	if err := site.CreateDatabase(siteCfg); err != nil {
		slog.Error("creating database failed", "hostname", req.Hostname, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to create database: " + err.Error()}})
		return
	}

	// Step 2: Write site config to disk.
	configDir := os.Getenv("KORA_CONFIG_DIR")
	if configDir == "" {
		configDir = "."
	}
	if err := writeSiteConfigToDir(configDir, req.Hostname, siteCfg); err != nil {
		slog.Error("writing site config failed", "hostname", req.Hostname, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to write site config: " + err.Error()}})
		return
	}

	// Step 3: Connect to the new database.
	db, err := site.Connect(siteCfg)
	if err != nil {
		slog.Error("connecting to new site DB failed", "hostname", req.Hostname, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to connect to database: " + err.Error()}})
		return
	}

	// Step 4: Bootstrap system tables.
	if err := bootstrapSystemTables(db); err != nil {
		db.Close()
		slog.Error("bootstrapping system tables failed", "hostname", req.Hostname, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to bootstrap system tables: " + err.Error()}})
		return
	}

	// Step 5: Create admin user.
	passwordHash, err := auth.HashPassword(req.AdminPassword)
	if err != nil {
		db.Close()
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to hash password"}})
		return
	}
	_, err = db.Exec(
		`INSERT IGNORE INTO _kora_user (name, email, password_hash, full_name, roles)
		 VALUES (?, ?, ?, ?, ?)`,
		ulid.Make().String(), req.AdminEmail, passwordHash, req.AdminFullName, "Administrator",
	)
	if err != nil {
		db.Close()
		slog.Error("creating admin user failed", "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to create admin user: " + err.Error()}})
		return
	}

	// Step 6: Build empty registry and create initial config version.
	registry := doctype.NewRegistry()
	var emptyDoctypes []*doctype.DocType
	registry.LoadFull(emptyDoctypes, nil, nil)

	
	// Create initial config version if none exists.
	var count int
	db.QueryRow("SELECT COUNT(*) FROM _kora_config_version WHERE site = ?", req.Hostname).Scan(&count)
	if count == 0 {
		versionID := ulid.Make().String()
		_, err = db.Exec(
			`INSERT INTO _kora_config_version (id, site, version, created_by, label, status, config, is_active)
			 VALUES (?, ?, 1, 'setup', 'Initial setup', 'Active', '{}', 0)`,
			versionID, req.Hostname,
		)
		if err != nil {
			slog.Warn("creating initial config version failed", "error", err)
		} else {
			db.Exec("UPDATE _kora_config_version SET is_active = 1 WHERE id = ?", versionID)
		}
	}

	// Step 7: Hot-add site to the running router.
	loaded := &net.LoadedSite{
		Name:   req.Hostname,
		Config: net.SiteRouterConfig{
			Hostname: req.Hostname,
			Domains:  []string{req.Hostname},
		},
		DB:       db,
		Registry: registry,
	}
	h.SiteRouter.AddSite(loaded)

	slog.Info("site created via console", "hostname", req.Hostname, "db_name", req.DBName)
	c.JSON(http.StatusCreated, Response{
		Data: gin.H{
			"hostname": req.Hostname,
			"db_name":  req.DBName,
			"status":   "active",
			"admin":    req.AdminEmail,
		},
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// bootstrapSystemTables creates the _kora_* tables if they don't exist.
func bootstrapSystemTables(db *sql.DB) error {
	tables := []string{
		`CREATE TABLE IF NOT EXISTS _kora_user (
			name VARCHAR(140) NOT NULL,
			email VARCHAR(140) NOT NULL,
			password_hash VARCHAR(255) NOT NULL,
			full_name VARCHAR(140) NOT NULL DEFAULT '',
			enabled TINYINT(1) NOT NULL DEFAULT 1,
			roles VARCHAR(255) NOT NULL DEFAULT '',
			creation DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			modified DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			PRIMARY KEY (name),
			UNIQUE KEY uq_email (email)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_session (
			sid VARCHAR(255) NOT NULL,
			user VARCHAR(140) NOT NULL,
			data JSON,
			expires_at DATETIME(6) NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (sid),
			INDEX idx_session_user (user),
			INDEX idx_session_expires (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_doctype (
			name VARCHAR(140) NOT NULL,
			module VARCHAR(140) NOT NULL DEFAULT '',
			is_submittable TINYINT(1) NOT NULL DEFAULT 0,
			is_child_table TINYINT(1) NOT NULL DEFAULT 0,
			is_single TINYINT(1) NOT NULL DEFAULT 0,
			track_changes TINYINT(1) NOT NULL DEFAULT 0,
			title_field VARCHAR(140) NOT NULL DEFAULT 'name',
			search_fields VARCHAR(255) NOT NULL DEFAULT '',
			sort_field VARCHAR(140) NOT NULL DEFAULT 'modified',
			sort_order VARCHAR(4) NOT NULL DEFAULT 'DESC',
			description TEXT,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_field (
			name VARCHAR(255) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			fieldname VARCHAR(140) NOT NULL,
			fieldtype VARCHAR(140) NOT NULL,
			label VARCHAR(140) NOT NULL,
			options VARCHAR(255) NOT NULL DEFAULT '',
			reqd TINYINT(1) NOT NULL DEFAULT 0,
			`+"`unique`"+` TINYINT(1) NOT NULL DEFAULT 0,
			default_value VARCHAR(255) NOT NULL DEFAULT '',
			hidden TINYINT(1) NOT NULL DEFAULT 0,
			read_only TINYINT(1) NOT NULL DEFAULT 0,
			bold TINYINT(1) NOT NULL DEFAULT 0,
			in_list_view TINYINT(1) NOT NULL DEFAULT 0,
			in_standard_filter TINYINT(1) NOT NULL DEFAULT 0,
			search_index TINYINT(1) NOT NULL DEFAULT 0,
			description TEXT,
			depends_on VARCHAR(140) NOT NULL DEFAULT '',
			mandatory_depends_on VARCHAR(140) NOT NULL DEFAULT '',
			computed VARCHAR(255) NOT NULL DEFAULT '',
			linked_field VARCHAR(255) NOT NULL DEFAULT '',
			renamed_from VARCHAR(140) NOT NULL DEFAULT '',
			idx INT NOT NULL DEFAULT 0,
			PRIMARY KEY (name),
			INDEX idx_doctype (doctype)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_config_version (
			id VARCHAR(140) NOT NULL,
			site VARCHAR(140) NOT NULL DEFAULT '',
			version INT NOT NULL DEFAULT 1,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			created_by VARCHAR(140) NOT NULL DEFAULT '',
			label VARCHAR(255) NOT NULL DEFAULT '',
			changelog TEXT,
			status VARCHAR(20) NOT NULL DEFAULT 'Draft',
			config JSON,
			is_active TINYINT(1) NOT NULL DEFAULT 0,
			PRIMARY KEY (id),
			INDEX idx_site (site)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_role (
			name VARCHAR(140) NOT NULL,
			description TEXT,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_permission (
			role VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			can_read TINYINT(1) NOT NULL DEFAULT 0,
			can_write TINYINT(1) NOT NULL DEFAULT 0,
			can_create TINYINT(1) NOT NULL DEFAULT 0,
			can_delete TINYINT(1) NOT NULL DEFAULT 0,
			can_submit TINYINT(1) NOT NULL DEFAULT 0,
			can_cancel TINYINT(1) NOT NULL DEFAULT 0,
			can_amend TINYINT(1) NOT NULL DEFAULT 0,
			PRIMARY KEY (role, doctype)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow (
			name VARCHAR(140) NOT NULL,
			doctype VARCHAR(140) NOT NULL,
			is_active TINYINT(1) NOT NULL DEFAULT 1,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow_state (
			name VARCHAR(255) NOT NULL,
			workflow VARCHAR(140) NOT NULL,
			label VARCHAR(140) NOT NULL,
			is_initial TINYINT(1) NOT NULL DEFAULT 0,
			doc_status TINYINT(1) NOT NULL DEFAULT 0,
			color VARCHAR(20) NOT NULL DEFAULT '',
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_workflow_transition (
			name VARCHAR(255) NOT NULL,
			workflow VARCHAR(140) NOT NULL,
			from_state VARCHAR(255) NOT NULL,
			to_state VARCHAR(255) NOT NULL,
			label VARCHAR(140) NOT NULL,
			allowed_role VARCHAR(255) NOT NULL DEFAULT '',
			condition_expr TEXT,
			PRIMARY KEY (name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,

		`CREATE TABLE IF NOT EXISTS _kora_secret (
			site VARCHAR(140) NOT NULL,
			key_name VARCHAR(140) NOT NULL,
			encrypted_value BLOB NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
			PRIMARY KEY (site, key_name)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}

	for _, ddl := range tables {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("creating table: %w\nSQL: %s", err, ddl)
		}
	}

	// Insert Administrator role if not exists.
	db.Exec(`INSERT IGNORE INTO _kora_role (name, description) VALUES ('Administrator', 'Full access to all doctypes')`)

	return nil
}

// writeSiteConfigToDir writes a site_config.yaml in the given config directory.
func writeSiteConfigToDir(configDir, hostname string, cfg *site.SiteConfig) error {
	siteDir := fmt.Sprintf("%s/sites/%s", configDir, hostname)
	if err := os.MkdirAll(siteDir, 0755); err != nil {
		return err
	}
	filesDir := fmt.Sprintf("%s/files", siteDir)
	if err := os.MkdirAll(filesDir, 0755); err != nil {
		return err
	}

	content := fmt.Sprintf(`# Site configuration for %s
db_host: %s
db_port: %d
db_name: %s
db_user: %s
db_password: %s

redis_url: redis://localhost:6379/0

file_storage: local
files_path: sites/%s/files

apps:
  - core

hostname: %s
`, hostname, cfg.DBHost, cfg.DBPort, cfg.DBName, cfg.DBUser, cfg.DBPassword, hostname, hostname)

	return os.WriteFile(fmt.Sprintf("%s/site_config.yaml", siteDir), []byte(content), 0644)
}
