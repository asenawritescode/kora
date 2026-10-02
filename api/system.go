package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/schema"
)

// --- Auth Providers ---

// HandleAuthProviders returns enabled authentication providers.
// Public endpoint — no auth required.
func (h *Handler) HandleAuthProviders(c *gin.Context) {
	providers := []auth.AuthProvider(nil)
	if h.AuthProviders != nil {
		providers = h.AuthProviders.List()
	} else {
		providers = auth.NewProviderRegistry().List()
	}
	c.JSON(http.StatusOK, Response{
		Data: map[string]any{
			"providers": providers,
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

// SystemAuditEntry is the redacted, read-only projection of the operation
// ledger exposed to authorized Studio users. It intentionally excludes command
// arguments and business payloads; hashes and actor metadata are sufficient to
// explain an operation without leaking tenant data.
type SystemAuditEntry struct {
	ID            string `json:"id"`
	OperationID   string `json:"operation_id"`
	CorrelationID string `json:"correlation_id,omitempty"`
	CausationID   string `json:"causation_id,omitempty"`
	Source        string `json:"source,omitempty"`
	PrincipalType string `json:"principal_type,omitempty"`
	PrincipalID   string `json:"principal_id,omitempty"`
	ActorUser     string `json:"actor_user,omitempty"`
	Command       string `json:"command"`
	Doctype       string `json:"doctype,omitempty"`
	DocName       string `json:"doc_name,omitempty"`
	Status        string `json:"status"`
	ErrorCode     string `json:"error_code,omitempty"`
	PayloadHash   string `json:"payload_hash,omitempty"`
	BeforeHash    string `json:"before_hash,omitempty"`
	AfterHash     string `json:"after_hash,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// SystemDoctypeResponse is the full schema response for a single DocType.
type SystemDoctypeResponse struct {
	DocType      *doctype.DocType             `json:"doctype"`
	Status       string                       `json:"status"` // "Active" or "Draft"
	Workflow     *WorkflowResponse            `json:"workflow,omitempty"`
	Permissions  map[string]bool              `json:"permissions"`
	Transitions  []doctype.WorkflowTransition `json:"transitions,omitempty"`
	ReferencedBy []ReferenceInfo              `json:"referenced_by,omitempty"`
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
	if !canInspectDocType(c, dt.Name) {
		writeError(c, http.StatusForbidden, "permission.denied", "Token cannot inspect this DocType", map[string]any{"doctype": dt.Name})
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
	dbName := h.siteDatabaseName(c, db)
	resp.Status = "Draft"
	if liveSchema, err := h.siteDialect(c).LoadSchema(db, dbName); err == nil && liveSchema != nil {
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
	ops := []string{"read", "write", "create", "delete", "submit", "cancel", "amend", "export", "import", "report"}
	authType := c.GetString("auth_type")
	if authType == "extension" || authType == "channel_session" {
		key := "extension_permissions"
		if authType == "channel_session" {
			key = "channel_permissions"
		}
		value, _ := c.Get(key)
		permissions, _ := value.([]doctype.Permission)
		result := make(map[string]bool, len(ops))
		for _, operation := range ops {
			result[operation] = auth.HasExtensionPermission(permissions, dt, operation)
		}
		return result
	}
	userRoles := c.GetStringSlice("user_roles")
	// If no roles set, return full access (bootstrapping / system user).
	if len(userRoles) == 0 {
		return map[string]bool{
			"read": true, "write": true, "create": true, "delete": true,
			"submit": true, "cancel": true, "amend": true,
			"export": true, "import": true, "report": true,
		}
	}
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
	Modules           []ModuleGroup  `json:"modules"`
	Views             []ViewNavItem  `json:"views,omitempty"`
	Branding          Branding       `json:"branding"`
	Copy              ExperienceCopy `json:"copy"`
	User              UserInfo       `json:"user"`
	AdminCapabilities []string       `json:"admin_capabilities"`
}

// ModuleGroup is a group of DocTypes under a module.
type ModuleGroup struct {
	Module   string           `json:"module"`
	Label    string           `json:"label"`
	DocTypes []DocTypeNavItem `json:"doctypes"`
}

// DocTypeNavItem is a single DocType entry in the navigation.
type DocTypeNavItem struct {
	Name         string `json:"name"`
	ResourceName string `json:"resource_name"`
	Label        string `json:"label"`
	Icon         string `json:"icon,omitempty"`
	IsChild      bool   `json:"is_child"`
}

// ViewNavItem is a configured workspace view entry in the navigation.
type ViewNavItem struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Route  string `json:"route"`
	Type   string `json:"type"`
	Module string `json:"module"`
	Icon   string `json:"icon,omitempty"`
}

// AppBranding is the global branding config (set from common config at startup).
var AppBranding = Branding{AppName: "Kora", PrimaryColor: "#2563eb"}

// Branding holds per-site branding configuration.
type Branding struct {
	AppName      string `json:"app_name"`
	ShortName    string `json:"short_name,omitempty"`
	LogoURL      string `json:"logo_url,omitempty"`
	LogoDarkURL  string `json:"logo_dark_url,omitempty"`
	FaviconURL   string `json:"favicon_url,omitempty"`
	PrimaryColor string `json:"primary_color"`
	AccentColor  string `json:"accent_color,omitempty"`
	FontFamily   string `json:"font_family,omitempty"`
	HeadingFont  string `json:"heading_font,omitempty"`
	DefaultMode  string `json:"default_mode,omitempty"`
}

type ExperienceCopy struct {
	LoginTitle       string `json:"login_title"`
	LoginDescription string `json:"login_description"`
	LoginHelp        string `json:"login_help"`
	WorkspaceLoading string `json:"workspace_loading"`
	EmptyRecords     string `json:"empty_records"`
	SupportLabel     string `json:"support_label"`
}

type TenantExperience struct {
	Branding Branding       `json:"branding"`
	Copy     ExperienceCopy `json:"copy"`
}

var defaultExperienceCopy = ExperienceCopy{
	LoginTitle:       "Sign in to your workspace",
	LoginDescription: "Access your records, workflows, and reports.",
	LoginHelp:        "Use your organization account to continue.",
	WorkspaceLoading: "Preparing your workspace",
	EmptyRecords:     "No records found.",
	SupportLabel:     "Contact your administrator",
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
		if dt.IsChildTable || !canInspectDocType(c, dt.Name) {
			continue
		}
		module := dt.Module
		if module == "" {
			module = "System"
		}
		moduleMap[module] = append(moduleMap[module], DocTypeNavItem{
			Name:         dt.Name,
			ResourceName: dt.ResourceName,
			Label:        dt.Name,
			IsChild:      false,
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

	views := make([]ViewNavItem, 0)
	if store := h.viewStore(c); store != nil {
		seenRoutes := make(map[string]bool)
		configuredViews, _ := store.LoadViews(siteName(c))
		for _, view := range configuredViews {
			if view == nil || view.Route == "" || seenRoutes[view.Route] {
				continue
			}
			seenRoutes[view.Route] = true
			label := view.Label
			if label == "" {
				label = view.Name
			}
			views = append(views, ViewNavItem{
				Name: view.Name, Label: label, Route: view.Route,
				Type: view.Type, Module: view.Module,
			})
		}
		manifests, _ := store.LoadPageManifests(siteName(c))
		for _, manifest := range manifests {
			if manifest == nil || manifest.Spec.Route == "" || seenRoutes[manifest.Spec.Route] {
				continue
			}
			seenRoutes[manifest.Spec.Route] = true
			label := manifest.Metadata.Name
			if label == "" {
				label = manifest.Spec.Route
			}
			views = append(views, ViewNavItem{
				Name:   manifest.Metadata.Name,
				Label:  label,
				Route:  manifest.Spec.Route,
				Type:   "page_manifest",
				Module: manifest.Metadata.Package,
			})
		}
		sort.Slice(views, func(i, j int) bool {
			if views[i].Module == views[j].Module {
				return views[i].Label < views[j].Label
			}
			return views[i].Module < views[j].Module
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

	experience := h.resolveTenantExperience(c)

	// Admin capabilities: only users with the admin role see the Administrator section.
	// Each string matches the `name` of an admin item in the sidebar (Sidebar.tsx:adminItems).
	adminCapabilities := []string{}
	if userHasAdminRole(user.Roles) {
		adminCapabilities = []string{
			"doctypes", "permissions", "workflows", "versions",
			"users", "scripts", "extensions", "secrets", "analytics", "views",
		}
	}

	c.JSON(http.StatusOK, Response{
		Data: NavigationResponse{
			Modules:           modules,
			Views:             views,
			Branding:          experience.Branding,
			Copy:              experience.Copy,
			User:              user,
			AdminCapabilities: adminCapabilities,
		},
	})
}

// userHasAdminRole returns true if the user has the configured admin role.
func userHasAdminRole(roles []string) bool {
	for _, r := range roles {
		if r == doctype.AdminRole {
			return true
		}
	}
	return false
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
	dbName := h.siteDatabaseName(c, db)
	tableExists := make(map[string]bool)
	if liveSchema, err := h.siteDialect(c).LoadSchema(db, dbName); err == nil && liveSchema != nil {
		for tableName := range liveSchema.Tables {
			// Table names are like "tabProduct" — strip the "tab" prefix.
			tableExists[strings.TrimPrefix(tableName, "tab")] = true
		}
	}

	// Filter out child tables for the admin list.
	var result []doctypeWithStatus
	for _, dt := range doctypes {
		if !dt.IsChildTable {
			if !canInspectDocType(c, dt.Name) {
				continue
			}
			status := "Draft"
			if tableExists[dt.Name] {
				status = "Active"
			}
			result = append(result, doctypeWithStatus{DocType: dt, Status: status})
		}
	}

	c.JSON(http.StatusOK, Response{Data: result})
}

// canInspectDocType keeps schema discovery inside the same least-privilege
// boundary as record access. Browser sessions retain the existing behavior;
// delegated and extension credentials only see DocTypes present in their
// permission grant.
func canInspectDocType(c *gin.Context, doctypeName string) bool {
	authType := c.GetString("auth_type")
	if authType != "extension" && authType != "channel_session" {
		return true
	}
	key := "extension_permissions"
	if authType == "channel_session" {
		key = "channel_permissions"
	}
	value, ok := c.Get(key)
	permissions, valid := value.([]doctype.Permission)
	if !ok || !valid {
		return false
	}
	for _, operation := range []string{"read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report"} {
		if auth.HasExtensionPermission(permissions, doctypeName, operation) {
			return true
		}
	}
	return false
}

// --- Doctype Create ---

// HandleSystemDoctypeCreate creates a new DocType from JSON body.
// POST /api/system/doctype?activate=true|false
func (h *Handler) HandleSystemDoctypeCreate(c *gin.Context) {
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB
	siteName := c.GetString("site_name")

	var dt doctype.DocType
	if err := c.ShouldBindJSON(&dt); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+err.Error(), nil)
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
		conflictError(c, "doctype.already_exists", "DocType already exists: "+dt.Name, map[string]any{"doctype": dt.Name})
		return
	}

	// Determine if we should activate immediately.
	activate := c.Query("activate") != "false"

	store := configstore.NewStore(db, h.siteDialect(c))

	if activate {
		if !requireSafeDoctypeChange(c, nil, singleDocTypeSlice(&dt)) {
			return
		}

		// Activate immediately: save to DB, register, create permissions, run migration.
		if err := store.SaveDocType(&dt, siteName); err != nil {
			internalError(c, "saving doctype", err)
			return
		}
		reg.Register(&dt)

		if err := store.AutoCreatePermissionsForDoctype(dt.Name, siteName); err != nil {
			slog.Warn("failed to auto-create permissions for new doctype", "doctype", dt.Name, "error", err)
		} else {
			roles, err := store.LoadRoles(siteName)
			if err == nil {
				perms, err2 := store.LoadPermissions(siteName)
				if err2 == nil {
					reg.Permissions.LoadPermissionsFromDB(roles, perms)
				}
			}
		}

		dbName := h.siteDatabaseName(c, db)
		if err := schema.MigrateSiteFromRegistry(db, dbName, reg, h.siteDialect(c)); err != nil {
			slog.Error("migration failed after doctype create", "doctype", dt.Name, "error", err)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "Schema migration failed: " + err.Error()},
			})
			return
		}
		h.invalidateAnalyticsForDoctype(c, dt.Name)
	}

	// Create config version.
	var (
		snapshot      *doctype.ConfigSnapshot
		baseVersionID string
		err           error
	)
	if activate {
		snapshot, _ = store.CollectSnapshot(reg, siteName)
	} else {
		snapshot, baseVersionID, err = store.BuildDraftSnapshot(reg, siteName, &dt)
		if err != nil {
			internalError(c, "building draft snapshot", err)
			return
		}
	}

	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	status := "Draft"
	if activate {
		status = "Active"
	}
	var versionID string
	var versionNum int
	if activate {
		versionID, versionNum, err = store.CreateConfigVersion(
			siteName, createdBy, "Created "+dt.Name+" via web", status, snapshot,
		)
	} else {
		versionID, versionNum, err = store.CreateConfigVersionWithBase(
			siteName, createdBy, "Created "+dt.Name+" via web", status, snapshot, baseVersionID,
		)
	}
	if err != nil {
		slog.Warn("failed to create config version", "error", err)
	}

	code := http.StatusCreated
	if !activate {
		code = http.StatusOK
	}
	c.JSON(code, Response{
		Data: map[string]any{
			"doctype":     dt,
			"version_id":  versionID,
			"version_num": versionNum,
			"status":      status,
		},
	})

	if activate {
		if cfg := analytics.LoadCloudRelayConfig(); cfg != nil {
			siteName := c.GetString("site_name")
			analytics.SendCloudProductEvent(nil, *cfg, analytics.CloudProductEventDTO{
				SiteID:    siteName,
				AccountID: cfg.AccountID,
				Kind:      "first_doctype",
				Properties: analytics.FirstDocTypePropertiesDTO{
					DocType: dt.Name,
					Site:    siteName,
					Status:  status,
				},
			}, "first_doctype:"+siteName)
		}
	}
}

// --- Doctype Update ---

// HandleSystemDoctypeUpdate updates an existing DocType.
// PUT /api/system/doctype/:doctype?activate=true|false
func (h *Handler) HandleSystemDoctypeUpdate(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB
	siteName := c.GetString("site_name")

	oldDT := reg.Get(doctypeName)
	if oldDT == nil {
		notFoundError(c, "doctype.not_found", "DocType not found: "+doctypeName, map[string]any{"doctype": doctypeName})
		return
	}

	var newDT doctype.DocType
	if err := c.ShouldBindJSON(&newDT); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format: "+err.Error(), nil)
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

	store := configstore.NewStore(db, h.siteDialect(c))
	// Activate?
	activate := c.Query("activate") != "false"
	status := "Draft"
	if activate {
		if !requireSafeDoctypeChange(c, singleDocTypeSlice(oldDT), singleDocTypeSlice(&newDT)) {
			return
		}

		if err := store.SaveDocType(&newDT, siteName); err != nil {
			internalError(c, "saving doctype", err)
			return
		}

		// Update registry (replace old with new).
		reg.Register(&newDT)
		status = "Active"
		// Get DB name from the connection.
		dbName := h.siteDatabaseName(c, db)
		if err := schema.MigrateSiteFromRegistry(db, dbName, reg, h.siteDialect(c)); err != nil {
			slog.Error("migration failed after doctype update", "doctype", doctypeName, "error", err)
		}
		// Invalidate analytics worker — field changes mean regenerated metrics.
		h.invalidateAnalyticsForDoctype(c, doctypeName)
	}

	// Create config version.
	var (
		snapshot      *doctype.ConfigSnapshot
		baseVersionID string
		err           error
	)
	if activate {
		snapshot, _ = store.CollectSnapshot(reg, siteName)
	} else {
		snapshot, baseVersionID, err = store.BuildDraftSnapshot(reg, siteName, &newDT)
		if err != nil {
			internalError(c, "building draft snapshot", err)
			return
		}
	}
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	var versionID string
	var versionNum int
	if activate {
		versionID, versionNum, err = store.CreateConfigVersion(
			siteName, createdBy, "Updated "+doctypeName+" via web", status, snapshot,
		)
	} else {
		versionID, versionNum, err = store.CreateConfigVersionWithBase(
			siteName, createdBy, "Updated "+doctypeName+" via web", status, snapshot, baseVersionID,
		)
	}
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
//
//	cleanup=config (default): Delete config rows only. Business tables, analytics, permissions survive.
//	cleanup=full: Full cleanup — also deletes analytics, permissions, workflows, and clears Link fields.
//	cleanup=none: Soft delete — only remove from registry.
func (h *Handler) HandleSystemDoctypeDelete(c *gin.Context) {
	doctypeName := c.Param("doctype")
	reg := h.siteRegistry(c)
	db := h.siteTx(c).DB

	if !reg.Has(doctypeName) {
		notFoundError(c, "doctype.not_found", "DocType not found: "+doctypeName, map[string]any{"doctype": doctypeName})
		return
	}

	cleanup := c.Query("cleanup")
	if cleanup == "" {
		cleanup = "config" // default: current behavior
	}

	if !requireSafeDoctypeChange(c, singleDocTypeSlice(reg.Get(doctypeName)), nil) {
		return
	}

	// Delete from config tables (always).
	store := configstore.NewStore(db, h.siteDialect(c))
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_field WHERE parent = ?"), doctypeName); err != nil {
		internalError(c, "deleting fields", err)
		return
	}
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_doctype WHERE name = ?"), doctypeName); err != nil {
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
			if _, err := db.Exec(h.siteQuery(c, fmt.Sprintf("DELETE FROM %s WHERE doctype = ?", table)), doctypeName); err != nil {
				slog.Warn("analytics cleanup failed", "table", table, "doctype", doctypeName, "error", err)
			}
		}

		// Clean permissions.
		if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_permission WHERE doctype = ?"), doctypeName); err != nil {
			slog.Warn("permission cleanup failed", "doctype", doctypeName, "error", err)
		}

		// Clean workflows.
		if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_workflow WHERE document_type = ?"), doctypeName); err != nil {
			slog.Warn("workflow cleanup failed", "doctype", doctypeName, "error", err)
		}

		// Clear dangling Link fields in OTHER doctypes that pointed to this one.
		if _, err := db.Exec(h.siteQuery(c, "UPDATE _kora_field SET options = '' WHERE fieldtype = 'Link' AND options = ?"), doctypeName); err != nil {
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
	preview := schema.AnalyzeImpact(db, oldDT, &proposed, reg, h.siteDialect(c))

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
		h.siteQuery(c, "SELECT config, site, status, COALESCE(change_list, '') FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &currentStatus, &changeList)
	if err != nil {
		writeConfigVersionReadError(c, "loading config version preview", err)
		return
	}

	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "version.parse_failed", "Failed to parse version config", nil)
		return
	}

	// Check staleness.
	var newerActiveCount int
	if err := db.QueryRow(
		h.siteQuery(c, "SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND version > (SELECT version FROM _kora_config_version WHERE id = ?) AND status = 'Active'"),
		siteName, versionID,
	).Scan(&newerActiveCount); err != nil {
		internalError(c, "checking preview version staleness", err)
		return
	}

	var preview map[string]any

	// If a stored change_list exists, use it directly (no re-computation).
	if changeList != "" {
		var fullDiff doctype.ConfigDiffFull
		if parseErr := json.Unmarshal([]byte(changeList), &fullDiff); parseErr == nil {
			preview = map[string]any{
				"version_id":              versionID,
				"status":                  currentStatus,
				"doctypes_in_snapshot":    len(snapshot.DocTypes),
				"roles_in_snapshot":       len(snapshot.Roles),
				"permissions_in_snapshot": len(snapshot.Permissions),
				"workflows_in_snapshot":   len(snapshot.Workflows),
				"diff_summary":            fullDiff.Summary(),
				"is_breaking":             fullDiff.Doctypes != nil && fullDiff.Doctypes.IsBreaking,
				"newer_active_versions":   newerActiveCount,
				"section_changes":         fullDiff.SectionChanges,
				"change_list_version":     "v1",
				"warning":                 "",
			}
			if newerActiveCount > 0 {
				preview["warning"] = fmt.Sprintf("Activating this version will REVERT %d newer active version(s). Changes made since this version was created will be lost.", newerActiveCount)
			}
			if fullDiff.Doctypes != nil && fullDiff.Doctypes.IsBreaking {
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
		"version_id":              versionID,
		"status":                  currentStatus,
		"doctypes_in_snapshot":    len(snapshot.DocTypes),
		"roles_in_snapshot":       len(snapshot.Roles),
		"permissions_in_snapshot": len(snapshot.Permissions),
		"workflows_in_snapshot":   len(snapshot.Workflows),
		"diff_summary":            diff.Summary(),
		"is_breaking":             diff.IsBreaking,
		"newer_active_versions":   newerActiveCount,
		"change_list_version":     "legacy",
		"warning":                 "",
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

func writeConfigVersionReadError(c *gin.Context, operation string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
		return
	}
	internalError(c, operation, err)
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
		h.siteQuery(c, "SELECT config, site, status, COALESCE(change_list, ''), COALESCE(base_version_id, ''), COALESCE(min_kora_version, '') FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &currentStatus, &changeList, &baseVersionID, &minKoraVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
		} else {
			internalError(c, "loading config version for activation", err)
		}
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
		var activeVersionID, activeConfigHash, baseConfigHash string
		activeErr := db.QueryRow(
			h.siteQuery(c, "SELECT id, COALESCE(config_hash, '') FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"),
			siteName,
		).Scan(&activeVersionID, &activeConfigHash)
		stale := errors.Is(activeErr, sql.ErrNoRows)
		if activeErr != nil && !stale {
			internalError(c, "checking active config version", activeErr)
			return
		}
		baseErr := db.QueryRow(h.siteQuery(c, "SELECT COALESCE(config_hash, '') FROM _kora_config_version WHERE id = ?"), baseVersionID).Scan(&baseConfigHash)
		if errors.Is(baseErr, sql.ErrNoRows) {
			stale = true
		} else if baseErr != nil {
			internalError(c, "checking draft base config version", baseErr)
			return
		}
		if activeVersionID != "" && isStaleBaseVersion(baseVersionID, baseConfigHash, activeVersionID, activeConfigHash) {
			stale = true
		}
		if stale {
			slog.Warn("stale draft -- base version mismatch", "draft_id", versionID, "base", baseVersionID, "active", activeVersionID)
			if c.Query("force") != "true" {
				c.JSON(http.StatusConflict, ErrorResponse{
					Error: map[string]any{
						"message":           fmt.Sprintf("This draft was created from version %s but %s is now active. Re-save the draft to incorporate changes.", baseVersionID, activeVersionID),
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
	if err := db.QueryRow(
		h.siteQuery(c, "SELECT COUNT(*) FROM _kora_config_version WHERE site = ? AND version > (SELECT version FROM _kora_config_version WHERE id = ?) AND status = 'Active'"),
		siteName, versionID,
	).Scan(&newerActiveCount); err != nil {
		internalError(c, "checking for newer active config versions", err)
		return
	}
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

	store := configstore.NewStore(db, h.siteDialect(c))

	// Capture the pre-activation registry so the realtime notice can distinguish
	// a newly available record type from an updated one after the snapshot is
	// applied. The event is emitted only after the activation transaction commits.
	existingDoctypes := make(map[string]bool)
	previousDoctypes := make([]*doctype.DocType, 0)
	for _, existing := range reg.All() {
		if existing != nil {
			existingDoctypes[existing.Name] = true
			previousDoctypes = append(previousDoctypes, existing)
		}
	}

	// Compute schema changes before opening the write transaction. Inspection
	// uses *sql.DB; doing it after Begin can deadlock when the site pool has one
	// connection because the transaction already holds that connection.
	// Prefer stored change_list for precise DDL generation; fall back to
	// live schema diff for legacy versions without a stored change_list.
	var ddlStatements []string
	dbName := h.siteDatabaseName(c, db)

	if changeList != "" {
		var fullDiff doctype.ConfigDiffFull
		if parseErr := json.Unmarshal([]byte(changeList), &fullDiff); parseErr == nil {
			slog.Info("activation: generating DDL from stored change_list", "version", versionID)
			var doctypeChanges []doctype.ConfigChange
			if fullDiff.Doctypes != nil {
				doctypeChanges = fullDiff.Doctypes.Changes
			}
			changes := doctype.ConvertConfigChanges(doctypeChanges, fullDiff.SectionChanges, snapshot)
			ddlStatements, err = doctype.GenerateDDLFromDiff(changes, h.siteDialect(c))
			if err != nil {
				internalError(c, "generating schema changes for activation", err)
				return
			}
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
		liveSchema, schemaErr := schema.LoadLiveSchema(db, dbName, h.siteDialect(c))
		if schemaErr != nil {
			internalError(c, "loading live schema for config activation", schemaErr)
			return
		}
		schemaDiff := schema.ComputeDiff(snapshot.DocTypes, reg.Get, liveSchema, h.siteDialect(c))
		if !schemaDiff.IsEmpty() {
			ddlStatements = schemaDiff.GenerateDDL(snapshot.DocTypes, reg.Get, h.siteDialect(c))
			for _, stmt := range ddlStatements {
				slog.Info("activation DDL (live diff)", "sql", stmt)
			}
		}
	}

	// Begin the activation transaction only after read-only schema inspection.
	// Config writes and transactional DDL remain atomic from this point onward.
	tx, err := db.Begin()
	if err != nil {
		internalError(c, "beginning activation transaction", err)
		return
	}
	defer tx.Rollback() // no-op after successful commit

	// Step 1: Save all config to DB within the transaction and rebuild the registry.
	if err := store.ActivateSnapshot(tx, snapshot, siteName, h.siteDialect(c)); err != nil {
		slog.Error("activation: config write failed — rolling back", "version", versionID, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Config write failed: " + err.Error()},
		})
		return
	}

	// Step 2: Apply DDL.
	// LibSQL: use ExecuteBatch (SQLite DDL auto-commits, cannot be inside a tx).
	// MySQL: use ApplyDDLTx (wrapped in the activation transaction for rollback).
	if len(ddlStatements) > 0 {
		if h.siteDialect(c).DriverName() == "libsql" {
			if err := h.siteDialect(c).ExecuteBatch(db, ddlStatements); err != nil {
				slog.Error("activation: LibSQL DDL failed", "version", versionID, "error", err)
				writeError(c, http.StatusInternalServerError, "schema.migration_failed", "Schema migration failed", map[string]any{"error": err.Error()})
				return
			}
		} else {
			if err := configstore.ApplyDDLTx(tx, ddlStatements); err != nil {
				slog.Error("activation: DDL failed — rolling back", "version", versionID, "error", err)
				writeError(c, http.StatusInternalServerError, "schema.migration_failed", "Schema migration failed", map[string]any{"error": err.Error()})
				return
			}
		}
	}

	// Keep config rows, schema, and version history in one transaction. The
	// newly Active record supersedes prior active records transactionally.
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	if _, _, err := store.CreateConfigVersionTx(tx, siteName, createdBy, "Activated version "+versionID, "Active", snapshot, baseVersionID); err != nil {
		internalError(c, "recording active config version", err)
		return
	}
	draftResult, err := tx.Exec(h.siteQuery(c, "UPDATE _kora_config_version SET status = 'Superseded', is_active = ? WHERE site = ? AND id = ? AND status = 'Draft'"), 0, siteName, versionID)
	if err != nil {
		internalError(c, "marking activated draft as superseded", err)
		return
	}
	if affected, err := draftResult.RowsAffected(); err != nil {
		internalError(c, "verifying activated draft status", err)
		return
	} else if affected != 1 {
		writeError(c, http.StatusConflict, "version.state_changed", "Draft changed while activation was in progress; refresh and try again", nil)
		return
	}
	if err := store.SupersedeSiblingDraftsTx(tx, siteName, versionID, baseVersionID); err != nil {
		internalError(c, "superseding sibling config drafts", err)
		return
	}

	// Commit config, schema, and version state together.
	if err := tx.Commit(); err != nil {
		internalError(c, "committing activation transaction", err)
		return
	}
	configstore.ApplySnapshotToRegistry(snapshot, reg)

	// Notify live Studio clients that the workspace shape changed. The normal
	// document event path cannot see config-snapshot activation because the
	// snapshot writes registry/config tables rather than a user document. Keep
	// this on the same site EventBus so both local SSE and the NATS bridge see
	// one durable, site-scoped signal. The frontend uses it to refetch
	// navigation and present a concise inbox notification.
	if bus := h.runtimeService(c, siteName).EventBus; bus != nil {
		for _, dt := range snapshot.DocTypes {
			if dt == nil || strings.TrimSpace(dt.Name) == "" {
				continue
			}
			op := analytics.EventUpdate
			if !existingDoctypes[dt.Name] {
				op = analytics.EventInsert
			}
			_ = bus.Publish(analytics.ChangeEvent{
				ID:   fmt.Sprintf("config-%s-%s", versionID, strings.ToLower(strings.ReplaceAll(dt.Name, " ", "-"))),
				Site: siteName, Doctype: dt.Name, DocName: versionID, Operation: analytics.EventOp("config_activation"),
				Timestamp: time.Now().UTC(), ModifiedBy: c.GetString("user"),
				Data: map[string]any{"config_version_id": versionID, "new": op == analytics.EventInsert, "source": "config_activation"},
			})
		}
	}

	// Invalidate analytics worker metrics cache — config activation may change
	// field types, add/remove fields, or change submittable status.
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.InvalidateAllMetrics()
	}

	// Migrate analytics rollups after commit, using the exact in-memory schema
	// that was active before this snapshot was installed.
	if len(previousDoctypes) > 0 {
		analytics.MigrateRollupMetrics(db, siteName, previousDoctypes, snapshot.DocTypes)
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "activated", "status": "Active"}})
}

func isStaleBaseVersion(baseVersionID, baseConfigHash, activeVersionID, activeConfigHash string) bool {
	if baseVersionID == "" || activeVersionID == "" {
		return false
	}
	if baseConfigHash != "" && activeConfigHash != "" {
		return baseConfigHash != activeConfigHash
	}
	return baseVersionID != activeVersionID
}

// HandleConfigVersionDiscard discards a Draft version.
// POST /api/system/config/versions/:id/discard
func (h *Handler) HandleConfigVersionDiscard(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB

	var currentStatus string
	err := db.QueryRow(h.siteQuery(c, "SELECT status FROM _kora_config_version WHERE id = ?"), versionID).Scan(&currentStatus)
	if err != nil {
		writeConfigVersionReadError(c, "loading config version for discard", err)
		return
	}

	if currentStatus != "Draft" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Only Draft versions can be discarded"},
		})
		return
	}

	if _, err := db.Exec(h.siteQuery(c, "UPDATE _kora_config_version SET status = 'Superseded' WHERE id = ?"), versionID); err != nil {
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
		h.siteQuery(c, "SELECT config, site, status FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &currentStatus)
	if err != nil {
		writeConfigVersionReadError(c, "loading config version rollback preview", err)
		return
	}

	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "version.parse_failed", "Failed to parse version config", nil)
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
		h.siteQuery(c, "SELECT config, site, status FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &currentStatus)
	if err != nil {
		writeConfigVersionReadError(c, "loading config version for rollback", err)
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

	store := configstore.NewStore(db, h.siteDialect(c))

	// Compute the rollback DDL by comparing current registry state against
	// the target snapshot. This produces quarantine-aware DDL that renames
	// tables/columns instead of dropping them.
	// Generate quarantine-aware rollback DDL from the snapshot comparison.
	rollbackDDL := doctype.RollbackDDLForVersion(reg, snapshot, h.siteDialect(c))
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

	// Apply quarantine-aware DDL. LibSQL uses ExecuteBatch (DDL auto-commits).
	if len(rollbackDDL) > 0 {
		if h.siteDialect(c).DriverName() == "libsql" {
			if err := h.siteDialect(c).ExecuteBatch(db, rollbackDDL); err != nil {
				slog.Error("rollback: LibSQL DDL failed", "version", versionID, "error", err)
				writeError(c, http.StatusInternalServerError, "schema.rollback_failed", "Rollback DDL failed", map[string]any{"error": err.Error()})
				return
			}
		} else {
			if err := configstore.ApplyDDLTx(tx, rollbackDDL); err != nil {
				slog.Error("rollback: DDL failed — rolling back", "version", versionID, "error", err)
				writeError(c, http.StatusInternalServerError, "schema.rollback_failed", "Rollback DDL failed", map[string]any{"error": err.Error()})
				return
			}
		}
	}

	// Save the target version's config to DB within the transaction.
	if err := store.ActivateSnapshot(tx, snapshot, siteName, h.siteDialect(c)); err != nil {
		internalError(c, "saving config during rollback", err)
		return
	}
	createdBy := c.GetString("user")
	if createdBy == "" {
		createdBy = "system"
	}
	if _, _, err := store.CreateConfigVersionTx(tx, siteName, createdBy, "Rollback to version "+versionID, "Active", snapshot, ""); err != nil {
		internalError(c, "recording active config version during rollback", err)
		return
	}

	// Commit the transaction.
	if err := tx.Commit(); err != nil {
		internalError(c, "committing rollback transaction", err)
		return
	}
	configstore.ApplySnapshotToRegistry(snapshot, reg)

	// Invalidate analytics worker — rollback restores old schema.
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.InvalidateAllMetrics()
	}

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

// HandleConfigVersionSnapshot returns the full ConfigSnapshot for a version,
// including ready-to-use YAML pack files for the Template Pack workflow.
// GET /api/system/config/versions/:id/snapshot
func (h *Handler) HandleConfigVersionSnapshot(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB

	var configJSON, siteName, label string
	var versionNum int
	err := db.QueryRow(
		h.siteQuery(c, "SELECT config, site, version, COALESCE(label, '') FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &versionNum, &label)
	if err != nil {
		writeConfigVersionReadError(c, "loading config version snapshot", err)
		return
	}

	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to parse version config: " + err.Error()},
		})
		return
	}

	// Collect doctype names.
	doctypeNames := make([]string, 0, len(snapshot.DocTypes))
	for _, dt := range snapshot.DocTypes {
		if dt != nil {
			doctypeNames = append(doctypeNames, dt.Name)
		}
	}

	// Generate YAML pack files ready for Template Pack File rows.
	packFiles := make([]configVersionSnapshotFile, 0)

	// Doctypes → doctypes/<name>.yaml
	for _, dt := range snapshot.DocTypes {
		if dt == nil {
			continue
		}
		yamlBytes, err := yaml.Marshal(dt)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Failed to serialize doctype %s: %v", dt.Name, err)},
			})
			return
		}
		packFiles = append(packFiles, configVersionSnapshotFile{
			Path:        fmt.Sprintf("doctypes/%s.yaml", strings.ToLower(dt.Name)),
			Content:     string(yamlBytes),
			ContentType: "doctype",
		})
	}

	// Roles → roles.yaml
	if len(snapshot.Roles) > 0 {
		yamlBytes, err := yaml.Marshal(snapshot.Roles)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Failed to serialize roles: %v", err)},
			})
			return
		}
		packFiles = append(packFiles, configVersionSnapshotFile{
			Path:        "roles.yaml",
			Content:     string(yamlBytes),
			ContentType: "roles",
		})
	}

	// Permissions → permissions.yaml
	if len(snapshot.Permissions) > 0 {
		yamlBytes, err := yaml.Marshal(snapshot.Permissions)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Failed to serialize permissions: %v", err)},
			})
			return
		}
		packFiles = append(packFiles, configVersionSnapshotFile{
			Path:        "permissions.yaml",
			Content:     string(yamlBytes),
			ContentType: "permissions",
		})
	}

	// Workflows → workflows/<name>.yaml
	for _, wf := range snapshot.Workflows {
		if wf == nil {
			continue
		}
		yamlBytes, err := yaml.Marshal(wf)
		if err != nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Failed to serialize workflow %s: %v", wf.Name, err)},
			})
			return
		}
		packFiles = append(packFiles, configVersionSnapshotFile{
			Path:        fmt.Sprintf("doctypes/%s_workflow.yaml", strings.ToLower(wf.Name)),
			Content:     string(yamlBytes),
			ContentType: "workflow",
		})
	}

	// Full snapshot JSON (reference).
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to serialize snapshot: " + err.Error()},
		})
		return
	}

	c.JSON(http.StatusOK, Response{Data: configVersionSnapshotResponse{
		VersionID:        versionID,
		Version:          versionNum,
		Site:             siteName,
		Label:            label,
		DoctypeNames:     doctypeNames,
		DoctypeCount:     len(snapshot.DocTypes),
		RolesCount:       len(snapshot.Roles),
		PermissionsCount: len(snapshot.Permissions),
		WorkflowsCount:   len(snapshot.Workflows),
		Snapshot:         json.RawMessage(snapshotJSON),
		PackFiles:        packFiles,
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
	store := configstore.NewStore(db, h.siteDialect(c))
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
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}
	if role.Name == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "Role name is required", map[string]any{"field": "name"})
		return
	}
	store := configstore.NewStore(db, h.siteDialect(c))
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
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}
	role.Name = roleName
	store := configstore.NewStore(db, h.siteDialect(c))
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
	roleMembershipQuery := "SELECT COUNT(*) FROM _kora_user WHERE FIND_IN_SET(?, REPLACE(roles, ', ', ',')) > 0"
	switch h.siteDialect(c).DriverName() {
	case "postgres":
		roleMembershipQuery = "SELECT COUNT(*) FROM _kora_user WHERE POSITION(',' || ? || ',' IN ',' || REPLACE(roles, ', ', ',') || ',') > 0"
	case "libsql":
		roleMembershipQuery = "SELECT COUNT(*) FROM _kora_user WHERE instr(',' || REPLACE(roles, ', ', ',') || ',', ',' || ? || ',') > 0"
	}
	if err := db.QueryRow(h.siteQuery(c, roleMembershipQuery), roleName).Scan(&userCount); err != nil {
		internalError(c, "checking role usage", err)
		return
	}
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_role WHERE name = ?"), roleName); err != nil {
		internalError(c, "deleting role", err)
		return
	}
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_permission WHERE role = ?"), roleName); err != nil {
		internalError(c, "deleting role permissions", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: deletedResponse{Message: "deleted", UsersWithRole: userCount}})
}

// --- Permissions ---

// HandleSystemPermissions returns all permissions.
// GET /api/system/permissions
func (h *Handler) HandleSystemPermissions(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db, h.siteDialect(c))
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
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", nil)
		return
	}
	store := configstore.NewStore(db, h.siteDialect(c))
	if err := store.SavePermissions(permissions, c.GetString("site_name")); err != nil {
		internalError(c, "saving permissions", err)
		return
	}
	// Reload into registry.
	reg := h.siteRegistry(c)
	roles, _ := store.LoadRoles(c.GetString("site_name"))
	reg.Permissions.LoadPermissionsFromDB(roles, permissions)
	c.JSON(http.StatusOK, Response{Data: savedResponse{Message: "saved"}})
}

// HandleSystemRealtime streams authenticated realtime connection events.
// It supports WebSocket when the client upgrades, and SSE as a read-only fallback.
func (h *Handler) HandleSystemRealtime(c *gin.Context) {
	siteName := c.GetString("site_name")
	services := h.runtimeService(c, siteName)
	provider := services.Realtime
	bus := services.EventBus
	scopes := realtimeScopes(c)
	slog.Info("realtime request received",
		"site", siteName,
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"upgrade", c.GetHeader("Upgrade"),
		"connection", c.GetHeader("Connection"),
		"accept", c.GetHeader("Accept"),
		"origin", c.GetHeader("Origin"),
		"user_agent", c.Request.UserAgent(),
	)
	if isWebSocketUpgrade(c.Request) {
		h.handleSystemRealtimeWebSocket(c, provider, bus, siteName, scopes)
		return
	}
	h.handleSystemRealtimeSSE(c, provider, bus, siteName, scopes)
}

func (h *Handler) handleSystemRealtimeWebSocket(c *gin.Context, provider *natsprovider.Provider, bus analytics.EventBus, siteName string, scopes []string) {
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		internalError(c, "realtime websocket upgrade failed", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "closed")
	send := func(msg []byte) error {
		return conn.Write(c.Request.Context(), websocket.MessageText, msg)
	}
	if err := h.replayRealtime(c, realtimeCursor(c), scopes, send); err != nil {
		slog.Warn("realtime replay failed", "site", siteName, "error", err)
	}
	h.streamRealtime(c, send, provider, bus, siteName, scopes)
}

func (h *Handler) handleSystemRealtimeSSE(c *gin.Context, provider *natsprovider.Provider, bus analytics.EventBus, siteName string, scopes []string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		writeError(c, http.StatusInternalServerError, "server.streaming_unsupported", "streaming unsupported", nil)
		return
	}

	writeEvent := func(eventType string, payload map[string]any) error {
		// net/http applies WriteTimeout as an absolute request deadline. Refresh
		// it per SSE frame so heartbeats can keep this intentionally long-lived
		// response open without weakening the timeout for ordinary requests.
		if err := refreshRealtimeWriteDeadline(c.Writer); err != nil {
			return err
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\n", eventType); err != nil {
			return err
		}
		if eventID, ok := payload["id"].(string); ok && eventID != "" {
			if _, err := fmt.Fprintf(c.Writer, "id: %s\n", eventID); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(c.Writer, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	send := func(msg []byte) error {
		var envelope map[string]any
		if err := json.Unmarshal(msg, &envelope); err != nil {
			envelope = map[string]any{"type": "heartbeat"}
		}
		eventType, _ := envelope["type"].(string)
		if eventType == "" {
			eventType = "message"
		}
		return writeEvent(eventType, envelope)
	}
	if err := h.replayRealtime(c, realtimeCursor(c), scopes, send); err != nil {
		slog.Warn("realtime replay failed", "site", siteName, "error", err)
	}
	h.streamRealtime(c, send, provider, bus, siteName, scopes)
}

func refreshRealtimeWriteDeadline(w http.ResponseWriter) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func realtimeScopes(c *gin.Context) []string {
	values := append([]string{}, c.Request.URL.Query()["scope"]...)
	values = append(values, c.Request.URL.Query()["resource"]...)
	var scopes []string
	for _, value := range values {
		for _, raw := range strings.Split(value, ",") {
			if scope := strings.ToLower(strings.TrimSpace(raw)); scope != "" {
				scopes = append(scopes, scope)
			}
		}
	}
	return scopes
}

func matchesRealtimeScope(scopes []string, targets ...string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, scope := range scopes {
		if scope == "*" {
			return true
		}
		for _, target := range targets {
			if strings.EqualFold(scope, target) {
				return true
			}
		}
	}
	return false
}

func realtimePayloadMatchesScopes(payload []byte, scopes []string) bool {
	if len(scopes) == 0 {
		return true
	}
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		return false
	}
	var targets []string
	for _, key := range []string{"resource", "doctype", "aggregate_type"} {
		if value, ok := message[key].(string); ok && value != "" {
			targets = append(targets, value)
			if key != "resource" {
				targets = append(targets, "doctype:"+value)
			}
		}
	}
	return matchesRealtimeScope(scopes, targets...)
}

func realtimeCursor(c *gin.Context) string {
	if cursor := strings.TrimSpace(c.Query("after")); cursor != "" {
		return cursor
	}
	return strings.TrimSpace(c.GetHeader("Last-Event-ID"))
}

// replayRealtime replays durable outbox events after a client's last cursor.
// The outbox is the transactional source of truth for event IDs, so replay is
// scoped to the authenticated site and does not introduce a second history log.
func (h *Handler) replayRealtime(c *gin.Context, after string, scopes []string, send func([]byte) error) error {
	if after == "" || h.TxManager == nil {
		return nil
	}
	siteDB := h.siteTx(c).DB
	query := db.Rebind(h.siteDialect(c), `SELECT id, event_type, site, aggregate_type, aggregate_id, created_at
		FROM _kora_outbox WHERE site = ? AND id > ? ORDER BY id LIMIT 501`)
	rows, err := siteDB.QueryContext(c.Request.Context(), query, c.GetString("site_name"), after)
	if err != nil {
		return err
	}
	defer rows.Close()
	const replayLimit = 500
	replayed := 0
	truncated := false
	lastID := after
	for rows.Next() {
		var id, eventType, site, aggregateType, aggregateID string
		var occurredAt time.Time
		if err := rows.Scan(&id, &eventType, &site, &aggregateType, &aggregateID, &occurredAt); err != nil {
			return err
		}
		if !matchesRealtimeScope(scopes, "doctype:"+aggregateType, aggregateType) {
			continue
		}
		if replayed >= replayLimit {
			truncated = true
			break
		}
		payload, err := json.Marshal(map[string]any{
			"id": id, "type": "change", "transport": "replay", "site": site,
			"resource": "doctype:" + aggregateType, "doctype": aggregateType,
			"doc_name": aggregateID, "operation": realtimeOperation(eventType),
			"occurred_at": occurredAt,
		})
		if err != nil || send(payload) != nil {
			if err != nil {
				return err
			}
			return c.Request.Context().Err()
		}
		replayed++
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return err
	}
	marker, err := json.Marshal(map[string]any{
		"type": "replay_complete", "transport": "replay", "cursor": lastID,
		"replayed": replayed, "truncated": truncated,
	})
	if err != nil {
		return err
	}
	return send(marker)
}

func realtimeOperation(eventType string) string {
	if index := strings.LastIndex(eventType, "."); index >= 0 && index+1 < len(eventType) {
		return strings.TrimPrefix(eventType[index+1:], "after_")
	}
	return "update"
}

func (h *Handler) streamRealtime(c *gin.Context, send func([]byte) error, provider *natsprovider.Provider, bus analytics.EventBus, siteName string, scopes []string) {
	if provider == nil {
		var ch <-chan analytics.ChangeEvent
		var remove func()
		if multi, ok := bus.(*analytics.MultiBus); ok {
			listener := make(chan analytics.ChangeEvent, 256)
			multi.AddListener(listener)
			ch = listener
			remove = func() { multi.RemoveListener(listener) }
		} else if bus != nil {
			var err error
			ch, err = bus.Subscribe()
			if err != nil {
				ch = nil
			}
		}
		if remove != nil {
			defer remove()
		}
		_ = send([]byte(`{"type":"connected","transport":"local","site":"` + siteName + `"}`))
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.Request.Context().Done():
				return
			case event, ok := <-ch:
				if !ok {
					return
				}
				if event.Operation == analytics.EventNotification && !realtimeNotificationMatchesRecipient(event.Data, c.GetString("user")) {
					continue
				}
				if !matchesRealtimeScope(scopes, "doctype:"+event.Doctype, event.Doctype) {
					continue
				}
				payload, err := json.Marshal(map[string]any{
					"id": event.ID, "type": "change", "transport": "local", "site": event.Site,
					"resource": "doctype:" + event.Doctype, "doctype": event.Doctype,
					"doc_name": event.DocName, "operation": event.Operation, "occurred_at": event.Timestamp, "payload": event.Data,
				})
				if err == nil && send(payload) != nil {
					return
				}
			case <-ticker.C:
				if err := send([]byte(`{"type":"heartbeat","transport":"local"}`)); err != nil {
					return
				}
			}
		}
	}

	subject := provider.Config().SubjectPrefix + ".realtime." + siteName + ".>"
	ch, drain, err := provider.Subscribe(c.Request.Context(), subject)
	if err != nil {
		_ = send([]byte(`{"type":"connected","transport":"local"}`))
		return
	}
	defer drain()

	_ = send([]byte(`{"type":"connected","transport":"nats","site":"` + siteName + `"}`))
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if !realtimePayloadVisibleToUser(msg.Data, c.GetString("user")) {
				continue
			}
			if !realtimePayloadMatchesScopes(msg.Data, scopes) {
				continue
			}
			if err := send(msg.Data); err != nil {
				return
			}
		case <-ticker.C:
			if err := send([]byte(`{"type":"heartbeat","transport":"nats"}`)); err != nil {
				return
			}
		}
	}
}

func realtimePayloadVisibleToUser(payload []byte, user string) bool {
	var envelope struct {
		Operation string         `json:"operation"`
		Payload   map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Operation != string(analytics.EventNotification) {
		return true
	}
	return realtimeNotificationMatchesRecipient(envelope.Payload, user)
}

func realtimeNotificationMatchesRecipient(payload map[string]any, user string) bool {
	recipient, _ := payload["recipient"].(string)
	return strings.TrimSpace(user) != "" && strings.EqualFold(strings.TrimSpace(recipient), strings.TrimSpace(user))
}

func isWebSocketUpgrade(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Upgrade"), "websocket") && strings.Contains(strings.ToLower(req.Header.Get("Connection")), "upgrade")
}

// --- Workflows ---

// HandleSystemWorkflows returns all workflows.
// GET /api/system/workflows
func (h *Handler) HandleSystemWorkflows(c *gin.Context) {
	db := h.siteTx(c).DB
	store := configstore.NewStore(db, h.siteDialect(c))
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
		writeError(c, http.StatusNotFound, "workflow.not_found", "No workflow for doctype", map[string]any{"doctype": doctypeName})
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
		badRequestError(c, "validation.invalid_json", "Invalid request", nil)
		return
	}
	if wf.DocumentType == "" || wf.Name == "" {
		badRequestError(c, "validation.required_field", "name and document_type are required", map[string]any{"fields": []string{"name", "document_type"}})
		return
	}
	store := configstore.NewStore(db, h.siteDialect(c))
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
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_workflow WHERE document_type = ?"), doctypeName); err != nil {
		internalError(c, "deleting workflow", err)
		return
	}
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_workflow_state WHERE workflow IN (SELECT name FROM _kora_workflow WHERE document_type = ?)"), doctypeName); err != nil {
		slog.Warn("workflow state cleanup", "error", err)
	}
	if _, err := db.Exec(h.siteQuery(c, "DELETE FROM _kora_workflow_transition WHERE workflow IN (SELECT name FROM _kora_workflow WHERE document_type = ?)"), doctypeName); err != nil {
		slog.Warn("workflow transition cleanup", "error", err)
	}
	reg.Workflows.Remove(doctypeName)
	c.JSON(http.StatusOK, Response{Data: savedResponse{Message: "deleted"}})
}

// HandleSystemAudit returns a redacted operation-audit projection for
// authorized Studio inspection. It never returns command arguments or
// business payloads; hashes and actor metadata explain an operation without
// leaking tenant data.
func (h *Handler) HandleSystemAudit(c *gin.Context) {
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT id, operation_id, correlation_id, causation_id, source,
		principal_type, principal_id, actor_user, command_name, doctype, doc_name,
		status, error_code, payload_hash, before_hash, after_hash, created_at
		FROM _kora_operation_audit WHERE 1=1`
	args := make([]any, 0, 3)
	if doctype := strings.TrimSpace(c.Query("doctype")); doctype != "" {
		query += " AND doctype = ?"
		args = append(args, doctype)
	}
	if docName := strings.TrimSpace(c.Query("doc_name")); docName != "" {
		query += " AND doc_name = ?"
		args = append(args, docName)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := h.siteTx(c).DB.Query(db.Rebind(h.siteDialect(c), query), args...)
	if err != nil {
		internalError(c, "operation audit query failed", err)
		return
	}
	defer rows.Close()
	entries := make([]SystemAuditEntry, 0)
	for rows.Next() {
		var entry SystemAuditEntry
		if err := rows.Scan(&entry.ID, &entry.OperationID, &entry.CorrelationID, &entry.CausationID, &entry.Source,
			&entry.PrincipalType, &entry.PrincipalID, &entry.ActorUser, &entry.Command, &entry.Doctype, &entry.DocName,
			&entry.Status, &entry.ErrorCode, &entry.PayloadHash, &entry.BeforeHash, &entry.AfterHash, &entry.CreatedAt); err != nil {
			internalError(c, "operation audit scan failed", err)
			return
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		internalError(c, "operation audit iteration failed", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: entries})
}

// RegisterSystemRoutes registers system endpoints on the given API group.
func RegisterSystemRoutes(apiGroup *gin.RouterGroup, handler *Handler) {
	system := apiGroup.Group("/system")
	{
		// Read endpoints.
		system.GET("/doctype/:doctype", handler.HandleSystemDoctype)
		system.GET("/doctypes", handler.HandleSystemDoctypes)
		system.GET("/graph", handler.HandleSystemGraph)
		system.GET("/doctype/:doctype/references", handler.HandleSystemDoctypeReferences)
		system.GET("/navigation", handler.HandleSystemNavigation)
		system.GET("/settings", handler.HandleSiteSettings)
		system.GET("/experience", handler.HandleSystemBranding)
		system.GET("/audit", handler.HandleSystemAudit)

		// Write endpoints.
		system.POST("/doctype/validate", handler.HandleSystemDoctypeValidate)
		system.POST("/doctype/dry-run", handler.HandleSystemDoctypeDryRun)
		system.POST("/config/validate", handler.HandleConfigurationValidate)
		system.POST("/config/dry-run", handler.HandleConfigurationDryRun)
		system.POST("/doctype", handler.HandleSystemDoctypeCreate)
		system.PUT("/doctype/:doctype", handler.HandleSystemDoctypeUpdate)
		system.DELETE("/doctype/:doctype", handler.HandleSystemDoctypeDelete)
		system.PUT("/settings", handler.HandleSiteSettingsUpdate)
		system.PUT("/experience", handler.HandleSystemExperienceUpdate)

		// Config version actions.
		system.GET("/config/versions/:id/preview", handler.HandleConfigVersionPreview)
		system.POST("/config/versions/:id/activate", handler.HandleConfigVersionActivate)
		system.POST("/config/versions/:id/discard", handler.HandleConfigVersionDiscard)
		system.GET("/config/versions/:id/rollback-preview", handler.HandleConfigVersionRollbackPreview)
		system.POST("/config/versions/:id/rollback", handler.HandleConfigVersionRollback)
		system.GET("/config/versions/:id/snapshot", handler.HandleConfigVersionSnapshot)

		// Config import.
		system.POST("/config/import", handler.HandleConfigImport)
		system.POST("/config/drafts", handler.HandleConfigurationDraft)

		// Realtime stream.
		system.GET("/realtime", handler.HandleSystemRealtime)

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
