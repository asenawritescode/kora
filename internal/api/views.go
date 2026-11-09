package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/script"
)

// --- System View CRUD ---

// HandleSystemViews returns all views for the current site.
// GET /api/v1/system/views
func (h *Handler) HandleSystemViews(c *gin.Context) {
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
		return
	}

	views, err := store.LoadViews(site)
	if err != nil {
		internalError(c, "loading views", err)
		return
	}

	c.Header("ETag", viewsETag(views))
	c.JSON(http.StatusOK, Response{Data: views})
}

// HandleSystemView returns a single view by name.
// GET /api/v1/system/views/:name
func (h *Handler) HandleSystemView(c *gin.Context) {
	name := c.Param("name")
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
		return
	}

	view, err := store.LoadView(name, site)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "view.not_found", "View not found", map[string]any{"name": name})
		} else {
			internalError(c, "loading view", err)
		}
		return
	}

	// YAML export format.
	if c.Query("format") == "yaml" {
		yamlBytes, err := yaml.Marshal(view)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "view.serialize_failed", "Failed to serialize YAML", nil)
			return
		}
		c.Data(http.StatusOK, "text/yaml; charset=utf-8", yamlBytes)
		return
	}

	c.Header("ETag", viewETag(view))
	c.JSON(http.StatusOK, Response{Data: view})
}

// HandleSystemViewCreate creates a new view and returns a config version.
// POST /api/v1/system/views
func (h *Handler) HandleSystemViewCreate(c *gin.Context) {
	var view doctype.View
	if err := c.ShouldBindJSON(&view); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid view JSON", map[string]any{"error": err.Error()})
		return
	}

	if err := view.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"message": err.Error()})
		return
	}

	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
		return
	}

	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}
	upsertView(snapshot, &view)
	if rejectInvalidSnapshotReferences(c, snapshot, reg) {
		return
	}
	versionID, versionNum, err := saveViewDraft(store, &view, site, currentUser(c), "Created view "+view.Name, snapshot)
	if err != nil {
		internalError(c, "creating config version", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"view":        view,
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

// HandleSystemViewUpdate updates an existing view and returns a config version.
// PUT /api/v1/system/views/:name
func (h *Handler) HandleSystemViewUpdate(c *gin.Context) {
	name := c.Param("name")
	var view doctype.View
	if err := c.ShouldBindJSON(&view); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid view JSON", map[string]any{"error": err.Error()})
		return
	}

	// Enforce name consistency.
	view.Name = name

	if err := view.Validate(); err != nil {
		writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"message": err.Error()})
		return
	}

	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
		return
	}

	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}
	upsertView(snapshot, &view)
	if rejectInvalidSnapshotReferences(c, snapshot, reg) {
		return
	}
	versionID, versionNum, err := saveViewDraft(store, &view, site, currentUser(c), "Updated view "+view.Name, snapshot)
	if err != nil {
		internalError(c, "creating config version", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"view":        view,
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

func saveViewDraft(store *configstore.Store, view *doctype.View, site, user, label string, snapshot *doctype.ConfigSnapshot) (string, int, error) {
	tx, err := store.DB.Begin()
	if err != nil {
		return "", 0, fmt.Errorf("begin view draft transaction: %w", err)
	}
	defer tx.Rollback()
	if err := store.SaveViewTx(tx, view, site); err != nil {
		return "", 0, err
	}
	versionID, versionNum, err := store.CreateConfigVersionTx(tx, site, user, label, "Draft", snapshot, "")
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, fmt.Errorf("commit view draft transaction: %w", err)
	}
	return versionID, versionNum, nil
}

// HandleSystemViewDelete removes a view and returns a config version.
// DELETE /api/v1/system/views/:name
func (h *Handler) HandleSystemViewDelete(c *gin.Context) {
	name := c.Param("name")
	site := siteName(c)
	store := h.viewStore(c)
	if store == nil {
		writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
		return
	}

	if err := store.DeleteView(name, site); err != nil {
		internalError(c, "deleting view", err)
		return
	}

	// Create a config version.
	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}

	versionID, versionNum, err := store.CreateConfigVersion(site, currentUser(c), "Deleted view "+name, "Draft", snapshot)
	if err != nil {
		internalError(c, "creating config version", err)
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"version_id":  versionID,
		"version_num": versionNum,
		"status":      "Draft",
	}})
}

// HandleViewValidate validates a view config against the registry's doctypes.
// POST /api/v1/system/views/validate
func (h *Handler) HandleViewValidate(c *gin.Context) {
	var view doctype.View
	if err := c.ShouldBindJSON(&view); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid view JSON", map[string]any{"error": err.Error()})
		return
	}

	// Structural validation first.
	if err := view.Validate(); err != nil {
		c.JSON(http.StatusOK, Response{Data: map[string]any{
			"valid":  false,
			"errors": []map[string]string{{"message": err.Error()}},
		}})
		return
	}

	// Validate against registry doctypes.
	reg := h.siteRegistry(c)
	errors := validateViewAgainstRegistry(&view, reg)

	if len(errors) > 0 {
		errMsgs := make([]map[string]string, len(errors))
		for i, e := range errors {
			errMsgs[i] = map[string]string{"message": e}
		}
		c.JSON(http.StatusOK, Response{Data: map[string]any{
			"valid":  false,
			"errors": errMsgs,
		}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"valid": true,
	}})
}

// validateViewAgainstRegistry checks that component bindings reference real doctypes and fields.
func validateViewAgainstRegistry(view *doctype.View, reg *doctype.Registry) []string {
	var errors []string
	errors = append(errors, validateViewTaskDocTypes(view, nil, reg)...)

	for i := range view.Components {
		comp := &view.Components[i]
		errors = append(errors, validateComponentAgainstRegistry(comp, reg)...)
	}

	return errors
}

func validateComponentAgainstRegistry(comp *doctype.ViewComponent, reg *doctype.Registry) []string {
	var errors []string
	prefix := fmt.Sprintf("component %q", comp.ID)

	// Check source doctype exists.
	if comp.SourceDocType != "" {
		if dt := reg.Get(comp.SourceDocType); dt == nil {
			errors = append(errors, fmt.Sprintf("%s: source doctype %q not found", prefix, comp.SourceDocType))
		} else {
			// Check bindings reference real fields.
			for prop, fieldName := range comp.Bindings {
				if f := dt.GetField(fieldName); f == nil {
					// Check if it's a system field.
					if !isPublicSystemField(fieldName) {
						errors = append(errors, fmt.Sprintf("%s: binding %q references unknown field %q on %s",
							prefix, prop, fieldName, comp.SourceDocType))
					}
				}
			}
		}
	}

	// Validate nested children.
	for i := range comp.Components {
		errors = append(errors, validateComponentAgainstRegistry(&comp.Components[i], reg)...)
	}

	return errors
}

func viewsETag(views []*doctype.View) string {
	b, _ := json.Marshal(views)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func viewETag(view *doctype.View) string {
	b, _ := json.Marshal(view)
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func isPublicSystemField(name string) bool {
	switch name {
	case "name", "owner", "creation", "modified", "modified_by", "doc_status", "idx":
		return true
	default:
		return false
	}
}

// --- View Route Resolution ---

// HandleViewByRoute resolves a view by its route for authenticated users.
// GET /api/v1/views?route=/pos/register
// Optional: ?version=draft to preview the latest Draft version.
func (h *Handler) HandleViewByRoute(c *gin.Context) {
	route := c.Query("route")
	if route == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "route query parameter is required", map[string]any{"field": "route"})
		return
	}

	// Normalize: ensure leading slash.
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}

	// Draft preview: load from latest Draft config snapshot.
	if c.Query("version") == "draft" {
		store := h.viewStore(c)
		if store == nil {
			writeError(c, http.StatusInternalServerError, "server.store_unavailable", "view store not available", nil)
			return
		}
		site := siteName(c)
		var configJSON string
		err := store.DB.QueryRow(h.siteQuery(c,
			"SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Draft' ORDER BY version DESC LIMIT 1"),
			site,
		).Scan(&configJSON)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "version.not_found", "No draft version found for route", map[string]any{"route": route})
			return
		}
		if err != nil {
			internalError(c, "loading draft config for view preview", err)
			return
		}
		snapshot, err := doctype.ParseConfig(configJSON)
		if err != nil {
			internalError(c, "parsing draft config for view preview", err)
			return
		}
		for _, v := range snapshot.Views {
			if v.Route == route {
				c.JSON(http.StatusOK, Response{Data: map[string]any{
					"view":      v,
					"is_public": false,
					"draft":     true,
				}})
				return
			}
		}
		writeError(c, http.StatusNotFound, "view.not_found", "View not found in draft version", map[string]any{"route": route})
		return
	}

	reg := h.siteRegistry(c)
	view := reg.Views.GetByRoute(route)
	if view == nil {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found for route", map[string]any{"route": route})
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"view":      view,
		"is_public": false,
	}})
}

// --- Public Views ---

// HandlePublicView resolves a view by route for unauthenticated users.
// GET /v?route=/catalog
// Three-layer security check:
//  1. View allows public access for the component
//  2. Component's source doctype has public access enabled
//  3. Component bindings only reference public fields
func (h *Handler) HandlePublicView(c *gin.Context) {
	route := c.Query("route")
	if route == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "route query parameter is required", map[string]any{"field": "route"})
		return
	}

	// Normalize: ensure leading slash.
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}

	reg := h.siteRegistry(c)
	view := reg.Views.GetByRoute(route)
	if view == nil {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found", nil)
		return
	}

	// Layer 1: View allows public access.
	if view.PublicAccess == nil || !view.PublicAccess.Enabled {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found", nil)
		return
	}

	// Filter and validate components.
	filtered := make([]doctype.ViewComponent, 0)
	for _, comp := range view.Components {
		// Check if this component is in the allowed set.
		if !view.PublicAccess.AllowsComponent(comp.ID) {
			continue
		}

		// Layer 2: Source doctype must have public access enabled.
		if comp.SourceDocType != "" {
			dt := reg.Get(comp.SourceDocType)
			if dt == nil || dt.PublicAccess == nil || !dt.PublicAccess.Enabled {
				continue
			}

			// Layer 3: Strip bindings that reference non-public fields.
			publicFields := dt.PublicFieldSet()
			if comp.Bindings != nil {
				filteredBindings := make(map[string]string)
				for prop, fieldName := range comp.Bindings {
					if publicFields[fieldName] || isPublicSystemField(fieldName) {
						filteredBindings[prop] = fieldName
					}
				}
				comp.Bindings = filteredBindings
			}
		}

		// Strip mutation actions unless explicitly allowed.
		if !view.PublicAccess.AllowMutations {
			var safeActions []doctype.ViewAction
			for _, action := range comp.Actions {
				if !action.IsMutation() {
					safeActions = append(safeActions, action)
				}
			}
			comp.Actions = safeActions
		}

		filtered = append(filtered, comp)
	}

	// Return stripped view config.
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"view":       view,
		"components": filtered,
		"is_public":  true,
	}})
}

// --- Helpers ---

// ---------------------------------------------------------------------------
// View Action Execution
// ---------------------------------------------------------------------------

// HandleViewAction executes a view action server-side.
// POST /api/v1/view/action/:actionId
// The server resolves the action type and config from the stored view config.
// The client sends only context data — never chooses the action type.
type pageActionRuntime func(*Handler, *gin.Context, *doctype.ViewAction, map[string]any)

// serverPageActionRegistry is the server execution registry. Contract parity
// tests require every entry to have a matching Engine validation contract.
var serverPageActionRegistry = map[string]pageActionRuntime{
	"create_record":               (*Handler).executeCreateRecord,
	"update_record":               (*Handler).executeUpdateRecord,
	"create_transaction":          (*Handler).executeCreateTransaction,
	"create_quantity_effect":      (*Handler).executeCreateQuantityEffect,
	"initiate_external_operation": (*Handler).executeInitiateExternalOperation,
	"validate_external_operation": (*Handler).executeValidateExternalOperation,
}

func (h *Handler) HandleViewAction(c *gin.Context) {
	actionID := c.Param("actionId")

	var req struct {
		View      string         `json:"view"`
		Component string         `json:"component"`
		Context   map[string]any `json:"context"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid request", map[string]any{"error": err.Error()})
		return
	}

	if req.View == "" || req.Component == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "view and component are required", map[string]any{"fields": []string{"view", "component"}})
		return
	}

	reg := h.siteRegistry(c)
	view := reg.Views.GetByName(req.View)
	if view == nil {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found", map[string]any{"name": req.View})
		return
	}

	// Find the component and action in the stored view config.
	var targetAction *doctype.ViewAction
	var componentBindings map[string]string
	for i := range view.Components {
		if view.Components[i].ID == req.Component {
			componentBindings = view.Components[i].Bindings
			for j := range view.Components[i].Actions {
				if view.Components[i].Actions[j].ID == actionID {
					targetAction = &view.Components[i].Actions[j]
					break
				}
			}
			break
		}
	}

	if targetAction == nil {
		writeError(c, http.StatusNotFound, "action.not_found", "Action not found on component", map[string]any{
			"action_id": actionID,
			"component": req.Component,
			"view":      req.View,
		})
		return
	}

	// Resolve through the server registry based on stored config, never the client.
	if runtime, ok := serverPageActionRegistry[targetAction.Type]; ok {
		if targetAction.Type == "create_record" {
			if req.Context == nil {
				req.Context = make(map[string]any)
			}
			bindings := make(map[string]any, len(componentBindings))
			for field, value := range componentBindings {
				bindings[field] = strings.TrimPrefix(value, "literal:")
			}
			req.Context["_bindings"] = bindings
		}
		runtime(h, c, targetAction, req.Context)
	} else {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{
			"message": fmt.Sprintf("Action type %q must be executed client-side", targetAction.Type),
		}})
	}
}

func (h *Handler) executeCreateRecord(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	doctypeName := getString(action.Config, "target_doctype")
	if doctypeName == "" {
		doctypeName = getString(ctx, "_doctype")
	}
	if doctypeName == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "target_doctype is required", map[string]any{"field": "target_doctype"})
		return
	}

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "Doctype not found", map[string]any{"name": doctypeName})
		return
	}

	data := make(map[string]any)
	ctx["_actor"] = currentUser(c)
	if inputs, ok := action.Config["inputs"].([]any); ok {
		provided, _ := ctx["_inputs"].(map[string]any)
		for _, rawInput := range inputs {
			input, ok := rawInput.(map[string]any)
			if !ok {
				continue
			}
			field := getString(input, "field")
			if field == "" {
				continue
			}
			if dt.GetField(field) == nil {
				writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Configured input field does not exist on target DocType", map[string]any{"doctype": doctypeName, "field": field})
				return
			}
			value, exists := provided[field]
			if value == nil || value == "" {
				value, exists = ctx[field]
			}
			if input["required"] == true && (!exists || value == nil || value == "") {
				writeError(c, http.StatusBadRequest, "validation.required_field", "A required action input is missing", map[string]any{"field": field, "label": getString(input, "label")})
				return
			}
		}
	}
	for k, v := range ctx {
		if !strings.HasPrefix(k, "_") && dt.GetField(k) != nil {
			data[k] = v
		}
	}
	if configuredFields, ok := action.Config["fields"].(map[string]any); ok {
		for field, template := range configuredFields {
			if dt.GetField(field) == nil {
				writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Configured create field does not exist on target DocType", map[string]any{"doctype": doctypeName, "field": field})
				return
			}
			value, err := resolveCreateRecordTemplate(template, ctx, action.Config)
			if err != nil {
				writeError(c, http.StatusBadRequest, "validation.invalid_action_context", err.Error(), map[string]any{"field": field})
				return
			}
			if value != nil {
				data[field] = value
			}
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid action data", nil)
		return
	}
	doc, cerr := h.runKernelResourceMutation(c, kernel.CommandRecordCreate, doctypeName, "", raw)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}

	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
}

func resolveCreateRecordTemplate(raw any, ctx map[string]any, config map[string]any) (any, error) {
	template, ok := raw.(string)
	if !ok || !strings.HasPrefix(template, "$") {
		return raw, nil
	}
	switch template {
	case "$generated":
		prefix := getString(config, "reference_prefix")
		if prefix == "" {
			prefix = "REC"
		}
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()), nil
	case "$now":
		return time.Now().UTC().Format(time.RFC3339), nil
	case "$actor":
		return currentUserFromContext(ctx), nil
	}
	for _, scope := range []string{"binding", "context", "input"} {
		prefix := "$" + scope + "."
		if strings.HasPrefix(template, prefix) {
			key := strings.TrimPrefix(template, prefix)
			var source any
			switch scope {
			case "binding":
				source = ctx["_bindings"]
			case "context":
				source = ctx["_prerequisites"]
			case "input":
				source = ctx["_inputs"]
			}
			values, ok := source.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("missing %s values for configured field", scope)
			}
			value, exists := values[key]
			if !exists || value == nil || value == "" {
				if scope == "input" {
					return nil, nil
				}
				return nil, fmt.Errorf("missing configured %s value %q", scope, key)
			}
			return value, nil
		}
	}
	return raw, nil
}

func currentUserFromContext(ctx map[string]any) string {
	if actor, ok := ctx["_actor"].(string); ok {
		return actor
	}
	return ""
}

func (h *Handler) executeUpdateRecord(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	doctypeName := getString(action.Config, "target_doctype")
	if doctypeName == "" {
		doctypeName = getString(ctx, "_doctype")
	}
	name := getString(ctx, "name")
	if doctypeName == "" || name == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "target_doctype and name are required", map[string]any{"fields": []string{"target_doctype", "name"}})
		return
	}

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "Doctype not found", map[string]any{"name": doctypeName})
		return
	}

	data := make(map[string]any)
	for k, v := range ctx {
		if !strings.HasPrefix(k, "_") && k != "name" && dt.GetField(k) != nil {
			data[k] = v
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid action data", nil)
		return
	}
	existing, cerr := h.runKernelResourceMutation(c, kernel.CommandRecordUpdate, doctypeName, name, raw)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}

	c.JSON(http.StatusOK, Response{Data: documentToMap(existing, dt)})
}

func (h *Handler) executeCreateTransaction(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	if allowed, ok := action.Config["allowed_context_values"].(map[string]any); ok {
		for field, raw := range allowed {
			choices, ok := raw.([]any)
			if !ok || len(choices) == 0 {
				writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Transaction eligibility has no allowed values", map[string]any{"field": field})
				return
			}
			accepted := false
			for _, choice := range choices {
				accepted = accepted || fmt.Sprint(ctx[field]) == fmt.Sprint(choice)
			}
			if !accepted {
				writeError(c, http.StatusBadRequest, "validation.failed", "This transaction option is not available", map[string]any{"field": field})
				return
			}
		}
	}
	targetDoctype := getString(action.Config, "target_doctype")
	if targetDoctype == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "target_doctype is required", map[string]any{"field": "target_doctype"})
		return
	}

	reg := h.siteRegistry(c)
	dt := reg.Get(targetDoctype)
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "Doctype not found", map[string]any{"name": targetDoctype})
		return
	}

	user := currentUser(c)
	tm := h.siteTx(c)

	// Build parent document from context (excluding cart/items transport fields).
	doc := doctype.NewDocument("")
	for k, v := range ctx {
		if k != "cart" && k != "items" && k != "total" && !strings.HasPrefix(k, "_") {
			doc.Set(k, v)
		}
	}
	// Required identifiers are generated by the configured transaction action,
	// not by the browser. This keeps the contract reusable for every client.
	if referenceField := getString(action.Config, "reference_field"); referenceField != "" && doc.GetString(referenceField) == "" {
		prefix := getString(action.Config, "reference_prefix")
		if prefix == "" {
			prefix = targetDoctype
		}
		doc.Set(referenceField, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
	}
	if configuredFields, ok := action.Config["fields"].(map[string]any); ok {
		for field, value := range configuredFields {
			switch value {
			case "$today":
				value = time.Now().Format("2006-01-02")
			case "$now":
				value = time.Now().Format(time.RFC3339)
			}
			if doc.Get(field) == nil {
				doc.Set(field, value)
			}
		}
	}
	// Run the same DocType defaults used by ordinary record creation before
	// validating a configured transaction. Required fields can have defaults
	// even when they are not part of the action's client context.
	doctype.ApplyDefaultValues(dt, doc)
	stockUpdates := make([]kernel.RecordMutationBundleItem, 0)

	parentField, childDT, err := resolveTransactionChildTable(reg, dt, action)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
		return
	}

	var children []*doctype.Document
	if childDT != nil {
		rawItems := ctx["cart"]
		if rawItems == nil {
			rawItems = ctx["items"]
		}
		children, err = buildTransactionChildren(rawItems, childDT, action.Config)
		if err != nil {
			writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
			return
		}
		for _, child := range children {
			doctype.ApplyDefaultValues(childDT, child)
		}
		if len(children) == 0 {
			writeError(c, http.StatusBadRequest, "validation.required_field", "transaction requires at least one item", map[string]any{"field": "items"})
			return
		}
		if err := applyTransactionLinkedFields(tm, reg, childDT, children); err != nil {
			writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
			return
		}
		doc.SetTable(parentField, children)
		if totalErrs := applyTransactionTotals(dt, doc, children, action.Config); totalErrs.HasErrors() {
			writeError(c, http.StatusBadRequest, "validation.failed", "One or more transaction amounts are invalid", map[string]any{"fields": validationErrorDetails(totalErrs)})
			return
		}
		if validationErrs := validateTransactionChildren(childDT, children, reg); validationErrs.HasErrors() {
			writeError(c, http.StatusBadRequest, "validation.failed", "One or more transaction lines are invalid", map[string]any{"fields": validationErrorDetails(validationErrs)})
			return
		}
		if action.Config["decrement_stock"] == true && action.Config["related_quantity_updates"] == nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Transaction action requires related quantity bindings", nil)
			return
		}
		if bindings, ok := action.Config["related_quantity_updates"].(map[string]any); ok {
			typeName := getString(bindings, "target_doctype")
			quantityField := getString(bindings, "target_quantity_field")
			recordField := getString(bindings, "source_record_field")
			lineQuantityField := getString(bindings, "source_quantity_field")
			targetDT := reg.Get(typeName)
			if typeName == "" || quantityField == "" || recordField == "" || lineQuantityField == "" || targetDT == nil || targetDT.GetField(quantityField) == nil {
				writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Transaction action has invalid related quantity bindings", nil)
				return
			}
			quantities := make(map[string]float64)
			for _, line := range children {
				quantities[line.GetString(recordField)] += line.GetFloat(lineQuantityField)
			}
			names := make([]string, 0, len(quantities))
			for name := range quantities {
				names = append(names, name)
			}
			sort.Strings(names) // stable kernel payload for idempotent replay
			for _, name := range names {
				quantity := quantities[name]
				if name == "" || quantity <= 0 || bindings["whole_units"] == true && quantity != float64(int64(quantity)) {
					writeError(c, http.StatusBadRequest, "validation.failed", "Related quantities must be positive and valid", map[string]any{"record": name})
					return
				}
				data, marshalErr := json.Marshal(map[string]any{quantityField: quantity})
				if marshalErr != nil {
					writeError(c, http.StatusInternalServerError, "internal.error", "Could not prepare related quantity update", nil)
					return
				}
				stockUpdates = append(stockUpdates, kernel.RecordMutationBundleItem{Key: "quantity_" + name, Operation: "decrement", Doctype: targetDT.Name, Name: name, Data: data})
			}
		}
	}

	if requiredStatus := getString(action.Config, "requires_operation_status"); requiredStatus != "" {
		operationName := getString(ctx, "external_operation")
		if operationName == "" {
			writeError(c, http.StatusBadRequest, "validation.required_field", "external_operation is required before completing this transaction", map[string]any{"field": "external_operation"})
			return
		}
		operationDT := reg.Get("External Operation")
		if operationDT == nil {
			writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "External Operation doctype is not available", nil)
			return
		}
		operation, err := tm.GetDoc(operationDT, operationName, "")
		if err != nil || operation.GetString("status") != requiredStatus {
			writeError(c, http.StatusBadRequest, "validation.failed", "payment has not been confirmed", nil)
			return
		}
	}

	// Validate the assembled transaction before calling an external provider;
	// an invalid cart must never trigger a charge or payment prompt.
	if validationErrs := doctype.ValidateDocument(dt, doc, reg, nil); validationErrs.HasErrors() {
		writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"fields": validationErrorDetails(validationErrs)})
		return
	}
	if statusErr := validateTransactionRelatedStatuses(tm, reg, dt, doc, action.Config); statusErr != nil {
		writeError(c, http.StatusConflict, "validation.related_record_status", statusErr.Error(), nil)
		return
	}

	// A provider must approve payment before the Sale exists. The script name
	// comes from stored view configuration, never from client input.
	if scriptName := getString(action.Config, "payment_script"); scriptName != "" {
		if err := h.executeTransactionPaymentScript(c, scriptName, dt, doc); err != nil {
			writeError(c, http.StatusBadRequest, "payment.failed", err.Error(), nil)
			return
		}
	}

	operationName := getString(ctx, "external_operation")
	saleData, dataErr := transactionDocumentData(dt, doc)
	if dataErr != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid transaction data", nil)
		return
	}
	bundle := kernel.RecordMutationBundlePayload{
		Doctype: dt.Name,
		Records: []kernel.RecordMutationBundleItem{{Key: "sale", Doctype: dt.Name, Data: saleData}},
	}
	bundle.Records = append(bundle.Records, stockUpdates...)
	lineRecords, lineRecordErr := configuredTransactionLineRecords(reg, action.Config, dt, doc, childDT, children, user)
	if lineRecordErr != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_configuration", lineRecordErr.Error(), nil)
		return
	}
	bundle.Records = append(bundle.Records, lineRecords...)
	counterUpdates, counterErr := configuredTransactionNumericUpdates(reg, action.Config, dt, doc)
	if counterErr != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_configuration", counterErr.Error(), nil)
		return
	}
	bundle.Records = append(bundle.Records, counterUpdates...)
	if operationName == "" {
		related, relatedErr := configuredTransactionRelatedRecords(reg, action.Config, ctx, user)
		if relatedErr != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_configuration", relatedErr.Error(), nil)
			return
		}
		bundle.Records = append(bundle.Records, related...)
	} else if paymentDT := reg.Get("Payment"); paymentDT != nil {
		// The provider operation and its Payment already exist. Complete the
		// Sale and link Payment atomically so either both become final or neither.
		if paymentName, lookupErr := linkedPaymentName(h, c, h.queryDB(c), operationName); lookupErr == nil && paymentName != "" {
			paymentChanges, encodeErr := json.Marshal(map[string]any{"sale": "$records.sale.name", "status": "Succeeded"})
			if encodeErr != nil {
				writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid payment data", nil)
				return
			}
			bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{
				Key: "payment", Operation: "update", Doctype: paymentDT.Name, Name: paymentName, Data: paymentChanges,
			})
		}
	}
	idempotencyKey, keyErr := transactionIdempotencyKey(c, action, doc)
	if keyErr != nil {
		writeError(c, http.StatusBadRequest, "validation.required_field", keyErr.Error(), map[string]any{"field": "Idempotency-Key"})
		return
	}
	result, _, cerr := h.runKernelMutationBundle(c, bundle, idempotencyKey)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	doc = orm.DocumentFromMap(reg, dt.Name, result.Document)

	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
}

func validateTransactionRelatedStatuses(tx *orm.TxManager, reg *doctype.Registry, parentDT *doctype.DocType, parent *doctype.Document, config map[string]any) error {
	items, ok := config["required_related_status"].([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("related status requirement %d must be an object", index+1)
		}
		targetDT := reg.Get(getString(item, "doctype"))
		recordField, statusField, expected := getString(item, "record_field"), getString(item, "status_field"), getString(item, "equals")
		if targetDT == nil || parentDT.GetField(recordField) == nil || targetDT.GetField(statusField) == nil || expected == "" {
			return fmt.Errorf("related status requirement %d is incomplete", index+1)
		}
		recordName := parent.GetString(recordField)
		if recordName == "" {
			return fmt.Errorf("%s is required before this transaction can be completed", parentDT.GetField(recordField).Label)
		}
		related, err := tx.GetDoc(targetDT, recordName, "")
		if err != nil || related == nil {
			return fmt.Errorf("the selected %s could not be found", targetDT.Name)
		}
		if !strings.EqualFold(related.GetString(statusField), expected) {
			return fmt.Errorf("the selected %s is not %s", targetDT.Name, expected)
		}
	}
	return nil
}

// configuredTransactionLineRecords creates one configured companion record per
// transaction line. This supports generic ledgers such as stock movements
// without embedding POS-specific rules in the transaction engine.
func configuredTransactionLineRecords(reg *doctype.Registry, config map[string]any, parentDT *doctype.DocType, parent *doctype.Document, lineDT *doctype.DocType, lines []*doctype.Document, actor string) ([]kernel.RecordMutationBundleItem, error) {
	raw, ok := config["line_records"].([]any)
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	if lineDT == nil || len(lines) == 0 {
		return nil, fmt.Errorf("line_records requires transaction lines")
	}
	items := make([]kernel.RecordMutationBundleItem, 0, len(raw)*len(lines))
	for bindingIndex, rawBinding := range raw {
		binding, ok := rawBinding.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("line record binding %d must be an object", bindingIndex+1)
		}
		targetName := getString(binding, "doctype")
		targetDT := reg.Get(targetName)
		fields, fieldsOK := binding["fields"].(map[string]any)
		if targetDT == nil || !fieldsOK || len(fields) == 0 {
			return nil, fmt.Errorf("line record binding %d requires an available doctype and fields", bindingIndex+1)
		}
		prefix := getString(binding, "key")
		if prefix == "" {
			prefix = fmt.Sprintf("line_record_%d", bindingIndex+1)
		}
		quantity, _ := binding["quantity"].(map[string]any)
		for lineIndex, line := range lines {
			data := make(map[string]any, len(fields)+1)
			for field, source := range fields {
				value := source
				if expression, isExpression := source.(string); isExpression {
					switch {
					case expression == "$actor":
						value = actor
					case strings.HasPrefix(expression, "$line."):
						name := strings.TrimPrefix(expression, "$line.")
						if lineDT.GetField(name) == nil {
							return nil, fmt.Errorf("line record binding references unknown line field %q", name)
						}
						value = line.Get(name)
					case strings.HasPrefix(expression, "$parent."):
						name := strings.TrimPrefix(expression, "$parent.")
						if parentDT.GetField(name) == nil {
							return nil, fmt.Errorf("line record binding references unknown transaction field %q", name)
						}
						value = parent.Get(name)
					}
				}
				data[field] = value
			}
			if len(quantity) > 0 {
				fieldName := getString(quantity, "field")
				sourceName := getString(quantity, "source_field")
				sign, signErr := strconv.ParseFloat(fmt.Sprint(quantity["sign"]), 64)
				amount := line.GetFloat(sourceName)
				if targetDT.GetField(fieldName) == nil || lineDT.GetField(sourceName) == nil || signErr != nil || (sign != 1 && sign != -1) || amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
					return nil, fmt.Errorf("line record binding %d has an invalid signed quantity mapping", bindingIndex+1)
				}
				data[fieldName] = amount * sign
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return nil, fmt.Errorf("encode line record: %w", err)
			}
			items = append(items, kernel.RecordMutationBundleItem{
				Key: fmt.Sprintf("%s_%d", prefix, lineIndex+1), Doctype: targetDT.Name, Data: encoded,
			})
		}
	}
	if len(items)+1 > kernel.MaxMutationBundleRecords {
		return nil, fmt.Errorf("transaction creates too many related records")
	}
	return items, nil
}

// configuredTransactionNumericUpdates applies configured signed deltas to
// related numeric fields in the same kernel bundle as the transaction.
func configuredTransactionNumericUpdates(reg *doctype.Registry, config map[string]any, parentDT *doctype.DocType, parent *doctype.Document) ([]kernel.RecordMutationBundleItem, error) {
	raw, ok := config["related_numeric_updates"].([]any)
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	items := make([]kernel.RecordMutationBundleItem, 0, len(raw))
	for index, rawUpdate := range raw {
		update, ok := rawUpdate.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("numeric update %d must be an object", index+1)
		}
		targetDT := reg.Get(getString(update, "doctype"))
		recordField, fieldName := getString(update, "record_field"), getString(update, "field")
		if targetDT == nil || parentDT.GetField(recordField) == nil || targetDT.GetField(fieldName) == nil || !targetDT.GetField(fieldName).IsNumeric() {
			return nil, fmt.Errorf("numeric update %d has an invalid doctype or field", index+1)
		}
		if condition, ok := update["when"].(map[string]any); ok {
			conditionField := getString(condition, "field")
			if parentDT.GetField(conditionField) == nil {
				return nil, fmt.Errorf("numeric update %d references an unknown condition field", index+1)
			}
			if fmt.Sprint(parent.Get(conditionField)) != fmt.Sprint(condition["equals"]) {
				continue
			}
		}
		recordName := parent.GetString(recordField)
		if recordName == "" {
			return nil, fmt.Errorf("numeric update %d requires transaction field %q", index+1, recordField)
		}
		amount := 0.0
		if sourceName := getString(update, "source_field"); sourceName != "" {
			if parentDT.GetField(sourceName) == nil {
				return nil, fmt.Errorf("numeric update %d references an unknown source field", index+1)
			}
			amount = parent.GetFloat(sourceName)
		} else {
			parsed, err := strconv.ParseFloat(fmt.Sprint(update["value"]), 64)
			if err != nil {
				return nil, fmt.Errorf("numeric update %d has an invalid constant", index+1)
			}
			amount = parsed
		}
		sign := 1.0
		if rawSign := update["sign"]; rawSign != nil {
			parsed, err := strconv.ParseFloat(fmt.Sprint(rawSign), 64)
			if err != nil || (parsed != 1 && parsed != -1) {
				return nil, fmt.Errorf("numeric update %d sign must be 1 or -1", index+1)
			}
			sign = parsed
		}
		amount *= sign
		if amount == 0 {
			continue
		}
		if math.IsNaN(amount) || math.IsInf(amount, 0) || targetDT.GetField(fieldName).Fieldtype == "Int" && math.Trunc(amount) != amount {
			return nil, fmt.Errorf("numeric update %d must be a nonzero valid amount", index+1)
		}
		encoded, err := json.Marshal(map[string]any{fieldName: amount})
		if err != nil {
			return nil, fmt.Errorf("encode numeric update: %w", err)
		}
		items = append(items, kernel.RecordMutationBundleItem{
			Key: fmt.Sprintf("counter_%d", index+1), Operation: "adjust", Doctype: targetDT.Name, Name: recordName, Data: encoded,
		})
	}
	return items, nil
}

func transactionIdempotencyKey(c *gin.Context, action *doctype.ViewAction, doc *doctype.Document) (string, error) {
	prefix := getString(action.Config, "idempotency_prefix")
	if prefix == "" {
		prefix = "view-action:" + action.ID
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(key) > 256 {
		return "", fmt.Errorf("Idempotency-Key must be at most 256 bytes")
	}
	if key == "" {
		referenceField := getString(action.Config, "reference_field")
		if referenceField == "" || doc.GetString(referenceField) == "" {
			return "", fmt.Errorf("Idempotency-Key is required unless the transaction has a configured reference field")
		}
		key = doc.GetString(referenceField)
	}
	return prefix + ":" + key, nil
}

// applyTransactionLinkedFields makes configured linked values authoritative at
// checkout. A child field such as unit_price can declare linked_field:
// "product.selling_price"; the browser's cart value is then ignored.
func applyTransactionLinkedFields(tx *orm.TxManager, registry *doctype.Registry, childDT *doctype.DocType, children []*doctype.Document) error {
	if tx == nil || childDT == nil {
		return nil
	}
	type linkedValue struct {
		doc *doctype.Document
	}
	cache := make(map[string]linkedValue)
	for _, targetField := range childDT.NonTableDataFields() {
		if strings.TrimSpace(targetField.LinkedField) == "" {
			continue
		}
		linkName, sourceName, ok := strings.Cut(targetField.LinkedField, ".")
		linkField := childDT.GetField(linkName)
		if !ok || linkName == "" || sourceName == "" || linkField == nil || !strings.EqualFold(linkField.Fieldtype, "Link") {
			return fmt.Errorf("linked field configuration for %s.%s must use a Link field and source field", childDT.Name, targetField.Fieldname)
		}
		linkedDT := registry.Get(linkField.Options)
		if linkedDT == nil || linkedDT.GetField(sourceName) == nil {
			return fmt.Errorf("linked field configuration for %s.%s references an unavailable field", childDT.Name, targetField.Fieldname)
		}
		for index, child := range children {
			name := child.GetString(linkName)
			if name == "" {
				continue
			}
			cacheKey := linkedDT.Name + "\x00" + name
			entry, exists := cache[cacheKey]
			if !exists {
				doc, err := tx.GetDoc(linkedDT, name, "")
				if err != nil || doc == nil {
					return fmt.Errorf("transaction line %d references a product that is no longer available", index+1)
				}
				entry = linkedValue{doc: doc}
				cache[cacheKey] = entry
			}
			child.Set(targetField.Fieldname, entry.doc.Get(sourceName))
		}
	}
	return nil
}

func validateTransactionChildren(childDT *doctype.DocType, children []*doctype.Document, registry *doctype.Registry) doctype.ValidationErrors {
	var all doctype.ValidationErrors
	for index, child := range children {
		for _, field := range childDT.NonTableDataFields() {
			if !field.IsNumeric() {
				continue
			}
			value := child.Get(field.Fieldname)
			if value == nil || value == "" {
				continue
			}
			parsed, err := strconv.ParseFloat(fmt.Sprint(value), 64)
			if err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) && (!strings.EqualFold(field.Fieldtype, "Int") || parsed == math.Trunc(parsed)) {
				continue
			}
			message := fmt.Sprintf("Item %d: %s must be a finite number.", index+1, field.Label)
			if strings.EqualFold(field.Fieldtype, "Int") {
				message = fmt.Sprintf("Item %d: %s must be a whole number.", index+1, field.Label)
			}
			all = append(all, &doctype.ValidationError{
				Type: "ValidationError", Message: message,
				Field: field.Fieldname, DocType: childDT.Name,
			})
		}
		for _, validationErr := range doctype.ValidateDocument(childDT, child, registry, nil) {
			all = append(all, &doctype.ValidationError{
				Type: validationErr.Type, Message: fmt.Sprintf("Item %d: %s", index+1, validationErr.Message),
				Field: validationErr.Field, DocType: validationErr.DocType, Predicate: validationErr.Predicate,
			})
		}
	}
	return all
}

// configuredTransactionRelatedRecords builds companion records from the view
// contract. Context bindings preserve the source JSON value type; record-name
// placeholders are resolved later by the kernel within the same transaction.
func configuredTransactionRelatedRecords(reg *doctype.Registry, config, ctx map[string]any, actor string) ([]kernel.RecordMutationBundleItem, error) {
	raw, ok := config["related_records"].([]any)
	if !ok || len(raw) == 0 {
		return nil, nil
	}
	items := make([]kernel.RecordMutationBundleItem, 0, len(raw))
	for _, value := range raw {
		binding, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("related record binding must be an object")
		}
		key, typeName := getString(binding, "key"), getString(binding, "doctype")
		if key == "" || typeName == "" || reg.Get(typeName) == nil {
			return nil, fmt.Errorf("related record key and configured doctype are required")
		}
		fields, ok := binding["fields"].(map[string]any)
		if !ok || len(fields) == 0 {
			return nil, fmt.Errorf("related record fields are required")
		}
		data := make(map[string]any, len(fields))
		for field, source := range fields {
			if expression, ok := source.(string); ok {
				switch {
				case expression == "$actor":
					data[field] = actor
				case strings.HasPrefix(expression, "$context."):
					name := strings.TrimPrefix(expression, "$context.")
					value, exists := ctx[name]
					if !exists {
						return nil, fmt.Errorf("related record context field %q is missing", name)
					}
					data[field] = value
				default:
					data[field] = source
				}
			} else {
				data[field] = source
			}
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("encode related record %q: %w", key, err)
		}
		items = append(items, kernel.RecordMutationBundleItem{Key: key, Doctype: typeName, Data: encoded})
	}
	return items, nil
}

// executeCreateQuantityEffect records a signed, reasoned adjustment and its
// configured quantity change as one idempotent kernel operation.
func (h *Handler) executeCreateQuantityEffect(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	config := action.Config
	reg := h.siteRegistry(c)
	rootName := getString(config, "target_doctype")
	relatedName := getString(config, "related_doctype")
	recordField := getString(config, "source_record_field")
	deltaField := getString(config, "delta_field")
	reasonField := getString(config, "reason_field")
	quantityField := getString(config, "related_quantity_field")
	rootDT, relatedDT := reg.Get(rootName), reg.Get(relatedName)
	if rootDT == nil || relatedDT == nil || rootDT.GetField(recordField) == nil || rootDT.GetField(deltaField) == nil || rootDT.GetField(reasonField) == nil || relatedDT.GetField(quantityField) == nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_configuration", "Quantity effect bindings are incomplete", nil)
		return
	}
	itemName := getString(ctx, recordField)
	reason := strings.TrimSpace(getString(ctx, reasonField))
	delta, err := strconv.ParseFloat(fmt.Sprint(ctx[deltaField]), 64)
	if itemName == "" || reason == "" || err != nil || delta == 0 || math.IsNaN(delta) || math.IsInf(delta, 0) || relatedDT.GetField(quantityField).Fieldtype == "Int" && math.Trunc(delta) != delta {
		writeError(c, http.StatusBadRequest, "validation.failed", "A linked record, nonzero signed quantity, and reason are required", nil)
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "Idempotency-Key is required", nil)
		return
	}
	data, _ := json.Marshal(ctx)
	change, _ := json.Marshal(map[string]any{quantityField: delta})
	bundle := kernel.RecordMutationBundlePayload{Doctype: rootDT.Name, Records: []kernel.RecordMutationBundleItem{
		{Key: "record", Doctype: rootDT.Name, Data: data},
		{Key: "quantity", Operation: "adjust", Doctype: relatedDT.Name, Name: itemName, Data: change},
	}}
	result, _, cerr := h.runKernelMutationBundle(c, bundle, key)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	doc := orm.DocumentFromMap(reg, rootDT.Name, result.Document)
	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, rootDT)})
}

func (h *Handler) executeInitiateExternalOperation(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	reg := h.siteRegistry(c)
	dt := reg.Get("External Operation")
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "External Operation doctype is not available", nil)
		return
	}
	scriptName := getString(action.Config, "script")
	doc := doctype.NewDocument("")
	setDefault := func(field, value string) {
		if value != "" {
			doc.Set(field, value)
		}
	}
	setDefault("operation_type", getString(action.Config, "operation_type"))
	setDefault("purpose", getString(action.Config, "purpose"))
	setDefault("source_doctype", getString(action.Config, "source_doctype"))
	setDefault("provider", getString(action.Config, "provider"))
	if doc.GetString("operation_type") == "" {
		doc.Set("operation_type", "Payment")
	}
	if doc.GetString("purpose") == "" {
		doc.Set("purpose", "POS payment")
	}
	if doc.GetString("source_doctype") == "" {
		doc.Set("source_doctype", "Sale")
	}
	if doc.GetString("provider") == "" {
		doc.Set("provider", "M-Pesa")
	}
	initialStatus := "Initiating"
	if scriptName == "" {
		initialStatus = "Pending"
	}
	doc.Set("status", initialStatus)
	doc.Set("currency", getString(action.Config, "currency"))
	if doc.GetString("currency") == "" {
		currency, err := h.tenantCurrency(c)
		if err != nil {
			internalError(c, "reading tenant currency", err)
			return
		}
		doc.Set("currency", currency)
	}
	if value, ok := ctx["total"]; ok {
		doc.Set("amount", value)
	}
	if value, ok := ctx["customer_phone"]; ok {
		doc.Set("contact_reference", value)
	}
	if value, ok := ctx["client_reference"]; ok {
		doc.Set("idempotency_key", value)
	}
	if doc.GetString("idempotency_key") == "" {
		doc.Set("idempotency_key", fmt.Sprintf("%s-%d", currentUser(c), time.Now().UnixNano()))
	}
	doc.Set("request_payload", ctx)
	doc.Set("initiated_by", currentUser(c))

	tm := h.siteTx(c)
	user := currentUser(c)
	paymentDT := reg.Get("Payment")
	if paymentDT != nil {
		doc.Set("source_doctype", "Payment")
		doc.Set("source_name", "$records.payment.name")
		operationData, encodeErr := transactionDocumentData(dt, doc)
		if encodeErr != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid operation data", nil)
			return
		}
		paymentValues := map[string]any{
			"reference": "PAY-$records.operation.name", "amount": ctx["total"],
			"method": ctx["payment_method"], "phone_number": ctx["customer_phone"],
			"external_operation": "$records.operation.name", "status": "Pending", "assignee": user,
		}
		paymentData, encodeErr := configuredFieldData(paymentDT, paymentValues)
		if encodeErr != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid payment data", nil)
			return
		}
		bundle := kernel.RecordMutationBundlePayload{
			Doctype: dt.Name,
			Records: []kernel.RecordMutationBundleItem{
				{Key: "operation", Doctype: dt.Name, Data: operationData},
				{Key: "payment", Doctype: paymentDT.Name, Data: paymentData},
			},
		}
		if scriptName == "" {
			if eventDT := reg.Get("External Operation Event"); eventDT != nil {
				eventOperation := *doc
				eventOperation.Name = "$records.operation.name"
				eventKey := "external-operation-init:" + doc.GetString("idempotency_key")
				eventData, encodeErr := externalOperationEventData(eventDT, &eventOperation, "Outbound", "Initiate", "Initiating", "Pending", ctx, nil, "Processed", "", eventKey, time.Time{})
				if encodeErr != nil {
					writeError(c, http.StatusInternalServerError, "internal.error", "Encoding operation initiation event failed", nil)
					return
				}
				bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{Key: "event", Doctype: eventDT.Name, Data: eventData})
			}
		}
		result, replayed, cerr := h.runKernelMutationBundle(c, bundle, "external-operation:"+doc.GetString("idempotency_key"))
		if cerr != nil {
			h.writeKernelError(c, cerr)
			return
		}
		doc = orm.DocumentFromMap(reg, dt.Name, result.Document)
		doc.Set("initiated_at", time.Now().UTC())
		if replayed {
			if current, getErr := tm.GetDoc(dt, doc.Name, ""); getErr == nil {
				doc = current
			}
			c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
			return
		}
	} else {
		operationData, encodeErr := transactionDocumentData(dt, doc)
		if encodeErr != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid operation data", nil)
			return
		}
		created, cerr := h.runKernelResourceMutationWithKey(c, kernel.CommandRecordCreate, dt.Name, "", operationData, "external-operation:"+doc.GetString("idempotency_key"))
		if cerr != nil {
			h.writeKernelError(c, cerr)
			return
		}
		doc = created
		if c.Writer.Header().Get("X-Kora-Replay") == "true" {
			if current, getErr := tm.GetDoc(dt, doc.Name, ""); getErr == nil {
				doc = current
			}
			c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
			return
		}
	}
	if scriptName != "" {
		doc.Set("_mode", "initiate")
		result, err := h.executeNamedOperationScript(c, scriptName, doc)
		delete(doc.Fields, "_mode")
		if err != nil {
			if failureErr := h.commitExternalOperationFailure(c, dt, doc, ctx, err.Error()); failureErr != nil {
				h.writeKernelError(c, failureErr)
				return
			}
			writeError(c, http.StatusBadRequest, "operation.failed", err.Error(), nil)
			return
		}
		applyOperationScriptResult(doc, result)
		if outcomeErr := h.commitExternalOperationSuccess(c, dt, doc, ctx, "Initiating"); outcomeErr != nil {
			h.writeKernelError(c, outcomeErr)
			return
		}
		if current, getErr := tm.GetDoc(dt, doc.Name, ""); getErr == nil {
			doc = current
		}
	} else {
		// With no external script there is no provider I/O phase: operation,
		// companion payment, and initiation event were committed together above.
		c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
		return
	}
	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
}

func (h *Handler) executeValidateExternalOperation(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	reg := h.siteRegistry(c)
	dt := reg.Get("External Operation")
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "External Operation doctype is not available", nil)
		return
	}
	name := getString(ctx, "operation_id")
	if name == "" {
		name = getString(ctx, "external_operation")
	}
	if name == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "operation_id is required", map[string]any{"field": "operation_id"})
		return
	}
	tm := h.siteTx(c)
	doc, err := tm.GetDoc(dt, name, "")
	if err != nil {
		writeError(c, http.StatusNotFound, "resource.document_not_found", "External Operation not found", nil)
		return
	}
	if scriptName := getString(action.Config, "script"); scriptName != "" {
		previousStatus := doc.GetString("status")
		doc.Set("_mode", "validate")
		result, scriptErr := h.executeNamedOperationScript(c, scriptName, doc)
		delete(doc.Fields, "_mode")
		if scriptErr != nil {
			doc.Set("status", "Failed")
			doc.Set("error_message", scriptErr.Error())
		} else {
			applyOperationScriptResult(doc, result)
		}
		if err := h.saveExternalOperation(c, dt, doc, ""); err != nil {
			h.writeKernelError(c, err)
			return
		}
		// The audit event is best-effort once the operation has been saved; failures are logged inside.
		_ = h.recordExternalOperationEvent(c, doc, "Outbound", "Status Check", previousStatus, doc.GetString("status"), ctx, doc.Get("response_payload"), "Processed", "")
		if scriptErr != nil {
			writeError(c, http.StatusBadRequest, "operation.failed", scriptErr.Error(), nil)
			return
		}
	}
	if getString(action.Config, "script") == "" {
		_ = h.recordExternalOperationEvent(c, doc, "Internal", "Status Check", doc.GetString("status"), doc.GetString("status"), ctx, nil, "Processed", "")
	}
	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
}

func (h *Handler) recordExternalOperationEvent(c *gin.Context, operation *doctype.Document, direction, eventType, previousStatus, newStatus string, requestPayload, responsePayload any, processingStatus, errorMessage string) *contract.Error {
	reg := h.siteRegistry(c)
	dt := reg.Get("External Operation Event")
	if dt == nil || operation == nil || operation.Name == "" {
		return nil
	}
	eventKey := fmt.Sprintf("%s:%s:%d", operation.Name, strings.ToLower(strings.ReplaceAll(eventType, " ", "-")), time.Now().UnixNano())
	data, err := externalOperationEventData(dt, operation, direction, eventType, previousStatus, newStatus, requestPayload, responsePayload, processingStatus, errorMessage, eventKey, time.Now().UTC())
	if err != nil {
		slog.Warn("external operation event could not be encoded", "operation", operation.Name, "event", eventType, "error", err)
		return contract.NewError(contract.CodeInternal, "encoding operation event failed")
	}
	if _, err := h.runKernelResourceMutationWithKey(c, kernel.CommandRecordCreate, dt.Name, "", data, "operation-event:"+eventKey); err != nil {
		slog.Warn("external operation event could not be recorded", "operation", operation.Name, "event", eventType, "error", err)
		return err
	}
	return nil
}

// commitExternalOperationFailure records the provider failure only after the
// external call has returned. It updates the operation, any companion payment,
// and its event in one kernel-owned transaction so a provider error cannot
// leave the customer-facing payment pending while its operation is failed.
func (h *Handler) commitExternalOperationFailure(c *gin.Context, operationDT *doctype.DocType, operation *doctype.Document, requestPayload any, failureMessage string) *contract.Error {
	if operationDT == nil || operation == nil || operation.Name == "" {
		return contract.NewError(contract.CodeValidationFailed, "external operation is required")
	}
	operation.Set("status", "Failed")
	operation.Set("error_message", failureMessage)
	operationData, err := transactionDocumentData(operationDT, operation)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding failed external operation")
	}
	bundle := kernel.RecordMutationBundlePayload{
		Doctype: operationDT.Name,
		Records: []kernel.RecordMutationBundleItem{{
			Key: "operation", Operation: "update", Doctype: operationDT.Name, Name: operation.Name, Data: operationData,
		}},
	}
	if paymentDT := h.siteRegistry(c).Get("Payment"); paymentDT != nil {
		paymentName, lookupErr := linkedPaymentName(h, c, h.queryDB(c), operation.Name)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return contract.NewError(contract.CodeDependencyUnavailable, "load companion payment failed")
		}
		if lookupErr == nil {
			paymentData, encodeErr := configuredFieldData(paymentDT, map[string]any{
				"status": "Failed", "provider_reference": operation.Get("provider_reference"),
			})
			if encodeErr != nil {
				return contract.NewError(contract.CodeInternal, "encoding failed companion payment")
			}
			bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{
				Key: "payment", Operation: "update", Doctype: paymentDT.Name, Name: paymentName, Data: paymentData,
			})
		}
	}
	eventDT := h.siteRegistry(c).Get("External Operation Event")
	if eventDT != nil {
		eventKey := fmt.Sprintf("external-operation-failed:%x", sha256.Sum256([]byte(siteName(c)+"\x00"+operation.Name)))
		eventData, encodeErr := externalOperationEventData(eventDT, operation, "Outbound", "Initiate", "Initiating", "Failed", requestPayload, nil, "Failed", failureMessage, eventKey, time.Now().UTC())
		if encodeErr != nil {
			return contract.NewError(contract.CodeInternal, "encoding operation failure event")
		}
		bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{Key: "event", Doctype: eventDT.Name, Data: eventData})
	}
	if _, _, commandErr := h.runKernelMutationBundle(c, bundle, "external-operation-failed:"+operation.Name); commandErr != nil {
		return commandErr
	}
	return nil
}

// commitExternalOperationSuccess persists the provider result, companion
// payment status, and durable event as one kernel-owned transaction. Provider
// I/O has already completed before this bundle is invoked.
func (h *Handler) commitExternalOperationSuccess(c *gin.Context, operationDT *doctype.DocType, operation *doctype.Document, requestPayload any, previousStatus string) *contract.Error {
	if operationDT == nil || operation == nil || operation.Name == "" {
		return contract.NewError(contract.CodeValidationFailed, "external operation is required")
	}
	operationData, err := transactionDocumentData(operationDT, operation)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding completed external operation failed")
	}
	bundle := kernel.RecordMutationBundlePayload{
		Doctype: operationDT.Name,
		Records: []kernel.RecordMutationBundleItem{{
			Key: "operation", Operation: "update", Doctype: operationDT.Name, Name: operation.Name, Data: operationData,
		}},
	}
	if paymentDT := h.siteRegistry(c).Get("Payment"); paymentDT != nil {
		paymentName, lookupErr := linkedPaymentName(h, c, h.queryDB(c), operation.Name)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return contract.NewError(contract.CodeDependencyUnavailable, "load companion payment failed")
		}
		if lookupErr == nil {
			paymentData, encodeErr := configuredFieldData(paymentDT, map[string]any{
				"status": operation.GetString("status"), "provider_reference": operation.Get("provider_reference"),
			})
			if encodeErr != nil {
				return contract.NewError(contract.CodeInternal, "encoding completed companion payment failed")
			}
			bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{
				Key: "payment", Operation: "update", Doctype: paymentDT.Name, Name: paymentName, Data: paymentData,
			})
		}
	}
	if eventDT := h.siteRegistry(c).Get("External Operation Event"); eventDT != nil {
		eventKey := "external-operation-result:" + operation.Name
		eventData, encodeErr := externalOperationEventData(eventDT, operation, "Outbound", "Initiate", previousStatus,
			operation.GetString("status"), requestPayload, operation.Get("response_payload"), "Processed", "", eventKey, time.Now().UTC())
		if encodeErr != nil {
			return contract.NewError(contract.CodeInternal, "encoding completed external operation event failed")
		}
		bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{Key: "event", Doctype: eventDT.Name, Data: eventData})
	}
	if _, _, commandErr := h.runKernelMutationBundle(c, bundle, "external-operation-result:"+operation.Name); commandErr != nil {
		return commandErr
	}
	return nil
}

func externalOperationEventData(dt *doctype.DocType, operation *doctype.Document, direction, eventType, previousStatus, newStatus string, requestPayload, responsePayload any, processingStatus, errorMessage, eventKey string, occurredAt time.Time) (json.RawMessage, error) {
	event := doctype.NewDocument("")
	event.Set("operation", operation.Name)
	event.Set("direction", direction)
	event.Set("event_type", eventType)
	event.Set("provider", operation.Get("provider"))
	event.Set("provider_reference", operation.Get("provider_reference"))
	event.Set("previous_status", previousStatus)
	event.Set("new_status", newStatus)
	event.Set("request_payload", requestPayload)
	event.Set("response_payload", responsePayload)
	event.Set("processing_status", processingStatus)
	event.Set("error_message", errorMessage)
	event.Set("idempotency_key", eventKey)
	if !occurredAt.IsZero() {
		event.Set("received_at", occurredAt)
		event.Set("processed_at", occurredAt)
	}
	return transactionDocumentData(dt, event)
}

func (h *Handler) saveExternalOperation(c *gin.Context, dt *doctype.DocType, doc *doctype.Document, stage string) *contract.Error {
	data, err := transactionDocumentData(dt, doc)
	if err != nil {
		return contract.NewError(contract.CodeInternal, "encoding external operation update failed")
	}
	idempotencyKey := ""
	if stage != "" {
		idempotencyKey = "external-operation-state:" + doc.Name + ":" + stage
	}
	_, cerr := h.runKernelResourceMutationWithKey(c, kernel.CommandRecordUpdate, dt.Name, doc.Name, data, idempotencyKey)
	return cerr
}

func configuredFieldData(dt *doctype.DocType, values map[string]any) (json.RawMessage, error) {
	fields := make(map[string]any, len(values))
	for name, value := range values {
		if dt.GetField(name) != nil {
			fields[name] = value
		}
	}
	return json.Marshal(fields)
}

func (h *Handler) executeNamedOperationScript(c *gin.Context, scriptName string, doc *doctype.Document) (map[string]any, error) {
	site := siteName(c)
	if h.ScriptRunner == nil || h.runtimeService(c, site).ScriptStore == nil {
		return nil, fmt.Errorf("operation script runner is not available")
	}
	store := h.runtimeService(c, site).ScriptStore
	rec, err := store.LoadByName(site, scriptName)
	if err != nil {
		return nil, fmt.Errorf("load operation script %q: %w", scriptName, err)
	}
	if rec == nil || !rec.IsActive {
		return nil, fmt.Errorf("operation script %q is not active", scriptName)
	}
	timeout := paymentScriptTimeout(rec.TimeoutMs)
	execCtx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	tx := h.siteTx(c)
	user, roles, verified := scriptIdentity(tx.Execution, site)
	if !verified {
		return nil, contract.NewError(contract.CodeUnauthenticated, "verified payment-script execution identity required")
	}
	result, err := h.ScriptRunner.Execute(execCtx, script.ExecuteRequest{
		Script: rec.Script, ScriptType: script.TypeAPIMethod, ScriptName: rec.Name,
		DocType: "External Operation", Event: script.EventPayment, Document: doc.ToMap(),
		User: user, UserRoles: roles, Site: site,
		Timeout: timeout, Provider: tx.ScriptProvider,
	})
	if err != nil {
		_ = store.LogExecution(site, *rec, "External Operation", doc.Name, script.EventPayment, user, int(resultDuration(result).Milliseconds()), "error", err.Error())
		return nil, err
	}
	value, ok := result.Result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("operation script %q must return { result: { success, status } }", scriptName)
	}
	if success, exists := value["success"]; exists {
		if accepted, ok := success.(bool); ok && !accepted {
			return nil, fmt.Errorf("operation rejected: %s", getString(value, "message"))
		}
	}
	_ = store.LogExecution(site, *rec, "External Operation", doc.Name, script.EventPayment, user, int(resultDuration(result).Milliseconds()), "success", "")
	return value, nil
}

func applyOperationScriptResult(doc *doctype.Document, result map[string]any) {
	for key, value := range result {
		switch key {
		case "status":
			status := fmt.Sprint(value)
			if status == "Paid" {
				status = "Succeeded"
			}
			doc.Set("status", status)
		case "provider_reference", "response_payload", "error_message":
			doc.Set(key, value)
		}
	}
}

func resultDuration(result *script.ExecuteResult) time.Duration {
	if result == nil {
		return 0
	}
	return result.Duration
}

// executeTransactionPaymentScript runs a named provider adapter before a
// transaction is inserted. The adapter must return {success: true, ...}; a
// thrown error or success:false prevents the Sale from being created.
func (h *Handler) executeTransactionPaymentScript(c *gin.Context, scriptName string, dt *doctype.DocType, doc *doctype.Document) error {
	site := siteName(c)
	if h.ScriptRunner == nil {
		return fmt.Errorf("payment script runner is not available")
	}
	if h.runtimeService(c, site).ScriptStore == nil {
		return fmt.Errorf("payment script store is not available")
	}
	store := h.runtimeService(c, site).ScriptStore
	rec, err := store.LoadByName(site, scriptName)
	if err != nil {
		return fmt.Errorf("load payment script %q: %w", scriptName, err)
	}
	if rec == nil || !rec.IsActive {
		return fmt.Errorf("payment script %q is not active", scriptName)
	}

	tm := h.siteTx(c)
	user, roles, verified := scriptIdentity(tm.Execution, site)
	if !verified {
		return contract.NewError(contract.CodeUnauthenticated, "verified payment-script execution identity required")
	}
	timeout := paymentScriptTimeout(rec.TimeoutMs)
	execCtx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	result, execErr := h.ScriptRunner.Execute(execCtx, script.ExecuteRequest{
		Script: rec.Script, ScriptType: script.TypeAPIMethod, ScriptName: rec.Name,
		DocType: dt.Name, Event: script.EventPayment, Document: doc.ToMap(),
		User: user, UserRoles: roles, Site: site,
		Timeout: timeout, Provider: tm.ScriptProvider,
	})
	durationMs := 0
	if result != nil {
		durationMs = int(result.Duration.Milliseconds())
	}
	if execErr != nil {
		_ = store.LogExecution(site, *rec, dt.Name, "", script.EventPayment, user, durationMs, "error", execErr.Error())
		return fmt.Errorf("payment script %q failed: %w", scriptName, execErr)
	}

	paymentResult, ok := result.Result.(map[string]any)
	if !ok {
		err := fmt.Errorf("payment script %q must return { success: true, ... }", scriptName)
		_ = store.LogExecution(site, *rec, dt.Name, "", script.EventPayment, user, durationMs, "error", err.Error())
		return err
	}
	success, ok := paymentResult["success"].(bool)
	if !ok || !success {
		message := getString(paymentResult, "message")
		if message == "" {
			message = "payment provider rejected the transaction"
		}
		err := fmt.Errorf("payment declined: %s", message)
		_ = store.LogExecution(site, *rec, dt.Name, "", script.EventPayment, user, durationMs, "error", err.Error())
		return err
	}

	// Copy only known writable fields, allowing provider IDs/status/response to
	// be persisted without allowing a script to overwrite system-owned fields.
	for fieldName, value := range paymentResult {
		field := dt.GetField(fieldName)
		if field != nil && !field.ReadOnly && fieldName != "name" && fieldName != "doc_status" {
			doc.Set(fieldName, value)
		}
	}
	_ = store.LogExecution(site, *rec, dt.Name, "", script.EventPayment, user, durationMs, "success", "")
	return nil
}

func paymentScriptTimeout(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return 5 * time.Second
	}
	return time.Duration(timeoutMs) * time.Millisecond
}

func resolveTransactionChildTable(reg *doctype.Registry, parentDT *doctype.DocType, action *doctype.ViewAction) (string, *doctype.DocType, error) {
	configured := getString(action.Config, "child_table")
	parentField := getString(action.Config, "parent_field")

	for _, field := range parentDT.TableFields() {
		if parentField != "" && field.Fieldname != parentField {
			continue
		}
		if configured == "" || configured == field.Fieldname || configured == field.Options {
			childDT := reg.Get(field.Options)
			if childDT == nil {
				return "", nil, fmt.Errorf("child doctype %q not found", field.Options)
			}
			return field.Fieldname, childDT, nil
		}
	}

	if configured == "" {
		return "", nil, nil
	}
	return "", nil, fmt.Errorf("child_table %q is not a table field or child doctype on %s", configured, parentDT.Name)
}

func buildTransactionChildren(rawItems any, childDT *doctype.DocType, actionConfig map[string]any) ([]*doctype.Document, error) {
	items, ok := rawItems.([]any)
	if !ok {
		return nil, fmt.Errorf("cart/items must be an array")
	}

	children := make([]*doctype.Document, 0, len(items))
	lineFields, _ := actionConfig["line_fields"].(map[string]any)
	lineDefaults, _ := actionConfig["line_defaults"].(map[string]any)
	for i, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d must be an object", i+1)
		}

		child := doctype.NewDocument(childDT.Name)
		for _, field := range childDT.NonTableDataFields() {
			if field.ReadOnly {
				continue
			}
			sourceField := field.Fieldname
			if configuredSource := getString(lineFields, field.Fieldname); configuredSource != "" {
				sourceField = configuredSource
			}
			if val, ok := row[sourceField]; ok {
				child.Set(field.Fieldname, val)
				continue
			}
			if defaultValue, ok := lineDefaults[field.Fieldname]; ok {
				child.Set(field.Fieldname, defaultValue)
			}
		}
		children = append(children, child)
	}
	return children, nil
}

func applyTransactionTotals(parentDT *doctype.DocType, doc *doctype.Document, children []*doctype.Document, actionConfig map[string]any) doctype.ValidationErrors {
	var errors doctype.ValidationErrors
	if parentDT == nil || doc == nil || len(children) == 0 {
		return errors
	}
	totalsConfig, ok := actionConfig["totals"].(map[string]any)
	if !ok || len(totalsConfig) == 0 {
		return errors
	}
	linesConfig, _ := totalsConfig["lines"].(map[string]any)
	quantityField := getString(linesConfig, "quantity_field")
	unitPriceField := getString(linesConfig, "unit_price_field")
	discountField := getString(linesConfig, "discount_field")
	taxField := getString(linesConfig, "tax_field")
	lineTotalField := getString(linesConfig, "line_total_field")
	subtotalField := getString(totalsConfig, "subtotal_field")
	discountTotalField := getString(totalsConfig, "discount_total_field")
	taxTotalField := getString(totalsConfig, "tax_total_field")
	totalField := getString(totalsConfig, "total_field")
	if quantityField == "" || unitPriceField == "" || (subtotalField == "" && totalField == "") {
		return errors
	}
	var subtotal float64
	var taxTotal float64
	var discountTotal float64
	for index, child := range children {
		if child == nil {
			continue
		}
		quantity := numberOrDefault(child.Get(quantityField), 1)
		unitPrice := numberOrDefault(child.Get(unitPriceField), 0)
		discount := numberOrDefault(child.Get(discountField), 0)
		tax := numberOrDefault(child.Get(taxField), 0)
		lineTotal := quantity*unitPrice - discount + tax
		if math.IsNaN(lineTotal) || math.IsInf(lineTotal, 0) {
			field := lineTotalField
			if field == "" {
				field = unitPriceField
			}
			errors = append(errors, &doctype.ValidationError{Type: "ValidationError", Message: fmt.Sprintf("Item %d amount must be a finite number.", index+1), Field: field, DocType: child.DocType})
			continue
		}
		if lineTotalField != "" {
			child.Set(lineTotalField, lineTotal)
		}
		subtotal += quantity * unitPrice
		discountTotal += discount
		taxTotal += tax
	}
	total := subtotal - discountTotal + taxTotal
	if math.IsNaN(total) || math.IsInf(total, 0) {
		field := totalField
		if field == "" {
			field = subtotalField
		}
		errors = append(errors, &doctype.ValidationError{Type: "ValidationError", Message: "Transaction total must be a finite number.", Field: field, DocType: parentDT.Name})
		return errors
	}
	if subtotalField != "" && parentDT.GetField(subtotalField) != nil {
		doc.Set(subtotalField, subtotal)
	}
	if discountTotalField != "" && parentDT.GetField(discountTotalField) != nil {
		doc.Set(discountTotalField, discountTotal)
	}
	if taxTotalField != "" && parentDT.GetField(taxTotalField) != nil {
		doc.Set(taxTotalField, taxTotal)
	}
	if totalField != "" && parentDT.GetField(totalField) != nil {
		doc.Set(totalField, total)
	}
	return errors
}

func numberOrDefault(value any, fallback float64) float64 {
	switch v := value.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float32:
		return float64(v)
	case float64:
		return v
	case json.Number:
		if n, err := v.Float64(); err == nil {
			return n
		}
	case string:
		if v == "" {
			return fallback
		}
		var parsed float64
		if _, err := fmt.Sscanf(v, "%f", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func findViewComponentByID(components []doctype.ViewComponent, id string) *doctype.ViewComponent {
	for i := range components {
		if components[i].ID == id {
			return &components[i]
		}
		if child := findViewComponentByID(components[i].Components, id); child != nil {
			return child
		}
	}
	return nil
}

// HandleViewData returns aggregated data for dashboard/metric components.
// GET /api/v1/view/data?view=Name&component=id
func (h *Handler) HandleViewData(c *gin.Context) {
	viewName := c.Query("view")
	componentID := c.Query("component")

	if viewName == "" || componentID == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "view and component are required", map[string]any{"fields": []string{"view", "component"}})
		return
	}

	reg := h.siteRegistry(c)
	view := reg.Views.GetByName(viewName)
	if view == nil {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found", map[string]any{"name": viewName})
		return
	}

	// Find the component to get its source doctype. Components can be nested
	// inside containers such as dashboard_grid, tabs, or split_view.
	comp := findViewComponentByID(view.Components, componentID)
	if comp == nil {
		writeError(c, http.StatusNotFound, "component.not_found", "Component not found", map[string]any{"id": componentID})
		return
	}

	// For metric_card components, return count/aggregate.
	doctypeName := comp.SourceDocType
	if doctypeName == "" {
		writeError(c, http.StatusBadRequest, "validation.failed", "Component has no source_doctype", nil)
		return
	}

	dt := reg.Get(doctypeName)
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "Doctype not found", map[string]any{"name": doctypeName})
		return
	}
	rowScope, forbidden := checkPerm(c, reg, dt.Name, "read")
	if forbidden {
		return
	}

	// Get count via ORM.
	statusFilter := ""
	if comp.Bindings != nil {
		if s, ok := comp.Bindings["status"]; ok {
			statusFilter = s
		}
	}

	_, total, err := h.siteTx(c).GetListWithScope(dt, statusFilter, "", 1, 0, rowScope, nil, "")
	if err != nil {
		c.JSON(http.StatusOK, Response{Data: map[string]any{
			"count": 0,
			"label": comp.Label,
		}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"count": total,
		"label": comp.Label,
	}})
}

// HandlePublicCreate handles unauthenticated document creation.
// POST /v?route=/apply
func (h *Handler) HandlePublicCreate(c *gin.Context) {
	route := c.Query("route")
	if route == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "route query parameter is required", map[string]any{"field": "route"})
		return
	}

	reg := h.siteRegistry(c)
	view := reg.Views.GetByRoute(route)
	if view == nil || view.PublicAccess == nil || !view.PublicAccess.Enabled || !view.PublicAccess.AllowMutations {
		writeError(c, http.StatusNotFound, "view.not_found", "View not found or public mutations not allowed", nil)
		return
	}

	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid JSON", map[string]any{"error": err.Error()})
		return
	}

	// Use the view's source doctype.
	if view.SourceDocType == "" {
		writeError(c, http.StatusBadRequest, "validation.failed", "View has no source doctype", nil)
		return
	}

	dt := reg.Get(view.SourceDocType)
	if dt == nil || dt.PublicAccess == nil || !dt.PublicAccess.Enabled {
		writeError(c, http.StatusBadRequest, "permission.denied", "Doctype not public", nil)
		return
	}

	publicFields := dt.PublicFieldSet()
	data, err := json.Marshal(body)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid form data", nil)
		return
	}
	payload, err := json.Marshal(map[string]any{
		"doctype": dt.Name, "public_route": route, "data": json.RawMessage(data),
	})
	if err != nil {
		internalError(c, "encoding public form submission", err)
		return
	}
	commandResult, cerr := h.executeKernelOperation(c, kernel.CommandPublicFormSubmit, kernelRequest{Payload: payload})
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(commandResult.Data, &operation); err != nil {
		internalError(c, "decoding public form result", err)
		return
	}

	// Only return public fields.
	result := make(map[string]any)
	for field := range publicFields {
		result[field] = operation.Document[field]
	}
	result["name"] = operation.Name

	c.JSON(http.StatusOK, Response{Data: result})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// viewStore returns a config store for the current site, or nil if unavailable.
func (h *Handler) viewStore(c *gin.Context) *configstore.Store {
	db, _ := c.Get("site_db")
	if db == nil {
		return nil
	}
	sqlDB, ok := db.(*sql.DB)
	if !ok {
		return nil
	}
	return configstore.NewStore(sqlDB, h.siteDialect(c))
}

// currentUser returns an attribution identity derived from the verified
// execution. The legacy Gin user field is presentation/compatibility data.
func currentUser(c *gin.Context) string {
	if user, ok := requestUserID(c); ok {
		return user
	}
	return "system"
}

// getString safely gets a string value from a map.
func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// documentToMap converts a Document to a plain map for JSON responses.
func documentToMap(doc *doctype.Document, dt *doctype.DocType) map[string]any {
	result := make(map[string]any)
	result["name"] = doc.Name
	result["owner"] = doc.Get("owner")
	result["creation"] = doc.Get("creation")
	result["modified"] = doc.Get("modified")
	result["modified_by"] = doc.Get("modified_by")
	result["doc_status"] = doc.DocStatus
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" && !f.Hidden && f.Fieldtype != "Password" {
			result[f.Fieldname] = doc.Get(f.Fieldname)
		}
	}
	return result
}

func transactionDocumentData(dt *doctype.DocType, doc *doctype.Document) (json.RawMessage, error) {
	if dt == nil || doc == nil {
		return nil, fmt.Errorf("transaction document is required")
	}
	serialized := doc.ToMap()
	fields := make(map[string]any, len(dt.DataFields()))
	for _, field := range dt.DataFields() {
		if value, exists := serialized[field.Fieldname]; exists {
			fields[field.Fieldname] = value
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode transaction document: %w", err)
	}
	return encoded, nil
}
