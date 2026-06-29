package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/analytics"
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
	Status       string                        `json:"status"` // "Active" or "Draft"
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

	// Determine if this doctype is Active (table exists) or Draft (config only).
	db := h.siteTx(c).DB
	var dbName string
	_ = db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	resp.Status = "Draft"
	if liveSchema, err := h.TxManager.Dialect.LoadSchema(db, dbName); err == nil && liveSchema != nil {
		if _, ok := liveSchema.Tables["tab"+doctypeName]; ok {
			resp.Status = "Active"
		}
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

// doctypeWithStatus wraps a DocType with its activation status.
type doctypeWithStatus struct {
	*doctype.DocType
	Status string `json:"status"` // "Active" or "Draft"
}

// HandleSystemDoctypes returns a flat list of all DocTypes with status.
// GET /api/system/doctypes
func (h *Handler) HandleSystemDoctypes(c *gin.Context) {
	reg := h.siteRegistry(c)
	doctypes := reg.All()

	// Determine table existence so we can show Active vs Draft status.
	db := h.siteTx(c).DB
	var dbName string
	_ = db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	tableExists := make(map[string]bool)
	if liveSchema, err := h.TxManager.Dialect.LoadSchema(db, dbName); err == nil && liveSchema != nil {
		for tableName := range liveSchema.Tables {
			// Table names are like "tabProduct" — strip the "tab" prefix.
			tableExists[strings.TrimPrefix(tableName, "tab")] = true
		}
	}

	// Filter out child tables for the admin list.
	var result []doctypeWithStatus
	for _, dt := range doctypes {
		if !dt.IsChildTable {
			status := "Draft"
			if tableExists[dt.Name] {
				status = "Active"
			}
			result = append(result, doctypeWithStatus{DocType: dt, Status: status})
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

	// Determine if we should activate immediately.
	activate := c.Query("activate") != "false"

	store := configstore.NewStore(db, h.TxManager.Dialect)

	if activate {
		// Activate immediately: save to DB, register, create permissions, run migration.
		if err := store.SaveDocType(&dt, c.GetString("site_name")); err != nil {
			internalError(c, "saving doctype", err)
			return
		}
		reg.Register(&dt)

		if err := store.AutoCreatePermissionsForDoctype(dt.Name, c.GetString("site_name")); err != nil {
			slog.Warn("failed to auto-create permissions for new doctype", "doctype", dt.Name, "error", err)
		} else {
			roles, err := store.LoadRoles(c.GetString("site_name"))
			if err == nil {
				perms, err2 := store.LoadPermissions(c.GetString("site_name"))
				if err2 == nil {
					reg.Permissions.LoadPermissionsFromDB(roles, perms)
				}
			}
		}

		var dbName string
		db.QueryRow("SELECT DATABASE()").Scan(&dbName)
		if err := schema.MigrateSiteFromRegistry(db, dbName, reg, h.TxManager.Dialect); err != nil {
			slog.Error("migration failed after doctype create", "doctype", dt.Name, "error", err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Schema migration failed: " + err.Error()},
			})
			return
		}
		h.invalidateAnalyticsForDoctype(c, dt.Name)
	} else {
		// Draft: register temporarily for snapshot collection, then remove.
		// Do NOT save to _kora_doctype. Do NOT run migration.
		// The doctype only exists in the config version snapshot until activation.
		reg.Register(&dt)
	}

	// Create config version.
	snapshot, _ := store.CollectSnapshot(reg, c.GetString("site_name"))

	if !activate {
		// Remove from runtime — Draft doctypes are not live.
		reg.Unregister(dt.Name)
	}

	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	status := "Draft"
	if activate {
		status = "Active"
	}
	versionID, versionNum, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Created "+dt.Name+" via web", status, snapshot,
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
		if err := store.SaveDocType(&newDT, c.GetString("site_name")); err != nil {
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
		if err := schema.MigrateSiteFromRegistry(db, dbName, reg, h.TxManager.Dialect); err != nil {
			slog.Error("migration failed after doctype update", "doctype", doctypeName, "error", err)
		}
		// Invalidate analytics worker — field changes mean regenerated metrics.
		h.invalidateAnalyticsForDoctype(c, doctypeName)
	}

	// Create config version.
	snapshot, _ := store.CollectSnapshot(reg, c.GetString("site_name"))
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	versionID, versionNum, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Updated "+doctypeName+" via web", status, snapshot,
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
// DELETE /api/system/doctype/:doctype?cleanup=config|full
//   cleanup=config (default): Delete config rows only. Business tables, analytics, permissions survive.
//   cleanup=full: Full cleanup — also deletes analytics, permissions, workflows, and clears Link fields.
//   cleanup=none: Soft delete — only remove from registry.
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

	cleanup := c.Query("cleanup")
	if cleanup == "" {
		cleanup = "config" // default: current behavior
	}

	// Delete from config tables (always).
	store := configstore.NewStore(db, h.TxManager.Dialect)
	if _, err := db.Exec("DELETE FROM _kora_field WHERE parent = ?", doctypeName); err != nil {
		internalError(c, "deleting fields", err)
		return
	}
	if _, err := db.Exec("DELETE FROM _kora_doctype WHERE name = ?", doctypeName); err != nil {
		internalError(c, "deleting doctype", err)
		return
	}

	// Full cleanup: also clean analytics, permissions, workflows, and dangling Link fields.
	if cleanup == "full" {
		// Clean analytics rollup tables.
		for _, table := range []string{
			"_kora_analytics_daily",
			"_kora_analytics_monthly",
			"_kora_analytics_workflow",
			"_kora_analytics_events",
			"_kora_analytics_metric",
		} {
			if _, err := db.Exec(fmt.Sprintf("DELETE FROM %s WHERE doctype = ?", table), doctypeName); err != nil {
				slog.Warn("analytics cleanup failed", "table", table, "doctype", doctypeName, "error", err)
			}
		}

		// Clean permissions.
		if _, err := db.Exec("DELETE FROM _kora_permission WHERE doctype = ?", doctypeName); err != nil {
			slog.Warn("permission cleanup failed", "doctype", doctypeName, "error", err)
		}

		// Clean workflows.
		if _, err := db.Exec("DELETE FROM _kora_workflow WHERE document_type = ?", doctypeName); err != nil {
			slog.Warn("workflow cleanup failed", "doctype", doctypeName, "error", err)
		}

		// Clear dangling Link fields in OTHER doctypes that pointed to this one.
		if _, err := db.Exec("UPDATE _kora_field SET options = '' WHERE fieldtype = 'Link' AND options = ?", doctypeName); err != nil {
			slog.Warn("link field cleanup failed", "doctype", doctypeName, "error", err)
		}

		slog.Info("full doctype cleanup complete", "doctype", doctypeName)
	}

	// Remove from registry.
	reg.Remove(doctypeName)

	// Invalidate analytics worker metrics cache.
	h.invalidateAnalyticsForDoctype(c, doctypeName)

	// Create config version recording the deletion.
	snapshot, _ := store.CollectSnapshot(reg, c.GetString("site_name"))
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	_, _, err := store.CreateConfigVersion(
		c.GetString("site_name"), createdBy, "Deleted "+doctypeName+" via web", "Active", snapshot,
	)
	if err != nil {
		slog.Warn("failed to create config version", "error", err)
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "deleted", "cleanup": cleanup}})
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
	preview := schema.AnalyzeImpact(db, oldDT, &proposed, reg, h.TxManager.Dialect)

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

// HandleConfigVersionPreview returns a preview of what activating a version will change.
// GET /api/system/config/versions/:id/preview
func (h *Handler) HandleConfigVersionPreview(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)

	var configJSON, siteName, currentStatus, changeList string
	err := db.QueryRow(
		"SELECT config, site, status, COALESCE(change_list, '') FROM _kora_config_version WHERE id = ?", versionID,
	).Scan(&configJSON, &siteName, &currentStatus, &changeList)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Version not found"}})
		return
	}

	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to parse version config"}})
		return
	}

	// Check staleness.
	var newerActiveCount int
	db.QueryRow(
		"SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND version > (SELECT version FROM _kora_config_version WHERE id = ?) AND status = 'Active'",
		siteName, versionID,
	).Scan(&newerActiveCount)

	var preview map[string]any

	// If a stored change_list exists, use it directly (no re-computation).
	if changeList != "" {
		var fullDiff doctype.ConfigDiffFull
		if parseErr := json.Unmarshal([]byte(changeList), &fullDiff); parseErr == nil {
			preview = map[string]any{
				"version_id":                versionID,
				"status":                    currentStatus,
				"doctypes_in_snapshot":      len(snapshot.DocTypes),
				"roles_in_snapshot":         len(snapshot.Roles),
				"permissions_in_snapshot":   len(snapshot.Permissions),
				"workflows_in_snapshot":     len(snapshot.Workflows),
				"diff_summary":              fullDiff.Doctypes.Summary(),
				"is_breaking":               fullDiff.Doctypes.IsBreaking,
				"newer_active_versions":     newerActiveCount,
				"section_changes":           fullDiff.SectionChanges,
				"change_list_version":       "v1",
				"warning":                   "",
			}
			if newerActiveCount > 0 {
				preview["warning"] = fmt.Sprintf("Activating this version will REVERT %d newer active version(s). Changes made since this version was created will be lost.", newerActiveCount)
			}
			if fullDiff.Doctypes.IsBreaking {
				if preview["warning"] != "" {
					preview["warning"] = preview["warning"].(string) + " This version has BREAKING changes (field removals, type changes)."
				} else {
					preview["warning"] = "This version has BREAKING changes (field removals, type changes)."
				}
			}
			c.JSON(http.StatusOK, Response{Data: preview})
			return
		}
	}

	// Fallback: compute diff from current registry (for old versions without change_list).
	currentDoctypes := make([]*doctype.DocType, 0)
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			currentDoctypes = append(currentDoctypes, dt)
		}
	}
	diff := doctype.DiffConfigs(currentDoctypes, snapshot.DocTypes)

	preview = map[string]any{
		"version_id":             versionID,
		"status":                 currentStatus,
		"doctypes_in_snapshot":   len(snapshot.DocTypes),
		"roles_in_snapshot":      len(snapshot.Roles),
		"permissions_in_snapshot": len(snapshot.Permissions),
		"workflows_in_snapshot":  len(snapshot.Workflows),
		"diff_summary":           diff.Summary(),
		"is_breaking":            diff.IsBreaking,
		"newer_active_versions":  newerActiveCount,
		"change_list_version":    "legacy",
		"warning":               "",
	}
	if newerActiveCount > 0 {
		preview["warning"] = fmt.Sprintf("Activating this version will REVERT %d newer active version(s). Changes made since this version was created will be lost.", newerActiveCount)
	}
	if diff.IsBreaking {
		if preview["warning"] != "" {
			preview["warning"] = preview["warning"].(string) + " This version has BREAKING changes (field removals, type changes)."
		} else {
			preview["warning"] = "This version has BREAKING changes (field removals, type changes)."
		}
	}

	c.JSON(http.StatusOK, Response{Data: preview})
}

// HandleConfigVersionActivate activates a Draft version.
// POST /api/system/config/versions/:id/activate
func (h *Handler) HandleConfigVersionActivate(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)

	// Get the version's config snapshot, change_list, and base_version_id.
	var configJSON, siteName, currentStatus, changeList, baseVersionID, minKoraVersion string
	err := db.QueryRow(
		"SELECT config, site, status, COALESCE(change_list, ''), COALESCE(base_version_id, ''), COALESCE(min_kora_version, '') FROM _kora_config_version WHERE id = ?", versionID,
	).Scan(&configJSON, &siteName, &currentStatus, &changeList, &baseVersionID, &minKoraVersion)
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

	// min_kora_version check: block activation if the binary is too old.
	if minKoraVersion != "" && !doctype.MinVersionOK(BinaryVersion, minKoraVersion) {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{
				"message": fmt.Sprintf(
					"This config version requires kora %s or newer, but running %s. Upgrade the binary and try again.",
					minKoraVersion, BinaryVersion,
				),
			},
		})
		return
	}

	// Staleness check via base_version_id: ensure the draft was forked from the current active version.
	if baseVersionID != "" {
		var activeVersionID string
		err := db.QueryRow("SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' LIMIT 1", siteName).Scan(&activeVersionID)
		if err == nil && baseVersionID != activeVersionID {
			slog.Warn("stale draft -- base version mismatch", "draft_id", versionID, "base", baseVersionID, "active", activeVersionID)
			if c.Query("force") != "true" {
				c.JSON(http.StatusConflict, ErrorResponse{
					Error: map[string]any{
						"message": fmt.Sprintf("This draft was created from version %s but %s is now active. Re-save the draft to incorporate changes.", baseVersionID, activeVersionID),
					"conflict_type":     "stale_base_version",
					"base_version_id":   baseVersionID,
					"active_version_id": activeVersionID,
					},
				})
				return
			}
			slog.Warn("activating stale draft with force=true", "version", versionID)
		}
	}

	// Additional staleness check: warn if newer versions have been activated since this Draft was created.
	var newerActiveCount int
	db.QueryRow(
		"SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND version > (SELECT version FROM _kora_config_version WHERE id = ?) AND status = 'Active'",
		siteName, versionID,
	).Scan(&newerActiveCount)
	if newerActiveCount > 0 {
		slog.Warn("activating stale draft", "version", versionID, "newer_active_versions", newerActiveCount)
	}

	// Parse the config snapshot with backward compatibility (old array vs new object format).
	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config: " + err.Error()},
		})
		return
	}

	store := configstore.NewStore(db, h.TxManager.Dialect)

	// Begin the activation transaction.
	// This wraps config writes in a single transaction for atomicity.
	// DDL is also applied through the transaction (benefits SQLite/LibSQL;
	// MySQL DDL auto-commits but ordering is still correct).
	tx, err := db.Begin()
	if err != nil {
		internalError(c, "beginning activation transaction", err)
		return
	}
	defer tx.Rollback() // no-op after successful commit

	// Compute schema changes before applying them (read-only, uses *sql.DB).
	// Prefer stored change_list for precise DDL generation; fall back to
	// live schema diff for legacy versions without a stored change_list.
	var ddlStatements []string
	var dbName string
	if h.TxManager.Dialect.DriverName() != "libsql" {
		db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	}

	if changeList != "" {
		var fullDiff doctype.ConfigDiffFull
		if parseErr := json.Unmarshal([]byte(changeList), &fullDiff); parseErr == nil {
			slog.Info("activation: generating DDL from stored change_list", "version", versionID)
			changes := doctype.ConvertConfigChanges(fullDiff.Doctypes.Changes, fullDiff.SectionChanges, snapshot)
			ddlStatements, _ = doctype.GenerateDDLFromDiff(changes, h.TxManager.Dialect)
			for _, stmt := range ddlStatements {
				slog.Info("activation DDL (change_list)", "sql", stmt)
			}
		} else {
			slog.Warn("activation: stored change_list parse failed, falling back to live schema diff",
				"version", versionID, "error", parseErr)
			changeList = ""
		}
	}

	if changeList == "" {
		liveSchema, schemaErr := schema.LoadLiveSchema(db, dbName, h.TxManager.Dialect)
		if schemaErr == nil && liveSchema != nil {
			schemaDiff := schema.ComputeDiff(snapshot.DocTypes, reg.Get, liveSchema, h.TxManager.Dialect)
			if !schemaDiff.IsEmpty() {
				ddlStatements = schemaDiff.GenerateDDL(snapshot.DocTypes, reg.Get, h.TxManager.Dialect)
				for _, stmt := range ddlStatements {
					slog.Info("activation DDL (live diff)", "sql", stmt)
				}
			}
		}
	}

	// Step 1: Save all config to DB within the transaction and rebuild the registry.
	if err := store.ActivateSnapshot(tx, snapshot, reg, siteName, h.TxManager.Dialect); err != nil {
		slog.Error("activation: config write failed — rolling back", "version", versionID, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Config write failed: " + err.Error()},
		})
		return
	}

	// Step 2: Apply DDL within the transaction.
	if len(ddlStatements) > 0 {
		if err := configstore.ApplyDDLTx(tx, ddlStatements); err != nil {
			slog.Error("activation: DDL failed — rolling back", "version", versionID, "error", err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Schema migration failed: " + err.Error()},
			})
			return
		}
	}

	// Step 3: Deactivate old Active versions inside the tx so it's atomic
	// with the config writes. If anything fails after, the rollback preserves
	// the previous Active.
	if _, err := tx.Exec("UPDATE _kora_config_version SET status = 'Superseded' WHERE site = ? AND status = 'Active'", siteName); err != nil {
		slog.Warn("failed to deactivate previous active", "error", err)
	}

	// Step 4: Commit the transaction.
	if err := tx.Commit(); err != nil {
		internalError(c, "committing activation transaction", err)
		return
	}

	// Invalidate analytics worker metrics cache — config activation may change
	// field types, add/remove fields, or change submittable status.
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.InvalidateAllMetrics()
	}

	// Migrate analytics rollup data for field renames and doctype changes.
	var prevConfigJSON string
	db.QueryRow("SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' AND id != ? ORDER BY version DESC LIMIT 1", siteName, versionID).Scan(&prevConfigJSON)
	if prevConfigJSON != "" {
		prevSnapshot, _ := doctype.ParseConfig(prevConfigJSON)
		if prevSnapshot != nil {
			analytics.MigrateRollupMetrics(db, siteName, prevSnapshot.DocTypes, snapshot.DocTypes)
		}
	}

	// Create new Active version reflecting the resulting state.
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	newSnapshot, _ := store.CollectSnapshot(reg, siteName)
	_, _, err = store.CreateConfigVersion(siteName, createdBy, "Activated version "+versionID, "Active", newSnapshot)
	if err != nil {
		slog.Warn("failed to create active version", "error", err)
	}

	// Mark the Draft version as Superseded — it's been replaced by the new Active.
	if _, err := db.Exec("UPDATE _kora_config_version SET status = 'Superseded' WHERE id = ?", versionID); err != nil {
		slog.Warn("failed to update draft status after activation", "version", versionID, "error", err)
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

// HandleConfigVersionRollbackPreview returns a preview of what rolling back to a version will change.
// GET /api/system/config/versions/:id/rollback-preview
func (h *Handler) HandleConfigVersionRollbackPreview(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB
	reg := h.siteRegistry(c)

	var configJSON, siteName, currentStatus string
	err := db.QueryRow(
		"SELECT config, site, status FROM _kora_config_version WHERE id = ?", versionID,
	).Scan(&configJSON, &siteName, &currentStatus)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Version not found"}})
		return
	}

	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to parse version config"}})
		return
	}

	// Compute what would change.
	currentDoctypes := make([]*doctype.DocType, 0)
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			currentDoctypes = append(currentDoctypes, dt)
		}
	}
	diff := doctype.DiffConfigs(currentDoctypes, snapshot.DocTypes)

	// Check if any doctypes in current registry would be REMOVED by this rollback.
	var wouldBeRemoved []string
	for _, c := range diff.Changes {
		if c.Type == doctype.ChangeDocTypeRemoved {
			wouldBeRemoved = append(wouldBeRemoved, c.DocType)
		}
	}

	preview := map[string]any{
		"version_id":            versionID,
		"status":                currentStatus,
		"doctypes_in_snapshot":  len(snapshot.DocTypes),
		"diff_summary":          diff.Summary(),
		"is_breaking":           diff.IsBreaking,
		"would_remove_doctypes": wouldBeRemoved,
		"changes":               len(diff.Changes),
	}
	if len(wouldBeRemoved) > 0 {
		preview["warning"] = fmt.Sprintf("Rolling back will REMOVE these doctypes from the registry: %s. Their tables will be orphaned in the database.", strings.Join(wouldBeRemoved, ", "))
	}

	c.JSON(http.StatusOK, Response{Data: preview})
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

	// Parse the target version's snapshot (the state we're rolling back to).
	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config: " + err.Error()},
		})
		return
	}

	store := configstore.NewStore(db, h.TxManager.Dialect)

	// Compute the rollback DDL by comparing current registry state against
	// the target snapshot. This produces quarantine-aware DDL that renames
	// tables/columns instead of dropping them.
	var dbName string
	if h.TxManager.Dialect.DriverName() != "libsql" {
		db.QueryRow("SELECT DATABASE()").Scan(&dbName)
	}

	// Generate quarantine-aware rollback DDL from the snapshot comparison.
	rollbackDDL := doctype.RollbackDDLForVersion(reg, snapshot, h.TxManager.Dialect)
	for _, stmt := range rollbackDDL {
		slog.Info("rollback DDL", "sql", stmt)
	}

	// Begin a transaction for the rollback.
	tx, err := db.Begin()
	if err != nil {
		internalError(c, "beginning rollback transaction", err)
		return
	}
	defer tx.Rollback()

	// Apply quarantine-aware DDL through the transaction.
	if len(rollbackDDL) > 0 {
		if err := configstore.ApplyDDLTx(tx, rollbackDDL); err != nil {
			slog.Error("rollback: DDL failed — rolling back", "version", versionID, "error", err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Rollback DDL failed: " + err.Error()},
			})
			return
		}
	}

	// Save the target version's config to DB within the transaction.
	if err := store.ActivateSnapshot(tx, snapshot, reg, siteName, h.TxManager.Dialect); err != nil {
		internalError(c, "saving config during rollback", err)
		return
	}

	// Commit the transaction.
	if err := tx.Commit(); err != nil {
		internalError(c, "committing rollback transaction", err)
		return
	}

	// Invalidate analytics worker — rollback restores old schema.
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.InvalidateAllMetrics()
	}

	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	newSnapshot, _ := store.CollectSnapshot(reg, siteName)
	store.CreateConfigVersion(siteName, createdBy, "Rollback to version "+versionID, "Active", newSnapshot)

	// Collect quarantine info from the rollback DDL for user transparency.
	var quarantined []string
	for _, stmt := range rollbackDDL {
		if strings.Contains(stmt, "_dropquar_") || strings.Contains(stmt, "RENAME") {
			quarantined = append(quarantined, stmt)
		}
	}
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"message":     "rolled back",
		"status":      "Active",
		"quarantined": quarantined,
	}})
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
	roles, err := store.LoadRoles(c.GetString("site_name"))
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
	if err := store.SaveRoles([]*doctype.Role{&role}, c.GetString("site_name")); err != nil {
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
	if err := store.SaveRoles([]*doctype.Role{&role}, c.GetString("site_name")); err != nil {
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
	permissions, err := store.LoadPermissions(c.GetString("site_name"))
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
		if err := store.SavePermissions(permissions, c.GetString("site_name")); err != nil {
		internalError(c, "saving permissions", err)
		return
	}
	// Reload into registry.
	reg := h.siteRegistry(c)
		roles, _ := store.LoadRoles(c.GetString("site_name"))
	reg.Permissions.LoadPermissionsFromDB(roles, permissions)
	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "saved"}})
}

// --- Workflows ---

// HandleSystemWorkflows returns all workflows.
// GET /api/system/workflows
func (h *Handler) HandleSystemWorkflows(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db, h.TxManager.Dialect)
	workflows, err := store.LoadWorkflows(c.GetString("site_name"))
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
	store := configstore.NewStore(db, h.TxManager.Dialect)
		if err := store.SaveWorkflows([]*doctype.Workflow{&wf}, c.GetString("site_name")); err != nil {
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
		system.GET("/config/versions/:id/preview", handler.HandleConfigVersionPreview)
		system.POST("/config/versions/:id/activate", handler.HandleConfigVersionActivate)
		system.POST("/config/versions/:id/discard", handler.HandleConfigVersionDiscard)
		system.GET("/config/versions/:id/rollback-preview", handler.HandleConfigVersionRollbackPreview)
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
