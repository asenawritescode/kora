package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/schema"
)

// --- Auth Providers ---

// HandleAuthProviders returns enabled authentication providers.
// Public endpoint — no auth required.
func (h *Handler) HandleAuthProviders(c *gin.Context) {
	c.JSON(http.StatusOK, Response{
		Data: map[string]any{
			"providers": []map[string]any{
				{"name": "password", "label": "Email & Password"},
			},
		},
	})
}

// --- System Doctype ---

// ReferenceInfo describes a doctype that links to the current doctype via a Link field.
type ReferenceInfo struct {
	Doctype   string `json:"doctype"`
	Fieldname string `json:"fieldname"`
	Label     string `json:"label"`
}

// SystemDoctypeResponse is the full schema response for a single DocType.
type SystemDoctypeResponse struct {
	DocType      *doctype.DocType              `json:"doctype"`
	Workflow     *WorkflowResponse              `json:"workflow,omitempty"`
	Permissions  map[string]bool               `json:"permissions"`
	Transitions  []doctype.WorkflowTransition  `json:"transitions,omitempty"`
	ReferencedBy []ReferenceInfo               `json:"referenced_by,omitempty"`
}

// WorkflowResponse holds the workflow definition for a DocType.
type WorkflowResponse struct {
	States      []doctype.WorkflowState      `json:"states"`
	Transitions []doctype.WorkflowTransition `json:"transitions"`
	StateField  string                       `json:"state_field"`
}

// HandleSystemDoctype returns the full DocType schema with workflow and permissions.
// GET /api/system/doctype/:doctype
// Optional query param: ?format=yaml to get raw YAML output.
// Optional query param: ?state=current_state to get available transitions.
func (h *Handler) HandleSystemDoctype(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)
	dt := reg.Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "DocType not found: " + doctypeName},
		})
		return
	}

	// YAML export format.
	if c.Query("format") == "yaml" {
		yamlBytes, err := yaml.Marshal(dt)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Failed to serialize YAML"},
			})
			return
		}
		c.Data(http.StatusOK, "text/yaml; charset=utf-8", yamlBytes)
		return
	}

	resp := SystemDoctypeResponse{
		DocType:     dt,
		Permissions: getUserPermissions(reg, c, doctypeName),
	}

	// Compute which doctypes link to this one (back-references).
	resp.ReferencedBy = findReferencingDoctypes(reg, doctypeName)

	// Attach workflow data if this doctype has one.
	if reg.Workflows.Has(doctypeName) {
		wf := reg.Workflows.Get(doctypeName)
		resp.Workflow = &WorkflowResponse{
			States:      wf.States,
			Transitions: wf.Transitions,
			StateField:  wf.WorkflowStateField,
		}

		// Compute available transitions for the current state and user.
		currentState := c.Query("state")
		if currentState != "" {
			userRole := c.GetString("user_role")
			if userRole == "" {
				userRole = doctype.AdminRole
			}
			doc := doctype.NewDocument(doctypeName)
			for key, vals := range c.Request.URL.Query() {
				if key != "state" && len(vals) > 0 {
					doc.Set(key, vals[0])
				}
			}
			resp.Transitions = reg.Workflows.GetAvailableTransitions(doctypeName, currentState, userRole, doc)
		}
	}

	c.JSON(http.StatusOK, Response{Data: resp})
}

// getUserPermissions returns a map of operation → allowed for the current user on a doctype.
func getUserPermissions(reg *doctype.Registry, c *gin.Context, dt string) map[string]bool {
	userRoles := c.GetStringSlice("user_roles")
	// If no roles set, return full access (bootstrapping / system user).
	if len(userRoles) == 0 {
		return map[string]bool{
			"read": true, "write": true, "create": true, "delete": true,
			"submit": true, "cancel": true, "amend": true,
			"export": true, "import": true, "report": true,
		}
	}
	ops := []string{"read", "write", "create", "delete", "submit", "cancel", "amend", "export", "import", "report"}
	perms := make(map[string]bool, len(ops))
	for _, op := range ops {
		allowed, _ := reg.CanUser(userRoles, dt, op)
		perms[op] = allowed
	}
	return perms
}

// --- System Navigation ---

// NavigationResponse is the full navigation config for the SPA sidebar.
type NavigationResponse struct {
	Modules  []ModuleGroup `json:"modules"`
	Branding Branding      `json:"branding"`
	User     UserInfo      `json:"user"`
}

// ModuleGroup is a group of DocTypes under a module.
type ModuleGroup struct {
	Module   string           `json:"module"`
	Label    string           `json:"label"`
	DocTypes []DocTypeNavItem `json:"doctypes"`
}

// DocTypeNavItem is a single DocType entry in the navigation.
type DocTypeNavItem struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Icon    string `json:"icon,omitempty"`
	IsChild bool   `json:"is_child"`
}

// AppBranding is the global branding config (set from common config at startup).
var AppBranding = Branding{AppName: "Kora", PrimaryColor: "#2563eb"}

// Branding holds per-site branding configuration.
type Branding struct {
	AppName      string `json:"app_name"`
	PrimaryColor string `json:"primary_color"`
}

// UserInfo is the current user's public info for the UI.
type UserInfo struct {
	Name     string   `json:"name"`
	FullName string   `json:"full_name"`
	Email    string   `json:"email"`
	Roles    []string `json:"roles"`
}

// HandleSystemNavigation returns the navigation config (sidebar, branding, user).
// GET /api/system/navigation
func (h *Handler) HandleSystemNavigation(c *gin.Context) {
	reg := h.siteRegistry(c)
	doctypes := reg.All()

	// Group by module, skip child tables.
	moduleMap := make(map[string][]DocTypeNavItem)
	for _, dt := range doctypes {
		if dt.IsChildTable {
			continue
		}
		module := dt.Module
		if module == "" {
			module = "System"
		}
		moduleMap[module] = append(moduleMap[module], DocTypeNavItem{
			Name:    dt.Name,
			Label:   dt.Name,
			IsChild: false,
		})
	}

	// Sort modules deterministically.
	moduleNames := make([]string, 0, len(moduleMap))
	for m := range moduleMap {
		moduleNames = append(moduleNames, m)
	}
	sort.Strings(moduleNames)

	var modules []ModuleGroup
	for _, m := range moduleNames {
		items := moduleMap[m]
		// Sort doctypes within module.
		sort.Slice(items, func(i, j int) bool {
			return items[i].Label < items[j].Label
		})
		modules = append(modules, ModuleGroup{
			Module:   m,
			Label:    m,
			DocTypes: items,
		})
	}

	// Extract user info from context (set by SiteGuard/AuthMiddleware).
	user := UserInfo{}
	if userObj, exists := c.Get("user_obj"); exists {
		if u, ok := userObj.(*auth.User); ok {
			user.Name = u.Name
			user.FullName = u.FullName
			user.Email = u.Email
			user.Roles = u.Roles
		}
	}
	// Fallback: read individual context values.
	if user.Name == "" {
		user.Name = c.GetString("user")
	}
	if len(user.Roles) == 0 {
		user.Roles = c.GetStringSlice("user_roles")
	}
	if user.Email == "" {
		user.Email = c.GetString("user_email")
		if user.Email == "" {
			user.Email = c.GetString("user")
		}
	}
	if user.FullName == "" {
		user.FullName = c.GetString("user_full_name")
		if user.FullName == "" {
			user.FullName = user.Name
		}
	}

	branding := AppBranding

	c.JSON(http.StatusOK, Response{
		Data: NavigationResponse{
			Modules:  modules,
			Branding: branding,
			User:     user,
		},
	})
}

// findReferencingDoctypes returns a list of doctypes that have Link fields pointing to targetDoctype.
func findReferencingDoctypes(reg *doctype.Registry, targetDoctype string) []ReferenceInfo {
	var refs []ReferenceInfo
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		for _, f := range dt.Fields {
			if (f.Fieldtype == "Link" || f.Fieldtype == "Dynamic Link") && f.Options == targetDoctype {
				refs = append(refs, ReferenceInfo{
					Doctype:   dt.Name,
					Fieldname: f.Fieldname,
					Label:     f.Label,
				})
			}
		}
	}
	return refs
}

// --- Doctype List (admin) ---

// HandleSystemDoctypes returns a flat list of all DocTypes.
// GET /api/system/doctypes
func (h *Handler) HandleSystemDoctypes(c *gin.Context) {
	reg := h.siteRegistry(c)
	doctypes := reg.All()

	// Filter out child tables for the admin list.
	var result []*doctype.DocType
	for _, dt := range doctypes {
		if !dt.IsChildTable {
			result = append(result, dt)
		}
	}

	c.JSON(http.StatusOK, Response{Data: result})
}

// --- Doctype Create ---

// HandleSystemDoctypeCreate creates a new DocType from JSON body.
// POST /api/system/doctype?activate=true|false
func (h *Handler) HandleSystemDoctypeCreate(c *gin.Context) {
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB

	var dt doctype.DocType
	if err := c.ShouldBindJSON(&dt); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format: " + err.Error()},
		})
		return
	}

	// Validate the doctype.
	if err := dt.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Check for duplicate.
	if reg.Has(dt.Name) {
		c.JSON(http.StatusConflict, ErrorResponse{
			Error: map[string]string{"message": "DocType already exists: " + dt.Name},
		})
		return
	}

	// Save to configstore.
	store := configstore.NewStore(db)
	if err := store.SaveDocType(&dt); err != nil {
		internalError(c, "saving doctype", err)
		return
	}

	// Register in runtime registry.
	reg.Register(&dt)

	// Auto-create default permissions for all existing roles so non-admin users
	// can immediately access the new doctype (read, write, create).
	if err := store.AutoCreatePermissionsForDoctype(dt.Name); err != nil {
		slog.Warn("failed to auto-create permissions for new doctype", "doctype", dt.Name, "error", err)
	} else {
		// Reload permissions into the in-memory registry so they take effect immediately.
		roles, err := store.LoadRoles()
		if err == nil {
			perms, err2 := store.LoadPermissions()
			if err2 == nil {
				reg.Permissions.LoadPermissionsFromDB(roles, perms)
			}
		}
	}

	// Determine if we should activate immediately.
	activate := c.Query("activate") != "false"
	status := "Draft"
	if activate {
		status = "Active"
	}

	// Run migration if activating (use connected database name).
	if activate {
		var dbName string
		db.QueryRow("SELECT DATABASE()").Scan(&dbName)
		if err := schema.MigrateSite(db, dbName, reg); err != nil {
			slog.Error("migration failed after doctype create", "doctype", dt.Name, "error", err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Schema migration failed: " + err.Error()},
			})
			return
		}
	}

	// Create config version.
	allDoctypes := collectDoctypes(reg)
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	versionID, versionNum, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Created "+dt.Name+" via web", status, allDoctypes,
	)
	if err != nil {
		slog.Warn("failed to create config version", "error", err)
	}

	code := http.StatusCreated
	if !activate {
		code = http.StatusOK
	}
	c.JSON(code, Response{
		Data: map[string]any{
			"doctype":       dt,
			"version_id":    versionID,
			"version_num":   versionNum,
			"status":        status,
		},
	})
}

// --- Doctype Update ---

// HandleSystemDoctypeUpdate updates an existing DocType.
// PUT /api/system/doctype/:doctype?activate=true|false
func (h *Handler) HandleSystemDoctypeUpdate(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB

	oldDT := reg.Get(doctypeName)
	if oldDT == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "DocType not found: " + doctypeName},
		})
		return
	}

	var newDT doctype.DocType
	if err := c.ShouldBindJSON(&newDT); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format: " + err.Error()},
		})
		return
	}

	// Name must match.
	newDT.Name = doctypeName

	// Validate.
	if err := newDT.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Save.
	store := configstore.NewStore(db)
	if err := store.SaveDocType(&newDT); err != nil {
		internalError(c, "saving doctype", err)
		return
	}

	// Update registry (replace old with new).
	reg.Register(&newDT)

	// Activate?
	activate := c.Query("activate") != "false"
	status := "Draft"
	if activate {
		status = "Active"
		// Get DB name from the connection.
		var dbName string
		db.QueryRow("SELECT DATABASE()").Scan(&dbName)
		if err := schema.MigrateSite(db, dbName, reg); err != nil {
			slog.Error("migration failed after doctype update", "doctype", doctypeName, "error", err)
		}
	}

	// Create config version.
	allDoctypes := collectDoctypes(reg)
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	versionID, versionNum, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Updated "+doctypeName+" via web", status, allDoctypes,
	)
	if err != nil {
		slog.Warn("failed to create config version", "error", err)
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]any{
			"doctype":     newDT,
			"version_id":  versionID,
			"version_num": versionNum,
			"status":      status,
		},
	})
}

// --- Doctype Delete ---

// HandleSystemDoctypeDelete removes a DocType configuration.
// DELETE /api/system/doctype/:doctype
func (h *Handler) HandleSystemDoctypeDelete(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB

	if !reg.Has(doctypeName) {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "DocType not found: " + doctypeName},
		})
		return
	}

	// Delete from config tables.
	store := configstore.NewStore(db)
	if _, err := db.Exec("DELETE FROM _kora_field WHERE parent = ?", doctypeName); err != nil {
		internalError(c, "deleting fields", err)
		return
	}
	if _, err := db.Exec("DELETE FROM _kora_doctype WHERE name = ?", doctypeName); err != nil {
		internalError(c, "deleting doctype", err)
		return
	}

	// Remove from registry.
	reg.Remove(doctypeName)

	// Create config version recording the deletion.
	allDoctypes := collectDoctypes(reg)
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	_, _, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Deleted "+doctypeName+" via web", "Active", allDoctypes,
	)
	if err != nil {
		slog.Warn("failed to create config version", "error", err)
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "deleted"}})
}

// --- Doctype Validate ---

// HandleSystemDoctypeValidate validates a DocType JSON or YAML body without saving.
// POST /api/system/doctype/validate
// Accepts JSON (Content-Type: application/json) or YAML (Content-Type: application/x-yaml).
// Returns structured errors with line numbers for unknown keys and validation issues.
func (h *Handler) HandleSystemDoctypeValidate(c *gin.Context) {
	ct := c.GetHeader("Content-Type")

	// If YAML, use strict validation with line-numbered errors.
	if ct == "application/x-yaml" || ct == "text/yaml" || ct == "application/yaml" {
		body, err := c.GetRawData()
		if err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": "Failed to read request body"},
			})
			return
		}
		syntaxErrs, validationErrs, err := doctype.ValidateYAML(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": err.Error()},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"valid":       len(syntaxErrs) == 0 && len(validationErrs) == 0,
			"syntax":      syntaxErrs,
			"validations": validationErrs,
		})
		return
	}

	// JSON input — use existing flow.
	var dt doctype.DocType
	if err := c.ShouldBindJSON(&dt); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format: " + err.Error()},
		})
		return
	}

	if err := dt.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	c.JSON(http.StatusOK, Response{Data: &dt})
}

// --- Doctype Dry-Run ---

// HandleSystemDoctypeDryRun returns the impact analysis for a proposed doctype change.
// POST /api/system/doctype/dry-run
func (h *Handler) HandleSystemDoctypeDryRun(c *gin.Context) {
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB

	var proposed doctype.DocType
	if err := c.ShouldBindJSON(&proposed); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format: " + err.Error()},
		})
		return
	}

	if err := proposed.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Get old doctype (if exists) for comparison.
	var oldDT *doctype.DocType
	if reg.Has(proposed.Name) {
		oldDT = reg.Get(proposed.Name)
	}

	// Run impact analysis.
	preview := schema.AnalyzeImpact(db, oldDT, &proposed, reg)

	c.JSON(http.StatusOK, Response{Data: preview})
}

// --- Doctype References ---

// HandleSystemDoctypeReferences returns other doctypes that link to the given doctype.
// GET /api/system/doctype/:doctype/references
func (h *Handler) HandleSystemDoctypeReferences(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)

	if !reg.Has(doctypeName) {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "DocType not found: " + doctypeName},
		})
		return
	}

	refs := findReferencingDoctypes(reg, doctypeName)
	c.JSON(http.StatusOK, Response{Data: refs})
}

// --- Config Version Actions ---

// HandleConfigVersionActivate activates a Draft version.
// POST /api/system/config/versions/:id/activate
func (h *Handler) HandleConfigVersionActivate(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)

	// Get the version's config snapshot.
	var configJSON, siteName string
	var currentStatus string
	err := db.QueryRow(
		"SELECT config, site, status FROM _kora_config_version WHERE id = ?", versionID,
	).Scan(&configJSON, &siteName, &currentStatus)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Version not found"},
		})
		return
	}

	if currentStatus != "Draft" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Only Draft versions can be activated"},
		})
		return
	}

	// Parse the config snapshot.
	var doctypes []*doctype.DocType
	if err := json.Unmarshal([]byte(configJSON), &doctypes); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config"},
		})
		return
	}

	// Reload all doctypes from config version into the store + registry.
	store := configstore.NewStore(db)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			internalError(c, "saving doctype from version", err)
			return
		}
	}

	// Rebuild registry.
	roles, _ := store.LoadRoles()
	permissions, _ := store.LoadPermissions()
	reg.LoadFull(doctypes, roles, permissions)

	// Run migration.
	var dbName string
	db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	if err := schema.MigrateSite(db, dbName, reg); err != nil {
		slog.Error("migration failed on version activate", "version", versionID, "error", err)
	}

	// Create new Active version.
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	_, _, err = store.CreateConfigVersion(siteName, createdBy, "Activated version "+versionID, "Active", doctypes)
	if err != nil {
		slog.Warn("failed to create active version", "error", err)
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "activated", "status": "Active"}})
}

// HandleConfigVersionDiscard discards a Draft version.
// POST /api/system/config/versions/:id/discard
func (h *Handler) HandleConfigVersionDiscard(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB

	var currentStatus string
	err := db.QueryRow("SELECT status FROM _kora_config_version WHERE id = ?", versionID).Scan(&currentStatus)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Version not found"},
		})
		return
	}

	if currentStatus != "Draft" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Only Draft versions can be discarded"},
		})
		return
	}

	if _, err := db.Exec("UPDATE _kora_config_version SET status = 'Superseded' WHERE id = ?", versionID); err != nil {
		internalError(c, "discarding version", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "discarded", "status": "Superseded"}})
}

// HandleConfigVersionRollback activates a Superseded version (rollback).
// POST /api/system/config/versions/:id/rollback
func (h *Handler) HandleConfigVersionRollback(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)

	var configJSON, siteName, currentStatus string
	err := db.QueryRow(
		"SELECT config, site, status FROM _kora_config_version WHERE id = ?", versionID,
	).Scan(&configJSON, &siteName, &currentStatus)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Version not found"},
		})
		return
	}

	if currentStatus != "Superseded" && currentStatus != "Active" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Only Superseded or Active versions can be rolled back to"},
		})
		return
	}

	// Parse and apply.
	var doctypes []*doctype.DocType
	if err := json.Unmarshal([]byte(configJSON), &doctypes); err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config"},
		})
		return
	}

	store := configstore.NewStore(db)
	for _, dt := range doctypes {
		if err := store.SaveDocType(dt); err != nil {
			internalError(c, "saving during rollback", err)
			return
		}
	}

	roles, _ := store.LoadRoles()
	permissions, _ := store.LoadPermissions()
	reg.LoadFull(doctypes, roles, permissions)

	var dbName string
	db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	schema.MigrateSite(db, dbName, reg)

	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	store.CreateConfigVersion(siteName, createdBy, "Rollback to version "+versionID, "Active", doctypes)

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "rolled back", "status": "Active"}})
}

// --- Config Import (YAML upload) ---

// HandleConfigImport imports a YAML config file and returns parsed DocType JSON.
// POST /api/system/config/import
func (h *Handler) HandleConfigImport(c *gin.Context) {
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "No file provided"},
		})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Failed to read file"},
		})
		return
	}

	// Parse as YAML.
	var dt doctype.DocType
	if err := yaml.Unmarshal(data, &dt); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid YAML: " + err.Error()},
		})
		return
	}

	if err := dt.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Validation failed: " + err.Error()},
		})
		return
	}

	c.JSON(http.StatusOK, Response{Data: &dt})
}

// --- Helpers ---

// collectDoctypes returns all non-child doctypes from the registry as a slice.
func collectDoctypes(reg *doctype.Registry) []*doctype.DocType {
	var result []*doctype.DocType
	for _, dt := range reg.All() {
		if !dt.IsChildTable {
			result = append(result, dt)
		}
	}
	return result
}

// --- Roles ---

// HandleSystemRoles returns all roles.
// GET /api/system/roles
func (h *Handler) HandleSystemRoles(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db)
	roles, err := store.LoadRoles()
	if err != nil {
		internalError(c, "loading roles", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: roles})
}

// HandleSystemRoleCreate creates a new role.
// POST /api/system/roles
func (h *Handler) HandleSystemRoleCreate(c *gin.Context) {
	db := h.siteTx(c).DB
	var role doctype.Role
	if err := c.ShouldBindJSON(&role); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	if role.Name == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Role name is required"}})
		return
	}
	store := configstore.NewStore(db)
	if err := store.SaveRoles([]*doctype.Role{&role}); err != nil {
		internalError(c, "saving role", err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: &role})
}

// HandleSystemRoleUpdate updates an existing role.
// PUT /api/system/roles/:name
func (h *Handler) HandleSystemRoleUpdate(c *gin.Context) {
	db := h.siteTx(c).DB
	roleName := c.Param("name")
	var role doctype.Role
	if err := c.ShouldBindJSON(&role); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	role.Name = roleName
	store := configstore.NewStore(db)
	if err := store.SaveRoles([]*doctype.Role{&role}); err != nil {
		internalError(c, "saving role", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: &role})
}

// HandleSystemRoleDelete deletes a role.
// DELETE /api/system/roles/:name
func (h *Handler) HandleSystemRoleDelete(c *gin.Context) {
	db := h.siteTx(c).DB
	roleName := c.Param("name")
	// Check if any users have this role.
	var userCount int
	db.QueryRow("SELECT COUNT(*) FROM _kora_user WHERE FIND_IN_SET(?, REPLACE(roles, ' ', ''))", roleName).Scan(&userCount)
	if _, err := db.Exec("DELETE FROM _kora_role WHERE name = ?", roleName); err != nil {
		internalError(c, "deleting role", err)
		return
	}
	if _, err := db.Exec("DELETE FROM _kora_permission WHERE role = ?", roleName); err != nil {
		internalError(c, "deleting role permissions", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]any{"message": "deleted", "users_with_role": userCount}})
}

// --- Permissions ---

// HandleSystemPermissions returns all permissions.
// GET /api/system/permissions
func (h *Handler) HandleSystemPermissions(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db)
	permissions, err := store.LoadPermissions()
	if err != nil {
		internalError(c, "loading permissions", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: permissions})
}

// HandleSystemPermissionsSave replaces all permissions.
// PUT /api/system/permissions
func (h *Handler) HandleSystemPermissionsSave(c *gin.Context) {
	db := h.siteTx(c).DB
	var permissions []*doctype.Permission
	if err := c.ShouldBindJSON(&permissions); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	store := configstore.NewStore(db)
	if err := store.SavePermissions(permissions); err != nil {
		internalError(c, "saving permissions", err)
		return
	}
	// Reload into registry.
	reg := h.siteRegistry(c)
	roles, _ := store.LoadRoles()
	reg.Permissions.LoadPermissionsFromDB(roles, permissions)
	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "saved"}})
}

// --- Workflows ---

// HandleSystemWorkflows returns all workflows.
// GET /api/system/workflows
func (h *Handler) HandleSystemWorkflows(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db)
	workflows, err := store.LoadWorkflows()
	if err != nil {
		internalError(c, "loading workflows", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: workflows})
}

// HandleSystemWorkflowByDoctype returns the workflow for a specific doctype.
// GET /api/system/workflows/:doctype
func (h *Handler) HandleSystemWorkflowByDoctype(c *gin.Context) {
	reg := h.siteRegistry(c)
	doctypeName := c.Param("doctype")
	wf := reg.Workflows.Get(doctypeName)
	if wf == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "No workflow for " + doctypeName}})
		return
	}
	c.JSON(http.StatusOK, Response{Data: wf})
}

// HandleSystemWorkflowSave creates or updates a workflow.
// POST /api/system/workflows
func (h *Handler) HandleSystemWorkflowSave(c *gin.Context) {
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)
	var wf doctype.Workflow
	if err := c.ShouldBindJSON(&wf); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request"}})
		return
	}
	if wf.DocumentType == "" || wf.Name == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "name and document_type are required"}})
		return
	}
	store := configstore.NewStore(db)
	if err := store.SaveWorkflows([]*doctype.Workflow{&wf}); err != nil {
		internalError(c, "saving workflow", err)
		return
	}
	// Register in runtime.
	if wf.IsActive {
		reg.Workflows.Register(&wf)
	}
	c.JSON(http.StatusOK, Response{Data: &wf})
}

// HandleSystemWorkflowDelete removes a workflow for a doctype.
// DELETE /api/system/workflows/:doctype
func (h *Handler) HandleSystemWorkflowDelete(c *gin.Context) {
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)
	doctypeName := c.Param("doctype")
	if _, err := db.Exec("DELETE FROM _kora_workflow WHERE document_type = ?", doctypeName); err != nil {
		internalError(c, "deleting workflow", err)
		return
	}
	if _, err := db.Exec("DELETE FROM _kora_workflow_state WHERE workflow IN (SELECT name FROM _kora_workflow WHERE document_type = ?)", doctypeName); err != nil {
		slog.Warn("workflow state cleanup", "error", err)
	}
	if _, err := db.Exec("DELETE FROM _kora_workflow_transition WHERE workflow IN (SELECT name FROM _kora_workflow WHERE document_type = ?)", doctypeName); err != nil {
		slog.Warn("workflow transition cleanup", "error", err)
	}
	reg.Workflows.Remove(doctypeName)
	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "deleted"}})
}

// RegisterSystemRoutes registers system endpoints on the given API group.
func RegisterSystemRoutes(apiGroup *gin.RouterGroup, handler *Handler) {
	system := apiGroup.Group("/system")
	{
		// Read endpoints.
		system.GET("/doctype/:doctype", handler.HandleSystemDoctype)
		system.GET("/doctypes", handler.HandleSystemDoctypes)
		system.GET("/doctype/:doctype/references", handler.HandleSystemDoctypeReferences)
		system.GET("/navigation", handler.HandleSystemNavigation)

		// Write endpoints.
		system.POST("/doctype/validate", handler.HandleSystemDoctypeValidate)
		system.POST("/doctype/dry-run", handler.HandleSystemDoctypeDryRun)
		system.POST("/doctype", handler.HandleSystemDoctypeCreate)
		system.PUT("/doctype/:doctype", handler.HandleSystemDoctypeUpdate)
		system.DELETE("/doctype/:doctype", handler.HandleSystemDoctypeDelete)

		// Config version actions.
		system.POST("/config/versions/:id/activate", handler.HandleConfigVersionActivate)
		system.POST("/config/versions/:id/discard", handler.HandleConfigVersionDiscard)
		system.POST("/config/versions/:id/rollback", handler.HandleConfigVersionRollback)

		// Config import.
		system.POST("/config/import", handler.HandleConfigImport)

		// Roles.
		system.GET("/roles", handler.HandleSystemRoles)
		system.POST("/roles", handler.HandleSystemRoleCreate)
		system.PUT("/roles/:name", handler.HandleSystemRoleUpdate)
		system.DELETE("/roles/:name", handler.HandleSystemRoleDelete)

		// Permissions.
		system.GET("/permissions", handler.HandleSystemPermissions)
		system.PUT("/permissions", handler.HandleSystemPermissionsSave)

		// Workflows.
		system.GET("/workflows", handler.HandleSystemWorkflows)
		system.GET("/workflows/:doctype", handler.HandleSystemWorkflowByDoctype)
		system.POST("/workflows", handler.HandleSystemWorkflowSave)
		system.DELETE("/workflows/:doctype", handler.HandleSystemWorkflowDelete)

		// Users.
		system.GET("/users", handler.HandleUserList)
		system.POST("/users", handler.HandleUserCreate)
		system.GET("/users/:name", handler.HandleUserGet)
		system.PUT("/users/:name", handler.HandleUserUpdate)
		system.DELETE("/users/:name", handler.HandleUserDelete)
		system.POST("/users/:name/reset-password", handler.HandleUserResetPassword)

		// Secrets.
		system.GET("/secrets", handler.HandleSecretList)
		system.POST("/secrets", handler.HandleSecretSet)
		system.DELETE("/secrets/:key", handler.HandleSecretDelete)
	}
}
