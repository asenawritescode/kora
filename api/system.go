package api

import (
	"context"
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
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/policy"
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
	DocType      *doctype.DocType `json:"doctype"`
	Status       string           `json:"status"` // "Active" or "Draft"
	Permissions  map[string]bool  `json:"permissions"`
	ReferencedBy []ReferenceInfo  `json:"referenced_by,omitempty"`
}

// HandleSystemDoctype returns the full DocType schema and permissions.
// GET /api/system/doctype/:doctype
// Optional query param: ?format=yaml to get raw YAML output.
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
	if !canInspectDocType(c, reg, dt.Name) {
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

	c.JSON(http.StatusOK, Response{Data: resp})
}

// getUserPermissions returns a map of operation → allowed for the current user on a doctype.
func getUserPermissions(reg *doctype.Registry, c *gin.Context, dt string) map[string]bool {
	ops := []string{"read", "write", "create", "delete", "submit", "cancel", "amend", "export", "import", "report"}
	result := make(map[string]bool, len(ops))
	for _, operation := range ops {
		result[operation] = false
	}
	execution, valid := requestExecution(c)
	if !valid {
		return result
	}
	for _, operation := range ops {
		_, err := policy.Decide(execution, reg, dt, operation)
		result[operation] = err == nil
	}
	return result
}

// --- System Navigation ---

// NavigationResponse is the full navigation config for the SPA sidebar.
type NavigationResponse struct {
	Modules               []ModuleGroup  `json:"modules"`
	Views                 []ViewNavItem  `json:"views,omitempty"`
	HasConfiguration      bool           `json:"has_configuration"`
	Branding              Branding       `json:"branding"`
	Copy                  ExperienceCopy `json:"copy"`
	User                  UserInfo       `json:"user"`
	AdminCapabilities     []string       `json:"admin_capabilities"`
	CapabilitiesVersion   string         `json:"capabilities_version,omitempty"`
	SupportedCapabilities []string       `json:"supported_capabilities,omitempty"`
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
var AppBranding = Branding{AppName: "Kigae", PrimaryColor: "#2563eb"}

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
		if dt.IsChildTable || !canBrowseDocType(c, reg, dt.Name) {
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
		user.Name = currentUser(c)
	}
	if len(user.Roles) == 0 {
		if execution, valid := requestExecution(c); valid {
			user.Roles = execution.Actor().Roles
		}
	}
	if user.Email == "" {
		user.Email = c.GetString("user_email")
		if user.Email == "" {
			user.Email = currentUser(c)
		}
	}
	if user.FullName == "" {
		user.FullName = c.GetString("user_full_name")
		if user.FullName == "" {
			user.FullName = user.Name
		}
	}

	experience := h.resolveTenantExperience(c)
	hasConfiguration := len(doctypes) > 0 || len(views) > 0

	runtimeServices := h.runtimeService(c, c.GetString("site_name"))
	workflowOrchestrationAvailable := runtimeServices.WorkflowDefinitions != nil && runtimeServices.WorkflowInstances != nil
	// Admin capabilities: only users with the admin role see the Administrator section.
	// Each string matches the `name` of an admin item in the sidebar (Sidebar.tsx:adminItems).
	adminCapabilities := []string{}
	if execution, valid := requestExecution(c); valid && userHasAdminRole(execution.Actor().Roles) {
		adminCapabilities = []string{
			"doctypes", "permissions", "versions",
			"users", "scripts", "extensions", "secrets", "analytics", "views",
		}
		if workflowOrchestrationAvailable {
			adminCapabilities = append(adminCapabilities, "automations")
		}
	}
	supportedCapabilities := []string{}
	if strings.EqualFold(c.GetString("site_db_type"), "mysql") && runtimeServices.Requests != nil {
		supportedCapabilities = append(supportedCapabilities, "requests.inbox.v2")
	}
	if workflowOrchestrationAvailable {
		supportedCapabilities = append(supportedCapabilities, "workflow.orchestration.v2")
	}

	c.JSON(http.StatusOK, Response{
		Data: NavigationResponse{
			Modules:               modules,
			Views:                 views,
			HasConfiguration:      hasConfiguration,
			Branding:              experience.Branding,
			Copy:                  experience.Copy,
			User:                  user,
			AdminCapabilities:     adminCapabilities,
			CapabilitiesVersion:   "2",
			SupportedCapabilities: supportedCapabilities,
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
			if !canInspectDocType(c, reg, dt.Name) {
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

// canBrowseDocType keeps create-only supporting records out of the everyday
// navigation while still allowing their schema to be used by forms and bundles.
func canBrowseDocType(c *gin.Context, registry *doctype.Registry, doctypeName string) bool {
	execution, valid := requestExecution(c)
	if !valid || registry == nil {
		return false
	}
	_, err := policy.Decide(execution, registry, doctypeName, "read")
	return err == nil
}

// canInspectDocType keeps schema discovery inside the same least-privilege
// boundary as record access. Browser sessions retain the existing behavior;
// delegated and extension credentials only see DocTypes present in their
// permission grant.
func canInspectDocType(c *gin.Context, registry *doctype.Registry, doctypeName string) bool {
	execution, valid := requestExecution(c)
	if !valid || registry == nil {
		return false
	}
	for _, operation := range []string{"read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report"} {
		if _, err := policy.Decide(execution, registry, doctypeName, operation); err == nil {
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
	candidate, collectErr := store.CollectSnapshot(reg, siteName)
	if collectErr != nil {
		internalError(c, "collecting candidate configuration", collectErr)
		return
	}
	doctype.UpsertSnapshotDocType(candidate, &dt)
	if rejectInvalidSnapshotReferences(c, candidate, reg) {
		return
	}

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

	createdBy := currentUser(c)
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
		siteName := c.GetString("site_name")
		if relay := h.runtimeService(c, siteName).AnalyticsRelay; relay != nil {
			relay.EnqueueProductEvent(analytics.CloudProductEventDTO{
				SiteID: siteName,
				Kind:   "first_doctype",
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
	candidate, collectErr := store.CollectSnapshot(reg, siteName)
	if collectErr != nil {
		internalError(c, "collecting candidate configuration", collectErr)
		return
	}
	doctype.UpsertSnapshotDocType(candidate, &newDT)
	if rejectInvalidSnapshotReferences(c, candidate, reg) {
		return
	}
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
	createdBy := currentUser(c)
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
//	cleanup=full: Full cleanup — also deletes analytics and permissions, and clears Link fields.
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

	// Delete from config tables and advance the tenant config revision in one
	// transaction so management package requests cannot miss this mutation.
	store := configstore.NewStore(db, h.siteDialect(c))
	candidate, collectErr := store.CollectSnapshot(reg, c.GetString("site_name"))
	if collectErr != nil {
		internalError(c, "collecting candidate configuration", collectErr)
		return
	}
	remaining := candidate.DocTypes[:0]
	for _, candidateDoc := range candidate.DocTypes {
		if candidateDoc != nil && !strings.EqualFold(candidateDoc.Name, doctypeName) {
			remaining = append(remaining, candidateDoc)
		}
	}
	candidate.DocTypes = remaining
	validationRegistry := doctype.NewRegistry()
	for _, existing := range reg.All() {
		if existing != nil && !strings.EqualFold(existing.Name, doctypeName) {
			copy := *existing
			validationRegistry.Register(&copy)
		}
	}
	if rejectInvalidSnapshotReferences(c, candidate, validationRegistry) {
		return
	}
	deleteTx, err := db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "beginning doctype deletion", err)
		return
	}
	defer deleteTx.Rollback()
	if _, err := deleteTx.Exec(h.siteQuery(c, "DELETE FROM _kora_field WHERE parent = ?"), doctypeName); err != nil {
		internalError(c, "deleting fields", err)
		return
	}
	if _, err := deleteTx.Exec(h.siteQuery(c, "DELETE FROM _kora_doctype WHERE name = ?"), doctypeName); err != nil {
		internalError(c, "deleting doctype", err)
		return
	}
	if err := configstore.BumpConfigRevisionTx(deleteTx, h.siteDialect(c)); err != nil {
		internalError(c, "updating tenant config revision", err)
		return
	}
	if err := deleteTx.Commit(); err != nil {
		internalError(c, "committing doctype deletion", err)
		return
	}

	// Full cleanup: also clean analytics, permissions, and dangling Link fields.
	if cleanup == "full" {
		// Clean analytics rollup tables.
		for _, table := range []string{
			"_kora_analytics_daily",
			"_kora_analytics_monthly",
			"_kora_analytics_events",
			"_kora_analytics_metric",
		} {
			if _, err := db.Exec(h.siteQuery(c, fmt.Sprintf("DELETE FROM %s WHERE doctype = ?", table)), doctypeName); err != nil {
				slog.Warn("analytics cleanup failed", "table", table, "doctype", doctypeName, "error", err)
			}
		}

		// Clean permissions.
		if tx, txErr := db.BeginTx(c.Request.Context(), nil); txErr != nil {
			slog.Warn("permission cleanup transaction failed", "doctype", doctypeName, "error", txErr)
		} else {
			if _, deleteErr := tx.Exec(h.siteQuery(c, "DELETE FROM _kora_permission WHERE doctype = ?"), doctypeName); deleteErr != nil {
				_ = tx.Rollback()
				slog.Warn("permission cleanup failed", "doctype", doctypeName, "error", deleteErr)
			} else if bumpErr := configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), true, false); bumpErr != nil {
				_ = tx.Rollback()
				slog.Warn("permission revision update failed", "doctype", doctypeName, "error", bumpErr)
			} else if commitErr := tx.Commit(); commitErr != nil {
				slog.Warn("permission cleanup commit failed", "doctype", doctypeName, "error", commitErr)
			}
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
	createdBy := currentUser(c)
	if createdBy == "" {
		createdBy = "system"
	}
	_, _, err = store.CreateConfigVersion(
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

	var configJSON, siteName, currentStatus, baseVersionID string
	var version int
	err := db.QueryRow(
		h.siteQuery(c, "SELECT config, site, status, COALESCE(base_version_id, ''), version FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &currentStatus, &baseVersionID, &version)
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

	// Review the target snapshot against the version it was based on so roles,
	// permissions, views, and tables are all visible.
	var baseConfig string
	var baseErr error
	if baseVersionID != "" {
		baseErr = db.QueryRow(h.siteQuery(c, "SELECT config FROM _kora_config_version WHERE id = ? AND site = ?"), baseVersionID, siteName).Scan(&baseConfig)
		if baseErr != nil && !errors.Is(baseErr, sql.ErrNoRows) {
			internalError(c, "loading config version base", baseErr)
			return
		}
	}
	if baseConfig == "" {
		baseErr = db.QueryRow(h.siteQuery(c, "SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' AND version < ? ORDER BY version DESC LIMIT 1"), siteName, version).Scan(&baseConfig)
		if baseErr != nil && !errors.Is(baseErr, sql.ErrNoRows) {
			internalError(c, "loading active config version for diff", baseErr)
			return
		}
	}
	baseSnapshot := &doctype.ConfigSnapshot{}
	if baseConfig != "" {
		baseSnapshot, err = doctype.ParseConfig(baseConfig)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "version.parse_failed", "Failed to parse the base version", nil)
			return
		}
	}
	diff := doctype.DiffFullSnapshots(baseSnapshot, snapshot)

	preview := map[string]any{
		"version_id":              versionID,
		"status":                  currentStatus,
		"doctypes_in_snapshot":    len(snapshot.DocTypes),
		"roles_in_snapshot":       len(snapshot.Roles),
		"permissions_in_snapshot": len(snapshot.Permissions),
		"diff_summary":            diff.Summary(),
		"is_breaking":             diff.Doctypes.IsBreaking,
		"newer_active_versions":   newerActiveCount,
		"changes":                 diff.Doctypes.Changes,
		"section_changes":         diff.SectionChanges,
		"change_list_version":     "full-snapshot",
		"warning":                 "",
	}
	if newerActiveCount > 0 {
		preview["warning"] = fmt.Sprintf("Activating this version will REVERT %d newer active version(s). Changes made since this version was created will be lost.", newerActiveCount)
	}
	if diff.Doctypes.IsBreaking {
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
		op, journalErr := configstore.LoadActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate")
		if journalErr != nil && !errors.Is(journalErr, sql.ErrNoRows) {
			internalError(c, "loading config activation recovery state", journalErr)
			return
		}
		if journalErr == nil && (op.Phase == configstore.ActivationActivated || op.Phase == configstore.ActivationCompleted) {
			if op.Phase == configstore.ActivationActivated {
				recoveredSnapshot, parseErr := doctype.ParseConfig(configJSON)
				if parseErr != nil {
					internalError(c, "parsing recovered config activation", parseErr)
					return
				}
				previousDoctypes := reg.All()
				existing := make(map[string]bool, len(previousDoctypes))
				for _, dt := range previousDoctypes {
					if dt != nil {
						existing[dt.Name] = true
					}
				}
				configstore.ApplySnapshotToRegistry(recoveredSnapshot, reg)
				if worker := h.siteAnalyticsWorker(c); worker != nil {
					worker.InvalidateAllMetrics()
				}
				if bus := h.runtimeService(c, siteName).EventBus; bus != nil {
					for _, dt := range recoveredSnapshot.DocTypes {
						if dt == nil || strings.TrimSpace(dt.Name) == "" {
							continue
						}
						busOp := eventstream.EventUpdate
						if !existing[dt.Name] {
							busOp = eventstream.EventInsert
						}
						_ = bus.Publish(eventstream.ChangeEvent{
							ID:   fmt.Sprintf("config-%s-%s", versionID, strings.ToLower(strings.ReplaceAll(dt.Name, " ", "-"))),
							Site: siteName, Doctype: dt.Name, DocName: versionID, Operation: eventstream.EventOp("config_activation"),
							Timestamp: time.Now().UTC(), ModifiedBy: currentUser(c),
							Data: map[string]any{"config_version_id": versionID, "new": busOp == eventstream.EventInsert, "source": "config_activation"},
						})
					}
				}
				if len(previousDoctypes) > 0 {
					if err := analytics.MigrateRollupMetrics(db, h.siteDialect(c), siteName, previousDoctypes, recoveredSnapshot.DocTypes); err != nil {
						slog.Error("recovered analytics rollup migration failed", "site", siteName, "error", err)
					}
				}
				if err := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationCompleted, op.DDLPlan, op.RecoveryPlan, ""); err != nil {
					internalError(c, "completing recovered config activation", err)
					return
				}
			}
			c.JSON(http.StatusOK, Response{Data: map[string]any{"status": "Active", "recovered": true}})
			return
		}
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
	if rejectInvalidSnapshotReferences(c, snapshot, reg) {
		return
	}

	journal, err := configstore.EnsureActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", snapshot)
	if err != nil {
		internalError(c, "persisting config activation operation", err)
		return
	}
	if journal.Phase == configstore.ActivationRequested {
		if err := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationSnapshot, "", "replan-from-live-schema", ""); err != nil {
			internalError(c, "recording saved config activation snapshot", err)
			return
		}
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

	// Recompute the schema plan from the live database on every attempt. This
	// makes a retry safe after a non-transactional DDL statement succeeded but
	// the process stopped before recording activation. Field removals remain
	// quarantine operations, but are omitted once the source column has moved.
	dbName := h.siteDatabaseName(c, db)
	ddlStatements, err := schema.PlanSnapshotActivation(db, dbName, h.siteDialect(c), reg, snapshot, changeList, versionID)
	if err != nil {
		internalError(c, "planning config activation DDL", err)
		return
	}
	planJSON, err := json.Marshal(ddlStatements)
	if err != nil {
		internalError(c, "encoding config activation plan", err)
		return
	}
	if err := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationApplying, string(planJSON), "replan-from-live-schema", ""); err != nil {
		internalError(c, "recording config activation plan", err)
		return
	}
	if err := schema.ApplySnapshotActivationDDL(c.Request.Context(), db, h.siteDialect(c), ddlStatements); err != nil {
		if journalErr := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationApplying, string(planJSON), "replan-from-live-schema", err.Error()); journalErr != nil {
			err = fmt.Errorf("schema migration failed: %w; recording failure in config activation journal: %v", err, journalErr)
		}
		writeError(c, http.StatusInternalServerError, "schema.migration_failed", "Schema migration failed", map[string]any{"error": err.Error()})
		return
	}
	if err := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationValidated, string(planJSON), "replan-from-live-schema", ""); err != nil {
		internalError(c, "recording validated config activation", err)
		return
	}

	// Begin the config/version transaction after the independently committed DDL
	// plan has been journaled and validated.
	tx, err := db.Begin()
	if err != nil {
		err = rollbackConfigOperationTx(nil, db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationValidated, string(planJSON), "replan-from-live-schema", err)
		internalError(c, "beginning activation transaction", err)
		return
	}
	defer tx.Rollback() // no-op after successful commit
	recordActivationFailure := func(cause error) error {
		return rollbackConfigOperationTx(tx, db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationValidated, string(planJSON), "replan-from-live-schema", cause)
	}

	// Step 1: Save all config to DB within the transaction and rebuild the registry.
	if err := store.ActivateSnapshot(tx, snapshot, siteName, h.siteDialect(c)); err != nil {
		slog.Error("activation: config write failed — rolling back", "version", versionID, "error", err)
		err = recordActivationFailure(err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Config write failed: " + err.Error()},
		})
		return
	}

	// Keep config rows, schema, and version history in one transaction. The
	// newly Active record supersedes prior active records transactionally.
	createdBy := currentUser(c)
	if createdBy == "" {
		createdBy = "system"
	}
	if _, _, err := store.CreateConfigVersionTx(tx, siteName, createdBy, "Activated version "+versionID, "Active", snapshot, baseVersionID); err != nil {
		internalError(c, "recording active config version", recordActivationFailure(err))
		return
	}
	draftResult, err := tx.Exec(h.siteQuery(c, "UPDATE _kora_config_version SET status = 'Superseded', is_active = ? WHERE site = ? AND id = ? AND status = 'Draft'"), 0, siteName, versionID)
	if err != nil {
		internalError(c, "marking activated draft as superseded", recordActivationFailure(err))
		return
	}
	if affected, err := draftResult.RowsAffected(); err != nil {
		internalError(c, "verifying activated draft status", recordActivationFailure(err))
		return
	} else if affected != 1 {
		cause := fmt.Errorf("draft state changed while activation was in progress")
		if journalErr := recordActivationFailure(cause); journalErr != nil && !errors.Is(journalErr, cause) {
			internalError(c, "recording config activation conflict", journalErr)
			return
		}
		writeError(c, http.StatusConflict, "version.state_changed", "Draft changed while activation was in progress; refresh and try again", nil)
		return
	}
	if err := store.SupersedeSiblingDraftsTx(tx, siteName, versionID, baseVersionID); err != nil {
		internalError(c, "superseding sibling config drafts", recordActivationFailure(err))
		return
	}
	if err := configstore.UpdateActivationOperationTx(tx, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationActivated, string(planJSON), "replan-from-live-schema"); err != nil {
		internalError(c, "recording activated config operation", recordActivationFailure(err))
		return
	}

	// Commit config, schema, and version state together.
	if err := tx.Commit(); err != nil {
		internalError(c, "committing activation transaction", recordActivationFailure(err))
		return
	}
	if err := configstore.UpdateActivationOperation(db, h.siteDialect(c), siteName, versionID, "activate", configstore.ActivationCompleted, string(planJSON), "replan-from-live-schema", ""); err != nil {
		// The config/version transaction committed with phase=activated. A retry
		// recognizes that durable state and finishes this final transition.
		internalError(c, "finalizing config activation journal", err)
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
			op := eventstream.EventUpdate
			if !existingDoctypes[dt.Name] {
				op = eventstream.EventInsert
			}
			_ = bus.Publish(eventstream.ChangeEvent{
				ID:   fmt.Sprintf("config-%s-%s", versionID, strings.ToLower(strings.ReplaceAll(dt.Name, " ", "-"))),
				Site: siteName, Doctype: dt.Name, DocName: versionID, Operation: eventstream.EventOp("config_activation"),
				Timestamp: time.Now().UTC(), ModifiedBy: currentUser(c),
				Data: map[string]any{"config_version_id": versionID, "new": op == eventstream.EventInsert, "source": "config_activation"},
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
		if err := analytics.MigrateRollupMetrics(db, h.siteDialect(c), siteName, previousDoctypes, snapshot.DocTypes); err != nil {
			slog.Error("analytics rollup migration failed", "site", siteName, "error", err)
		}
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
	dialect := h.siteDialect(c)
	var baseActiveVersionID string
	activeErr := db.QueryRow(h.siteQuery(c, "SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"), siteName).Scan(&baseActiveVersionID)
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		internalError(c, "checking active version before rollback", activeErr)
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > 256 {
		badRequestError(c, "validation.invalid_idempotency_key", "Idempotency-Key must be at most 256 bytes", nil)
		return
	}
	if idempotencyKey == "" {
		idempotencyKey = contract.NewOperationID()
	}
	operationID := restoreOperationID(siteName, versionID, idempotencyKey)
	operation, err := configstore.EnsureActivationOperation(db, dialect, siteName, operationID, "rollback", snapshot)
	if err != nil {
		internalError(c, "persisting config rollback operation", err)
		return
	}
	var journalData restoreJournalData
	if operation.RecoveryPlan != "" {
		if err := json.Unmarshal([]byte(operation.RecoveryPlan), &journalData); err != nil {
			internalError(c, "reading config rollback recovery plan", err)
			return
		}
		if journalData.TargetVersionID != versionID {
			writeError(c, http.StatusConflict, "operation.identity_conflict", "Idempotency key is already bound to another rollback target", nil)
			return
		}
	} else {
		journalData = restoreJournalData{TargetVersionID: versionID, BaseActiveVersionID: baseActiveVersionID}
	}
	if operation.Phase == configstore.ActivationActivated || operation.Phase == configstore.ActivationCompleted {
		if operation.Phase == configstore.ActivationActivated {
			if journalData.ResultVersionID == "" {
				internalError(c, "config rollback journal lacks committed result identity", fmt.Errorf("operation %s has no result version", operationID))
				return
			}
			if baseActiveVersionID == journalData.ResultVersionID {
				previousDoctypes := reg.All()
				configstore.ApplySnapshotToRegistry(snapshot, reg)
				if worker := h.siteAnalyticsWorker(c); worker != nil {
					worker.InvalidateAllMetrics()
				}
				if len(previousDoctypes) > 0 {
					if err := analytics.MigrateRollupMetrics(db, dialect, siteName, previousDoctypes, snapshot.DocTypes); err != nil {
						slog.Error("recovered rollback analytics migration failed", "site", siteName, "error", err)
					}
				}
			}
			if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationCompleted, operation.DDLPlan, operation.RecoveryPlan, ""); err != nil {
				internalError(c, "completing recovered config rollback", err)
				return
			}
		}
		c.JSON(http.StatusOK, Response{Data: map[string]any{"message": "rolled back", "status": "Active", "operation_id": idempotencyKey, "recovered": true}})
		return
	}
	if journalData.BaseActiveVersionID != baseActiveVersionID {
		writeError(c, http.StatusConflict, "version.state_changed", "The active config changed while rollback was pending; refresh and start a new rollback", map[string]any{"expected_active_version_id": journalData.BaseActiveVersionID, "active_version_id": baseActiveVersionID})
		return
	}
	if operation.Phase == configstore.ActivationRequested {
		journalJSON, _ := json.Marshal(journalData)
		if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationSnapshot, "", string(journalJSON), ""); err != nil {
			internalError(c, "recording config rollback snapshot", err)
			return
		}
	}
	previousDoctypes := reg.All()
	liveSchema, err := schema.LoadLiveSchema(db, h.siteDatabaseName(c, db), dialect)
	if err != nil {
		internalError(c, "loading live schema for config rollback", err)
		return
	}
	rollbackDDL, err := buildConfigRestoreDDL(previousDoctypes, snapshot, liveSchema, dialect, operationID)
	if err != nil {
		writeError(c, http.StatusConflict, "schema.rollback_ambiguous", "Config rollback needs manual schema recovery", map[string]any{"error": err.Error()})
		return
	}
	planJSON, err := json.Marshal(rollbackDDL)
	if err != nil {
		internalError(c, "encoding config rollback plan", err)
		return
	}
	journalJSON, err := json.Marshal(journalData)
	if err != nil {
		internalError(c, "encoding config rollback identity", err)
		return
	}
	if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationApplying, string(planJSON), string(journalJSON), ""); err != nil {
		internalError(c, "recording config rollback DDL plan", err)
		return
	}
	if len(rollbackDDL) > 0 {
		if dialect.DriverName() == "libsql" {
			err = dialect.ExecuteBatch(db, rollbackDDL)
		} else {
			for _, statement := range rollbackDDL {
				if _, err = db.Exec(statement); err != nil {
					break
				}
			}
		}
		if err != nil {
			if journalErr := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationApplying, string(planJSON), string(journalJSON), err.Error()); journalErr != nil {
				err = fmt.Errorf("rollback DDL failed: %w; recording failure in recovery journal: %v", err, journalErr)
			}
			writeError(c, http.StatusInternalServerError, "schema.rollback_failed", "Rollback DDL failed", map[string]any{"error": err.Error()})
			return
		}
	}
	if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationValidated, string(planJSON), string(journalJSON), ""); err != nil {
		internalError(c, "recording validated config rollback", err)
		return
	}
	tx, err := db.Begin()
	if err != nil {
		internalError(c, "beginning rollback transaction", err)
		return
	}
	defer tx.Rollback()
	activeQuery := "SELECT id FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1"
	if dialect.DriverName() != "libsql" {
		activeQuery += " FOR UPDATE"
	}
	var lockedActiveVersionID string
	activeErr = tx.QueryRow(h.siteQuery(c, activeQuery), siteName).Scan(&lockedActiveVersionID)
	if activeErr != nil && !errors.Is(activeErr, sql.ErrNoRows) {
		internalError(c, "locking active config version for rollback", activeErr)
		return
	}
	if lockedActiveVersionID != journalData.BaseActiveVersionID {
		writeError(c, http.StatusConflict, "version.state_changed", "The active config changed while rollback was being prepared", map[string]any{"expected_active_version_id": journalData.BaseActiveVersionID, "active_version_id": lockedActiveVersionID})
		return
	}
	store := configstore.NewStore(db, dialect)
	if err := store.ActivateSnapshot(tx, snapshot, siteName, dialect); err != nil {
		internalError(c, "saving config during rollback", err)
		return
	}
	createdBy := currentUser(c)
	if createdBy == "" {
		createdBy = "system"
	}
	resultVersionID, _, err := store.CreateConfigVersionTx(tx, siteName, createdBy, "Rollback to version "+versionID, "Active", snapshot, journalData.BaseActiveVersionID)
	if err != nil {
		internalError(c, "recording active config version during rollback", err)
		return
	}
	journalData.ResultVersionID = resultVersionID
	journalJSON, err = json.Marshal(journalData)
	if err != nil {
		internalError(c, "encoding committed rollback identity", err)
		return
	}
	if err := configstore.UpdateActivationOperationTx(tx, dialect, siteName, operationID, "rollback", configstore.ActivationActivated, string(planJSON), string(journalJSON)); err != nil {
		internalError(c, "recording activated config rollback", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "committing rollback transaction", err)
		return
	}
	configstore.ApplySnapshotToRegistry(snapshot, reg)
	if worker := h.siteAnalyticsWorker(c); worker != nil {
		worker.InvalidateAllMetrics()
	}
	if len(previousDoctypes) > 0 {
		if err := analytics.MigrateRollupMetrics(db, dialect, siteName, previousDoctypes, snapshot.DocTypes); err != nil {
			slog.Error("rollback analytics rollup migration failed", "site", siteName, "error", err)
		}
	}
	if err := configstore.UpdateActivationOperation(db, dialect, siteName, operationID, "rollback", configstore.ActivationCompleted, string(planJSON), string(journalJSON), ""); err != nil {
		internalError(c, "finalizing config rollback journal", err)
		return
	}
	var quarantined []string
	for _, statement := range rollbackDDL {
		if strings.Contains(statement, "_dropquar_") || strings.Contains(statement, "RENAME") {
			quarantined = append(quarantined, statement)
		}
	}
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"message": "rolled back", "status": "Active", "operation_id": idempotencyKey,
		"quarantined": quarantined,
	}})
}

// HandleConfigVersionSnapshot returns the full ConfigSnapshot for a version,
// including ready-to-use YAML pack files for the Template Pack workflow.
// GET /api/system/config/versions/:id/snapshot
func (h *Handler) HandleConfigHeadSnapshot(c *gin.Context) {
	var versionID string
	err := h.siteTx(c).DB.QueryRow(h.siteQuery(c,
		"SELECT id FROM _kora_config_version WHERE status IN ('Draft', 'Active') ORDER BY CASE WHEN status = 'Draft' THEN 0 ELSE 1 END, version DESC LIMIT 1",
	)).Scan(&versionID)
	if err != nil {
		writeConfigVersionReadError(c, "loading current config version", err)
		return
	}
	c.Params = append(c.Params, gin.Param{Key: "id", Value: versionID})
	h.HandleConfigVersionSnapshot(c)
}

// HandleActiveConfigSnapshot returns only the configuration version currently
// activated in the tenant runtime. Consumers that need pending drafts must use
// the explicit head endpoint and inspect its status.
func (h *Handler) HandleActiveConfigSnapshot(c *gin.Context) {
	var versionID string
	err := h.siteTx(c).DB.QueryRow(h.siteQuery(c,
		"SELECT id FROM _kora_config_version WHERE status = 'Active' OR (status IS NULL AND is_active = 1) ORDER BY version DESC LIMIT 1",
	)).Scan(&versionID)
	if err != nil {
		writeConfigVersionReadError(c, "loading active config version", err)
		return
	}
	c.Params = append(c.Params, gin.Param{Key: "id", Value: versionID})
	h.HandleConfigVersionSnapshot(c)
}

func (h *Handler) HandleConfigVersionSnapshot(c *gin.Context) {
	versionID := c.Param("id")
	db := h.siteTx(c).DB

	var configJSON, siteName, label, versionStatus string
	var versionNum int
	err := db.QueryRow(
		h.siteQuery(c, "SELECT config, site, version, COALESCE(label, ''), COALESCE(status, CASE WHEN is_active = 1 THEN 'Active' ELSE 'Superseded' END) FROM _kora_config_version WHERE id = ?"), versionID,
	).Scan(&configJSON, &siteName, &versionNum, &label, &versionStatus)
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
		Status:           versionStatus,
		Site:             siteName,
		Label:            label,
		DoctypeNames:     doctypeNames,
		DoctypeCount:     len(snapshot.DocTypes),
		RolesCount:       len(snapshot.Roles),
		PermissionsCount: len(snapshot.Permissions),
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
	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err == nil {
		err = store.SaveRolesTx(tx, []*doctype.Role{&role}, c.GetString("site_name"))
	}
	if err == nil {
		err = configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), true, false)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err != nil {
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
	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err == nil {
		err = store.SaveRolesTx(tx, []*doctype.Role{&role}, c.GetString("site_name"))
	}
	if err == nil {
		err = configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), true, false)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err != nil {
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
	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		internalError(c, "deleting role", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(h.siteQuery(c, "DELETE FROM _kora_role WHERE name = ?"), roleName); err != nil {
		internalError(c, "deleting role", err)
		return
	}
	if _, err := tx.Exec(h.siteQuery(c, "DELETE FROM _kora_permission WHERE role = ?"), roleName); err != nil {
		internalError(c, "deleting role permissions", err)
		return
	}
	if err := configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), true, false); err != nil {
		internalError(c, "updating policy revision", err)
		return
	}
	if err := tx.Commit(); err != nil {
		internalError(c, "committing role deletion", err)
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
	tx, err := db.BeginTx(c.Request.Context(), nil)
	if err == nil {
		err = store.SavePermissionsTx(tx, permissions, c.GetString("site_name"))
	}
	if err == nil {
		err = configstore.BumpAuthorizationRevisionTx(tx, h.siteDialect(c), true, false)
	}
	if err == nil {
		err = tx.Commit()
	} else if tx != nil {
		_ = tx.Rollback()
	}
	if err != nil {
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
	ReleaseTenantStreamAuthWork(c)
	siteName := c.GetString("site_name")
	services := h.runtimeService(c, siteName)
	if services.RealtimeAdmission != nil {
		release, err := services.RealtimeAdmission.Acquire(c.Request.Context())
		if err != nil {
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "realtime.capacity_exhausted", "realtime stream capacity is exhausted", nil)
			return
		}
		defer release()
	}
	var releaseReplay func()
	if realtimeCursor(c) != "" && services.WorkAdmission != nil {
		release, err := services.WorkAdmission.Acquire(c.Request.Context())
		if err != nil {
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "dependency.unavailable", "tenant work capacity is temporarily exhausted", nil)
			return
		}
		releaseReplay = func() {
			if release != nil {
				release()
				release = nil
			}
		}
	} else {
		releaseReplay = func() {}
	}
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
		h.handleSystemRealtimeWebSocket(c, provider, bus, siteName, scopes, releaseReplay)
		return
	}
	h.handleSystemRealtimeSSE(c, provider, bus, siteName, scopes, releaseReplay)
}

func (h *Handler) handleSystemRealtimeWebSocket(c *gin.Context, provider *natsprovider.Provider, bus eventstream.Bus, siteName string, scopes []string, releaseReplay func()) {
	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		releaseReplay()
		internalError(c, "realtime websocket upgrade failed", err)
		return
	}
	streamCtx, cancelStream := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(streamCtx)
	defer cancelStream()
	if h.RealtimeConnections != nil {
		defer h.RealtimeConnections.Register(func() {
			cancelStream()
			_ = conn.CloseNow()
		})()
	}
	defer conn.Close(websocket.StatusNormalClosure, "closed")
	send := func(msg []byte) error {
		return conn.Write(c.Request.Context(), websocket.MessageText, msg)
	}
	if err := h.replayRealtime(c, realtimeCursor(c), scopes, send); err != nil {
		slog.Warn("realtime replay failed", "site", siteName, "error", err)
	}
	releaseReplay()
	h.streamRealtime(c, send, provider, bus, siteName, scopes)
}

func (h *Handler) handleSystemRealtimeSSE(c *gin.Context, provider *natsprovider.Provider, bus eventstream.Bus, siteName string, scopes []string, releaseReplay func()) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		releaseReplay()
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
	releaseReplay()
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
	type replayEntry struct {
		id, eventType, site, aggregateType, aggregateID string
		occurredAt                                      time.Time
	}
	entries := make([]replayEntry, 0, 501)
	for rows.Next() {
		var entry replayEntry
		var occurredAt any
		if err := rows.Scan(&entry.id, &entry.eventType, &entry.site, &entry.aggregateType, &entry.aggregateID, &occurredAt); err != nil {
			_ = rows.Close()
			return err
		}
		entry.occurredAt, err = parseRealtimeTimestamp(occurredAt)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("parse realtime event timestamp: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	const replayLimit = 500
	replayed := 0
	truncated := false
	lastID := after
	for _, entry := range entries {
		if !matchesRealtimeScope(scopes, "doctype:"+entry.aggregateType, entry.aggregateType) {
			continue
		}
		var envelope struct {
			BusinessScope contract.BusinessScope `json:"business_scope"`
		}
		_ = json.Unmarshal(entry.payload, &envelope)
		if !h.realtimeEventVisible(c, entry.aggregateType, entry.aggregateID, nil, eventstream.EventOp(realtimeOperation(entry.eventType)), envelope.BusinessScope) {
			continue
		}
		if replayed >= replayLimit {
			truncated = true
			break
		}
		payload, err := json.Marshal(map[string]any{
			"id": entry.id, "type": "change", "transport": "replay", "site": entry.site,
			"resource": "doctype:" + entry.aggregateType, "doctype": entry.aggregateType,
			"doc_name": entry.aggregateID, "operation": realtimeOperation(entry.eventType),
			"occurred_at": entry.occurredAt,
		})
		if err != nil || send(payload) != nil {
			if err != nil {
				return err
			}
			return c.Request.Context().Err()
		}
		replayed++
		lastID = entry.id
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

func (h *Handler) streamRealtime(c *gin.Context, send func([]byte) error, provider *natsprovider.Provider, bus eventstream.Bus, siteName string, scopes []string) {
	if provider == nil {
		var ch <-chan eventstream.ChangeEvent
		var remove func()
		if multi, ok := bus.(*analytics.MultiBus); ok {
			listener := multi.NewListenerQueue(true)
			if err := multi.AddRealtimeListener(listener); err != nil {
				slog.Warn("realtime local subscription rejected", "site", siteName, "error", err)
				return
			}
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
				if !matchesRealtimeScope(scopes, "doctype:"+event.Doctype, event.Doctype) {
					continue
				}
				if !h.realtimeEventVisible(c, event.Doctype, event.DocName, event.Data, event.Operation) {
					continue
				}
				data := event.Data
				if event.Operation != eventstream.EventNotification {
					if dt := h.siteRegistry(c).Get(event.Doctype); dt != nil {
						data = visibleRealtimeFields(dt, event.Data)
					}
				}
				payload, err := json.Marshal(map[string]any{
					"id": event.ID, "type": "change", "transport": "local", "site": event.Site,
					"resource": "doctype:" + event.Doctype, "doctype": event.Doctype,
					"doc_name": event.DocName, "operation": event.Operation, "occurred_at": event.Timestamp, "payload": data,
				})
				if err == nil && send(payload) != nil {
					return
				}
			case <-ticker.C:
				valid, err := auth.RevalidateRealtimeIdentity(c)
				if !valid {
					if err != nil {
						slog.Warn("realtime authorization refresh failed", "site", siteName, "error", err)
					}
					_ = send([]byte(`{"type":"authorization_ended"}`))
					return
				}
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
			if !realtimePayloadMatchesScopes(msg.Data, scopes) {
				continue
			}
			visible, allowed := h.authorizeRealtimeEnvelope(c, msg.Data)
			if !allowed {
				continue
			}
			if err := send(visible); err != nil {
				return
			}
		case <-ticker.C:
			valid, err := auth.RevalidateRealtimeIdentity(c)
			if !valid {
				if err != nil {
					slog.Warn("realtime authorization refresh failed", "site", siteName, "error", err)
				}
				_ = send([]byte(`{"type":"authorization_ended"}`))
				return
			}
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
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope.Operation != string(eventstream.EventNotification) {
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

		// Users.
		system.GET("/user-directory", handler.HandleUserDirectory)
		system.GET("/users", handler.HandleUserList)
		system.GET("/users/:name", handler.HandleUserGet)
		system.PUT("/users/:name", handler.HandleUserUpdate)

		// Secrets.
		system.GET("/secrets", handler.HandleSecretList)
		system.POST("/secrets", handler.HandleSecretSet)
		system.DELETE("/secrets/:key", handler.HandleSecretDelete)
	}
}
