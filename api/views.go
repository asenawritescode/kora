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
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/script"
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

	if err := store.SaveView(&view, site); err != nil {
		internalError(c, "saving view", err)
		return
	}

	// Create a config version for this change.
	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}

	versionID, versionNum, err := store.CreateConfigVersion(site, currentUser(c), "Created view "+view.Name, "Draft", snapshot)
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

	if err := store.SaveView(&view, site); err != nil {
		internalError(c, "saving view", err)
		return
	}

	// Create a config version.
	reg := h.siteRegistry(c)
	snapshot, err := store.CollectSnapshot(reg, site)
	if err != nil {
		internalError(c, "collecting snapshot", err)
		return
	}

	versionID, versionNum, err := store.CreateConfigVersion(site, currentUser(c), "Updated view "+view.Name, "Draft", snapshot)
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
	for i := range view.Components {
		if view.Components[i].ID == req.Component {
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

	// Execute the action based on resolved type (from stored config, not client).
	switch targetAction.Type {
	case "create_record":
		h.executeCreateRecord(c, targetAction, req.Context)
	case "update_record":
		h.executeUpdateRecord(c, targetAction, req.Context)
	case "workflow_transition":
		h.executeWorkflowTransition(c, targetAction, req.Context)
	case "create_transaction":
		h.executeCreateTransaction(c, targetAction, req.Context)
	case "initiate_external_operation":
		h.executeInitiateExternalOperation(c, targetAction, req.Context)
	case "validate_external_operation":
		h.executeValidateExternalOperation(c, targetAction, req.Context)
	default:
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
	for k, v := range ctx {
		if !strings.HasPrefix(k, "_") && dt.GetField(k) != nil {
			data[k] = v
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

func (h *Handler) executeWorkflowTransition(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
	transition := getString(action.Config, "transition")
	doctypeName := getString(action.Config, "doctype")
	name := getString(ctx, "name")

	if doctypeName == "" || name == "" || transition == "" {
		writeError(c, http.StatusBadRequest, "validation.required_field", "doctype, name, and transition are required", map[string]any{"fields": []string{"doctype", "name", "transition"}})
		return
	}

	reg := h.siteRegistry(c)
	wf := reg.Workflows.Get(doctypeName)
	if wf == nil {
		writeError(c, http.StatusBadRequest, "workflow.not_found", "No workflow for doctype", map[string]any{"doctype": doctypeName})
		return
	}

	dt := reg.Get(doctypeName)
	if dt == nil {
		writeError(c, http.StatusBadRequest, "resource.doctype_not_found", "Doctype not found", map[string]any{"name": doctypeName})
		return
	}

	tm := h.siteTx(c)
	doc, err := tm.GetDoc(dt, name, "")
	if err != nil {
		writeError(c, http.StatusNotFound, "resource.document_not_found", "Document not found", map[string]any{"name": name})
		return
	}

	transitioned, cerr := h.runKernelWorkflowTransition(c, doctypeName, name, transition, doc.Revision)
	if cerr != nil {
		status := http.StatusBadRequest
		code := "workflow.transition_failed"
		switch cerr.Type {
		case contract.CodePermissionDenied:
			status, code = http.StatusForbidden, "permission.denied"
		case contract.CodeNotFound:
			status, code = http.StatusNotFound, "resource.document_not_found"
		case contract.CodeConflict, contract.CodeIdempotencyKeyReused:
			status, code = http.StatusConflict, "resource.conflict"
		case contract.CodeInternal, contract.CodeDependencyUnavailable:
			status, code = http.StatusInternalServerError, "internal.error"
		}
		writeError(c, status, code, cerr.Message, nil)
		return
	}

	c.JSON(http.StatusOK, Response{Data: documentToMap(transitioned, dt)})
}

func (h *Handler) executeCreateTransaction(c *gin.Context, action *doctype.ViewAction, ctx map[string]any) {
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

	parentField, childDT, err := resolveTransactionChildTable(reg, dt, action)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
		return
	}

	if childDT != nil {
		rawItems := ctx["cart"]
		if rawItems == nil {
			rawItems = ctx["items"]
		}
		children, err := buildTransactionChildren(rawItems, childDT, action.Config)
		if err != nil {
			writeError(c, http.StatusBadRequest, "validation.failed", err.Error(), nil)
			return
		}
		if len(children) == 0 {
			writeError(c, http.StatusBadRequest, "validation.required_field", "transaction requires at least one item", map[string]any{"field": "items"})
			return
		}
		doc.SetTable(parentField, children)
		applyTransactionTotals(dt, doc, children, action.Config)
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

	// A provider must approve payment before the Sale exists. The script name
	// comes from stored view configuration, never from client input.
	if scriptName := getString(action.Config, "payment_script"); scriptName != "" {
		if err := h.executeTransactionPaymentScript(c, scriptName, dt, doc); err != nil {
			writeError(c, http.StatusBadRequest, "payment.failed", err.Error(), nil)
			return
		}
	}

	operationName := getString(ctx, "external_operation")
	paymentDT := reg.Get("Payment")
	saleData, dataErr := transactionDocumentData(dt, doc)
	if dataErr != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid transaction data", nil)
		return
	}
	bundle := kernel.RecordMutationBundlePayload{
		Doctype: dt.Name,
		Records: []kernel.RecordMutationBundleItem{{Key: "sale", Doctype: dt.Name, Data: saleData}},
	}
	if operationName == "" && paymentDT != nil {
		paymentData, dataErr := json.Marshal(map[string]any{
			"reference": "PAY-$records.sale.name", "sale": "$records.sale.name",
			"amount": ctx["total"], "method": ctx["payment_method"],
			"status": "Succeeded", "assignee": user,
		})
		if dataErr != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid payment data", nil)
			return
		}
		bundle.Records = append(bundle.Records, kernel.RecordMutationBundleItem{Key: "payment", Doctype: paymentDT.Name, Data: paymentData})
	} else if operationName != "" && paymentDT != nil {
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
	idempotencyKey := "pos-sale:" + doc.GetString(getString(action.Config, "reference_field"))
	result, _, cerr := h.runKernelMutationBundle(c, bundle, idempotencyKey)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	doc = orm.DocumentFromMap(reg, dt.Name, result.Document)

	c.JSON(http.StatusOK, Response{Data: documentToMap(doc, dt)})
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
		doc.Set("currency", "KES")
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
		doc.Set("idempotency_key", fmt.Sprintf("%s-%d", c.GetString("user"), time.Now().UnixNano()))
	}
	doc.Set("request_payload", ctx)
	doc.Set("initiated_by", c.GetString("user"))

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
		h.recordExternalOperationEvent(c, doc, "Outbound", "Status Check", previousStatus, doc.GetString("status"), ctx, doc.Get("response_payload"), "Processed", "")
		if scriptErr != nil {
			writeError(c, http.StatusBadRequest, "operation.failed", scriptErr.Error(), nil)
			return
		}
	}
	if getString(action.Config, "script") == "" {
		h.recordExternalOperationEvent(c, doc, "Internal", "Status Check", doc.GetString("status"), doc.GetString("status"), ctx, nil, "Processed", "")
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
	if h.ScriptRunner == nil || (h.SiteScriptStores == nil && h.RuntimeServices == nil) || h.runtimeService(c, site).ScriptStore == nil {
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
	result, err := h.ScriptRunner.Execute(execCtx, script.ExecuteRequest{
		Script: rec.Script, ScriptType: script.TypeAPIMethod, ScriptName: rec.Name,
		DocType: "External Operation", Event: script.EventPayment, Document: doc.ToMap(),
		User: c.GetString("user"), UserRoles: []string{c.GetString("user_role")}, Site: site,
		Timeout: timeout, Provider: h.siteTx(c).ScriptProvider,
	})
	if err != nil {
		_ = store.LogExecution(site, *rec, "External Operation", doc.Name, script.EventPayment, c.GetString("user"), int(resultDuration(result).Milliseconds()), "error", err.Error())
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
	_ = store.LogExecution(site, *rec, "External Operation", doc.Name, script.EventPayment, c.GetString("user"), int(resultDuration(result).Milliseconds()), "success", "")
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
	if (h.SiteScriptStores == nil && h.RuntimeServices == nil) || h.runtimeService(c, site).ScriptStore == nil {
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

	user := c.GetString("user")
	userRole := c.GetString("user_role")
	tm := h.siteTx(c)
	timeout := paymentScriptTimeout(rec.TimeoutMs)
	execCtx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	result, execErr := h.ScriptRunner.Execute(execCtx, script.ExecuteRequest{
		Script: rec.Script, ScriptType: script.TypeAPIMethod, ScriptName: rec.Name,
		DocType: dt.Name, Event: script.EventPayment, Document: doc.ToMap(),
		User: user, UserRoles: []string{userRole}, Site: site,
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

		child := doctype.NewDocument("")
		for _, field := range childDT.NonTableDataFields() {
			if field.ReadOnly && field.Computed != "" {
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

func applyTransactionTotals(parentDT *doctype.DocType, doc *doctype.Document, children []*doctype.Document, actionConfig map[string]any) {
	if parentDT == nil || doc == nil || len(children) == 0 {
		return
	}
	totalsConfig, ok := actionConfig["totals"].(map[string]any)
	if !ok || len(totalsConfig) == 0 {
		return
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
	if quantityField == "" || unitPriceField == "" || subtotalField == "" || totalField == "" {
		return
	}
	var subtotal float64
	var taxTotal float64
	var discountTotal float64
	for _, child := range children {
		if child == nil {
			continue
		}
		quantity := numberOrDefault(child.Get(quantityField), 1)
		unitPrice := numberOrDefault(child.Get(unitPriceField), 0)
		discount := numberOrDefault(child.Get(discountField), 0)
		tax := numberOrDefault(child.Get(taxField), 0)
		lineTotal := quantity*unitPrice - discount + tax
		if lineTotalField != "" {
			lineTotal = numberOrDefault(child.Get(lineTotalField), lineTotal)
			child.Set(lineTotalField, lineTotal)
		}
		subtotal += quantity * unitPrice
		discountTotal += discount
		taxTotal += tax
	}
	total := subtotal - discountTotal + taxTotal
	if parentDT.GetField(subtotalField) != nil && isEmptyNumber(doc.Get(subtotalField)) {
		doc.Set(subtotalField, subtotal)
	}
	if discountTotalField != "" && parentDT.GetField(discountTotalField) != nil && isEmptyNumber(doc.Get(discountTotalField)) {
		doc.Set(discountTotalField, discountTotal)
	}
	if taxTotalField != "" && parentDT.GetField(taxTotalField) != nil && isEmptyNumber(doc.Get(taxTotalField)) {
		doc.Set(taxTotalField, taxTotal)
	}
	if parentDT.GetField(totalField) != nil && isEmptyNumber(doc.Get(totalField)) {
		doc.Set(totalField, total)
	}
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

func isEmptyNumber(value any) bool {
	return numberOrDefault(value, 0) == 0
}

func firstPresent(row map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if val, ok := row[key]; ok && val != nil {
			return val, true
		}
	}
	return nil, false
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

	// Get count via ORM.
	statusFilter := ""
	if comp.Bindings != nil {
		if s, ok := comp.Bindings["status"]; ok {
			statusFilter = s
		}
	}

	_, total, err := h.siteTx(c).GetList(dt, statusFilter, "", 1, 0, "")
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

// currentUser returns the authenticated user identifier from context.
func currentUser(c *gin.Context) string {
	if user, ok := c.Get("user"); ok {
		if s, ok := user.(string); ok && s != "" {
			return s
		}
	}
	return "system"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
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
		if f.Fieldtype != "Table" {
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

// handleViewError maps ORM/database errors to API error responses.
func handleViewError(c *gin.Context, dt *doctype.DocType, err error) {
	if ve, ok := err.(*doctype.ValidationError); ok {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{
			"type":    "ValidationError",
			"message": ve.Error(),
			"field":   ve.Field,
		}})
		return
	}
	if strings.Contains(err.Error(), "ValidationError") {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{
			"type":    "ValidationError",
			"message": err.Error(),
		}})
		return
	}
	c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{
		"message": err.Error(),
	}})
}
