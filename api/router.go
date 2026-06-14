package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/yourorg/kora/doctype"
	"github.com/yourorg/kora/orm"
)

// Handler holds dependencies for API handlers.
// Registry and TxManager are fallbacks; handlers read site context from the request.
type Handler struct {
	Registry  *doctype.Registry
	TxManager *orm.TxManager
}

// NewHandler creates a new API handler.
func NewHandler(registry *doctype.Registry, txManager *orm.TxManager) *Handler {
	return &Handler{
		Registry:  registry,
		TxManager: txManager,
	}
}

// siteRegistry returns the registry for the current request's site.
// Falls back to h.Registry if no site context is set (single-site or boot).
func (h *Handler) siteRegistry(c *gin.Context) *doctype.Registry {
	if reg, ok := c.Get("site_registry"); ok {
		if r, ok := reg.(*doctype.Registry); ok && r != nil {
			return r
		}
	}
	return h.Registry
}

// siteTx returns a TxManager for the current request's site database and registry.
func (h *Handler) siteTx(c *gin.Context) *orm.TxManager {
	db, _ := c.Get("site_db")
	reg, _ := c.Get("site_registry")
	if db != nil && reg != nil {
		if sqlDB, ok := db.(*sql.DB); ok {
			if r, ok := reg.(*doctype.Registry); ok {
				return &orm.TxManager{DB: sqlDB, Registry: r}
			}
		}
	}
	return h.TxManager
}

// APIDefaultLimit and APIMaxLimit control pagination (set from common config at startup).
var APIDefaultLimit = 50
var APIMaxLimit = 500

// SetAPILimits sets pagination limits from config.
func SetAPILimits(def, max int) {
	if def > 0 { APIDefaultLimit = def }
	if max > 0 { APIMaxLimit = max }
}

// internalError logs the real error server-side and returns a generic 500 response.
// This prevents sensitive DB/internal details from leaking to API clients.
func internalError(c *gin.Context, msg string, err error) {
	slog.Error(msg, "error", err)
	c.JSON(http.StatusInternalServerError, ErrorResponse{
		Error: map[string]string{"message": "An internal error occurred"},
	})
}

// Meta holds response metadata.
type Meta struct {
	ConfigVersion int    `json:"config_version,omitempty"`
	DocType       string `json:"doctype,omitempty"`
	Total         int    `json:"total,omitempty"`
}

// Response is the standard API response envelope.
type Response struct {
	Data any    `json:"data,omitempty"`
	Meta *Meta  `json:"meta,omitempty"`
}

// ErrorResponse is the standard error response envelope.
type ErrorResponse struct {
	Error any   `json:"error"`
	Meta  *Meta `json:"meta,omitempty"`
}

// --- List Handler ---

// checkPerm is a helper that checks permission for the current user and returns
// whether the operation is owner-scoped. Returns true if forbidden (and writes response).
func checkPerm(c *gin.Context, registry *doctype.Registry, doctype, operation string) (ownerOnly bool, forbidden bool) {
	userRoles := c.GetStringSlice("user_roles")
	if len(userRoles) == 0 {
		// Fallback: if no roles set, allow (bootstrapping / system user).
		return false, false
	}
	allowed, ownerScoped := registry.CanUser(userRoles, doctype, operation)
	if !allowed {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: map[string]string{
				"message": fmt.Sprintf("Permission denied: cannot %s on %s", operation, doctype),
			},
		})
		return false, true
	}
	return ownerScoped, false
}

// HandleList handles GET /api/resource/{doctype}
func (h *Handler) HandleList(c *gin.Context) {
	doctypeName := c.Param("doctype")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check read permission.
	ownerOnly, forbidden := checkPerm(c, h.Registry, doctypeName, "read")
	if forbidden {
		return
	}
	owner := ""
	if ownerOnly {
		owner = c.GetString("user")
	}

	// Parse query parameters.
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", strconv.Itoa(APIDefaultLimit)))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	orderBy := c.Query("order_by")
	filters := c.Query("filters")

	if limit < 1 {
		limit = APIDefaultLimit
	}
	if limit > APIMaxLimit {
		limit = APIMaxLimit
	}

	// Parse fields filter.
	fieldsParam := c.Query("fields")
	var requestedFields []string
	if fieldsParam != "" {
		if err := json.Unmarshal([]byte(fieldsParam), &requestedFields); err != nil {
			slog.Warn("invalid fields parameter", "error", err)
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": "Invalid fields parameter"},
			})
			return
		}
	}

	docs, total, err := h.siteTx(c).GetList(dt, filters, orderBy, limit, offset, owner)
	if err != nil {
		internalError(c, "list query failed", err)
		return
	}

	// Filter fields if requested.
	var result []map[string]any
	for _, doc := range docs {
		item := docToMap(doc, dt, h.siteRegistry(c),requestedFields)
		result = append(result, item)
	}

	c.JSON(http.StatusOK, Response{
		Data: result,
		Meta: &Meta{
			DocType: doctypeName,
			Total:   total,
		},
	})
}

// --- Get Handler ---

// HandleGet handles GET /api/resource/{doctype}/{name}
func (h *Handler) HandleGet(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check read permission.
	ownerOnly, forbidden := checkPerm(c, h.Registry, doctypeName, "read")
	if forbidden {
		return
	}
	owner := ""
	if ownerOnly {
		owner = c.GetString("user")
	}

	doc, err := h.siteTx(c).GetDoc(dt, name, owner)
	if err != nil {
		slog.Warn("document get failed", "doctype", doctypeName, "name", name, "error", err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Document not found"},
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c),nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

// --- Create Handler ---

// HandleCreate handles POST /api/resource/{doctype}
func (h *Handler) HandleCreate(c *gin.Context) {
	doctypeName := c.Param("doctype")
	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	if _, forbidden := checkPerm(c, h.Registry, doctypeName, "create"); forbidden {
		return
	}

	// Parse request body.
	var rawData map[string]any
	if err := c.ShouldBindJSON(&rawData); err != nil {
		slog.Warn("invalid JSON in create", "error", err)
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format"},
		})
		return
	}

	// Build Document from raw data.
	doc := doctype.NewDocument(doctypeName)
	for key, val := range rawData {
		field := dt.GetField(key)
		if field != nil && field.Fieldtype == "Table" {
			// Parse child table rows.
			children, err := parseChildRows(val, field, h.siteRegistry(c))
			if err != nil {
				c.JSON(http.StatusBadRequest, ErrorResponse{
					Error: map[string]string{"message": fmt.Sprintf("Field %s: %s", key, err.Error())},
				})
				return
			}
			doc.Set(key, children)
		} else {
			doc.Set(key, val)
		}
	}

	// Set default values for fields not in request.
	for _, f := range dt.DataFields() {
		if f.Default != "" {
			if _, exists := rawData[f.Fieldname]; !exists {
				doc.Set(f.Fieldname, f.Default)
			}
		}
	}

	// Validate.
	validationErrs := doctype.ValidateDocument(dt, doc, h.Registry, nil)
	if validationErrs.HasErrors() {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: formatValidationErrors(validationErrs),
		})
		return
	}

	// Get current user.
	owner := c.GetString("user")
	if owner == "" {
		owner = "system"
	}

	// Insert.
	if err := h.siteTx(c).Insert(dt, doc, owner); err != nil {
		var valErr *doctype.ValidationError
		if errors.As(err, &valErr) {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: formatValidationErrors(doctype.ValidationErrors{valErr}),
			})
			return
		}
		internalError(c, "insert failed", err)
		return
	}

	c.JSON(http.StatusCreated, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c),nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

// --- Update Handler ---

// HandleUpdate handles PUT /api/resource/{doctype}/{name}
func (h *Handler) HandleUpdate(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check write permission.
	ownerOnly, forbidden := checkPerm(c, h.Registry, doctypeName, "write")
	if forbidden {
		return
	}
	owner := ""
	if ownerOnly {
		owner = c.GetString("user")
	}

	// Load existing document.
	oldDoc, err := h.siteTx(c).GetDoc(dt, name, owner)
	if err != nil {
		slog.Warn("document get failed for update", "doctype", doctypeName, "name", name, "error", err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Document not found"},
		})
		return
	}

	// Parse request body.
	var rawData map[string]any
	if err := c.ShouldBindJSON(&rawData); err != nil {
		slog.Warn("invalid JSON in update", "error", err)
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format"},
		})
		return
	}

	// Build updated Document.
	doc := doctype.NewDocument(doctypeName)
	doc.Name = name
	doc.IsNew = false

	// Start with existing values, then overlay request data.
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			doc.Set(f.Fieldname, oldDoc.Get(f.Fieldname))
		} else {
			doc.Set(f.Fieldname, oldDoc.Get(f.Fieldname))
		}
	}

	for key, val := range rawData {
		field := dt.GetField(key)
		if field != nil && field.Fieldtype == "Table" {
			children, err := parseChildRows(val, field, h.siteRegistry(c))
			if err != nil {
				c.JSON(http.StatusBadRequest, ErrorResponse{
					Error: map[string]string{"message": fmt.Sprintf("Field %s: %s", key, err.Error())},
				})
				return
			}
			doc.Set(key, children)
		} else if field != nil && field.ReadOnly {
			// Silently ignore read-only fields.
		} else {
			doc.Set(key, val)
		}
	}

	// Validate.
	validationErrs := doctype.ValidateDocument(dt, doc, h.Registry, oldDoc)
	if validationErrs.HasErrors() {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: formatValidationErrors(validationErrs),
		})
		return
	}

	// Get current user.
	modifiedBy := c.GetString("user")
	if modifiedBy == "" {
		modifiedBy = "system"
	}

	// Save.
	if err := h.siteTx(c).Save(dt, doc, modifiedBy, owner, oldDoc); err != nil {
		var valErr *doctype.ValidationError
		if errors.As(err, &valErr) {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: formatValidationErrors(doctype.ValidationErrors{valErr}),
			})
			return
		}
		internalError(c, "save failed", err)
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c),nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

// --- Delete Handler ---

// HandleDelete handles DELETE /api/resource/{doctype}/{name}
func (h *Handler) HandleDelete(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check delete permission.
	ownerOnly, forbidden := checkPerm(c, h.Registry, doctypeName, "delete")
	if forbidden {
		return
	}
	owner := ""
	if ownerOnly {
		owner = c.GetString("user")
	}

	if err := h.siteTx(c).Delete(dt, name, owner); err != nil {
		slog.Warn("document delete failed", "doctype", doctypeName, "name", name, "error", err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Document not found"},
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "deleted"},
		Meta: &Meta{DocType: doctypeName},
	})
}

// --- Helpers ---

// docToMap converts a Document to a map for JSON serialization, including system fields.
func docToMap(doc *doctype.Document, dt *doctype.DocType, registry *doctype.Registry, requestedFields []string) map[string]any {
	result := make(map[string]any)
	result["name"] = doc.Name
	result["doc_status"] = doc.DocStatus

	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			children := doc.GetTable(f.Fieldname)
			if children != nil {
				childDT := dtRegistryLookup(registry, dt, f.Fieldname)
				var childMaps []map[string]any
				for _, child := range children {
					childMaps = append(childMaps, docToMap(child, childDT, registry, nil))
				}
				result[f.Fieldname] = childMaps
			} else {
				result[f.Fieldname] = []any{}
			}
		} else {
			val := doc.Get(f.Fieldname)
			// Round Float/Currency/Percent to 2 decimal places.
			if f.Fieldtype == "Float" || f.Fieldtype == "Currency" || f.Fieldtype == "Percent" {
				if s, ok := val.(string); ok && s != "" {
					if n, err := strconv.ParseFloat(s, 64); err == nil {
						val = math.Round(n*100) / 100
					}
				}
			}
			result[f.Fieldname] = val
		}
	}

	// Filter to requested fields.
	if len(requestedFields) > 0 {
		filtered := make(map[string]any)
		filtered["name"] = result["name"]
		for _, fieldName := range requestedFields {
			if val, ok := result[fieldName]; ok {
				filtered[fieldName] = val
			}
		}
		return filtered
	}

	return result
}

// dtRegistryLookup looks up a child doctype from the registry for the given parent doctype and field.
// The registry parameter comes from the site context.
func dtRegistryLookup(registry *doctype.Registry, dt *doctype.DocType, fieldName string) *doctype.DocType {
	field := dt.GetField(fieldName)
	if field == nil || field.Options == "" {
		return nil
	}
	return registry.Get(field.Options)
}

func parseChildRows(val any, field *doctype.Field, registry *doctype.Registry) ([]*doctype.Document, error) {
	rows, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("expected array of child rows")
	}

	childDT := registry.Get(field.Options)
	if childDT == nil {
		return nil, fmt.Errorf("child doctype %q not found", field.Options)
	}

	var children []*doctype.Document
	for i, row := range rows {
		rowMap, ok := row.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("row %d: expected object", i)
		}

		child := doctype.NewDocument(field.Options)
		for k, v := range rowMap {
			child.Set(k, v)
		}
		_ = childDT
		children = append(children, child)
	}

	return children, nil
}

func formatValidationErrors(errors doctype.ValidationErrors) any {
	if len(errors) == 1 {
		return map[string]any{
			"type":    errors[0].Type,
			"message": errors[0].Message,
			"field":   errors[0].Field,
			"doctype": errors[0].DocType,
		}
	}

	var messages []map[string]any
	for _, e := range errors {
		messages = append(messages, map[string]any{
			"type":    e.Type,
			"message": e.Message,
			"field":   e.Field,
			"doctype": e.DocType,
		})
	}
	return map[string]any{
		"errors": messages,
	}
}

// RegisterRoutes registers all CRUD routes for all DocTypes in the registry on a full Engine.
func RegisterRoutes(router *gin.Engine, registry *doctype.Registry, txManager *orm.TxManager) {
	handler := NewHandler(registry, txManager)
	RegisterRoutesOnGroup(router.Group("/api"), registry, txManager)

	// Health check outside the API group.
	router.GET("/api/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "pong"})
	})

	// File upload endpoint.
	router.POST("/api/upload", handler.HandleUpload)

	_ = handler
}

// RegisterRoutesOnGroup registers all CRUD routes on an existing RouterGroup.
// This allows the caller to apply middleware (e.g., auth) before the group.
func RegisterRoutesOnGroup(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager) {
	handler := NewHandler(registry, txManager)

	resource := apiGroup.Group("/resource")
	{
		resource.GET("/:doctype", handler.HandleList)
		resource.POST("/:doctype", handler.HandleCreate)
		resource.GET("/:doctype/:name", handler.HandleGet)
		resource.PUT("/:doctype/:name", handler.HandleUpdate)
		resource.DELETE("/:doctype/:name", handler.HandleDelete)
		resource.POST("/:doctype/:name/workflow_action", handler.HandleWorkflowAction)
	}

	// OpenAPI docs.
	apiGroup.GET("/openapi.json", handler.HandleOpenAPI)
	apiGroup.GET("/swagger-ui", handler.HandleSwaggerUI)

	// System config endpoints.
	system := apiGroup.Group("/system/config")
	{
		system.GET("/versions", handler.HandleConfigVersions)
		system.GET("/versions/:id", handler.HandleConfigVersion)
		system.GET("/diff", handler.HandleConfigDiff)
	}

	// System schema/navigation endpoints.
	RegisterSystemRoutes(apiGroup, handler)
}

// HandleWorkflowAction handles POST /api/resource/{doctype}/{name}/workflow_action
func (h *Handler) HandleWorkflowAction(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check workflow exists.
	if !h.siteRegistry(c).Workflows.Has(doctypeName) {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("No workflow defined for %s", doctypeName)},
		})
		return
	}

	// Check submit permission.
	ownerOnly, forbidden := checkPerm(c, h.Registry, doctypeName, "submit")
	if forbidden {
		return
	}
	owner := ""
	if ownerOnly {
		owner = c.GetString("user")
	}

	// Parse request.
	var req struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		slog.Warn("invalid JSON in workflow action", "error", err)
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format"},
		})
		return
	}

	if req.Action == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "action is required"},
		})
		return
	}

	// Load document.
	doc, err := h.siteTx(c).GetDoc(dt, name, owner)
	if err != nil {
		slog.Warn("document get failed for workflow", "doctype", doctypeName, "name", name, "error", err)
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Document not found"},
		})
		return
	}

	// Get current state.
	currentState := doc.GetString(dt.GetField("status").Fieldname)
	if currentState == "" {
		currentState = "Draft"
	}

	// Get user role.
	userRole := c.GetString("user_role")
	if userRole == "" {
		userRole = doctype.AdminRole
	}

	// Check available transitions.
	available := h.siteRegistry(c).Workflows.GetAvailableTransitions(doctypeName, currentState, userRole, doc)
	found := false
	for _, t := range available {
		if t.Action == req.Action {
			found = true
			break
		}
	}
	if !found {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{
				"message": fmt.Sprintf("Transition %q is not available from state %q for role %q", req.Action, currentState, userRole),
			},
		})
		return
	}

	// Apply transition.
	newState, newDocStatus, err := h.siteRegistry(c).Workflows.ApplyTransition(doctypeName, currentState, req.Action, userRole, doc)
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Update document state.
	statusField := dt.GetField("status")
	if statusField == nil {
		// Try workflow_state_field.
		wf := h.siteRegistry(c).Workflows.Get(doctypeName)
		if wf != nil {
			statusField = dt.GetField(wf.WorkflowStateField)
		}
	}
	if statusField != nil {
		doc.Set(statusField.Fieldname, newState)
	}
	doc.DocStatus = newDocStatus

	// Save.
	modifiedBy := c.GetString("user")
	if modifiedBy == "" {
		modifiedBy = "system"
	}
	if err := h.siteTx(c).Save(dt, doc, modifiedBy, owner, nil); err != nil {
		internalError(c, "workflow save failed", err)
		return
	}

	// Dispatch workflow notifications.
	dispatchNotifications(h.Registry, doctypeName, newState, doc)

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c),nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

// dispatchNotifications fires workflow notifications for a state change.
func dispatchNotifications(registry *doctype.Registry, doctypeName, toState string, doc *doctype.Document) {
	wf := registry.Workflows.Get(doctypeName)
	if wf == nil {
		return
	}
	for _, n := range wf.Notifications {
		if n.Event != "state_change" || n.ToState != toState {
			continue
		}
		data := make(map[string]string)
		data["name"] = doc.Name
		dt := registry.Get(doctypeName)
		if dt != nil {
			for _, f := range dt.DataFields() {
				if f.Fieldtype != "Table" {
					data[f.Fieldname] = fmt.Sprintf("%v", doc.Get(f.Fieldname))
				}
			}
		}
		for _, r := range n.Recipients {
			if field, ok := r["field"]; ok {
				recipient := doc.GetString(field)
				if recipient != "" {
					slog.Info("workflow notification", "to", recipient, "subject", n.Subject, "state", toState)
				}
			}
		}
	}
}

// HandleConfigVersions lists all config versions.
func (h *Handler) HandleConfigVersions(c *gin.Context) {
	rows, err := h.siteTx(c).DB.Query(
		"SELECT id, site, version, created_at, created_by, label, is_active FROM _kora_config_version ORDER BY version DESC LIMIT 50",
	)
	if err != nil {
		internalError(c, "config versions query failed", err)
		return
	}
	defer rows.Close()

	var versions []map[string]any
	for rows.Next() {
		var id, site, createdBy, label, createdAt string
		var version int
		var isActive bool
		if err := rows.Scan(&id, &site, &version, &createdAt, &createdBy, &label, &isActive); err != nil {
			continue
		}
		versions = append(versions, map[string]any{
			"id": id, "site": site, "version": version,
			"created_at": createdAt, "created_by": createdBy,
			"label": label, "is_active": isActive,
		})
	}
	c.JSON(http.StatusOK, Response{Data: versions})
}

// HandleConfigVersion gets a single config version snapshot.
func (h *Handler) HandleConfigVersion(c *gin.Context) {
	id := c.Param("id")
	var configJSON, changelog, label string
	var version int
	err := h.siteTx(c).DB.QueryRow(
		"SELECT version, label, config, changelog FROM _kora_config_version WHERE id = ?", id,
	).Scan(&version, &label, &configJSON, &changelog)
	if err != nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Version not found"}})
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"id": id, "version": version, "label": label,
		"config": configJSON, "changelog": changelog,
	}})
}

// HandleConfigDiff returns the diff between two config versions.
func (h *Handler) HandleConfigDiff(c *gin.Context) {
	fromID := c.Query("from")
	toID := c.Query("to")
	if fromID == "" || toID == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "from and to required"}})
		return
	}
	var fromJSON, toJSON string
	h.siteTx(c).DB.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", fromID).Scan(&fromJSON)
	h.siteTx(c).DB.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", toID).Scan(&toJSON)
	if fromJSON == "" || toJSON == "" {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Version not found"}})
		return
	}
	var from, to []*doctype.DocType
	yaml.Unmarshal([]byte(fromJSON), &from)
	yaml.Unmarshal([]byte(toJSON), &to)
	diff := doctype.DiffConfigs(from, to)
	c.JSON(http.StatusOK, Response{Data: diff})
}

// HandleUpload handles file uploads via multipart form.
// POST /api/upload
// Stores files to sites/<site>/files/<YYYY>/<MM>/<filename>.
func (h *Handler) HandleUpload(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "No file provided"},
		})
		return
	}
	defer file.Close()

	// Determine site for directory scoping.
	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}

	// Create directory: sites/<site>/files/<YYYY>/<MM>/
	now := time.Now()
	dir := filepath.Join("sites", siteName, "files",
		fmt.Sprintf("%04d", now.Year()),
		fmt.Sprintf("%02d", now.Month()))
	if err := os.MkdirAll(dir, 0755); err != nil {
		internalError(c, "creating upload directory", err)
		return
	}

	// Sanitize filename and avoid collisions.
	filename := filepath.Base(header.Filename)
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	dest := filepath.Join(dir, filename)
	for i := 1; fileExists(dest); i++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
	}

	out, err := os.Create(dest)
	if err != nil {
		internalError(c, "creating file", err)
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		internalError(c, "writing file", err)
		return
	}

	// Return the relative path for storing in an Attach field.
	relPath := dest
	c.JSON(http.StatusCreated, Response{
		Data: map[string]string{"path": relPath, "filename": filepath.Base(dest)},
	})
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// init registers this package's types for JSON handling.
func init() {
	_ = strings.NewReader
}
