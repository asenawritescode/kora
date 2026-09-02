package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
)

// BinaryVersion is set at startup from cli.Version to avoid circular imports.
// cli/serve.go sets this to its Version value during initialization.
var BinaryVersion = "dev"

// fallbackStorage is used when no site-specific storage backend is configured.
var fallbackStorage storage.Backend

// Handler holds dependencies for API handlers.
// Registry and TxManager are fallbacks; handlers read site context from the request.
type Handler struct {
	Registry      *doctype.Registry
	TxManager     *orm.TxManager
	AuthProviders *auth.ProviderRegistry

	// SiteEventBuses maps site name → EventBus for analytics event emission.
	// When set, siteTx() propagates the EventBus to the TxManager.
	SiteEventBuses map[string]analytics.EventBus

	// SiteOutboxes maps site name → outbox.Writer for transactional outbox recording.
	// When a site has a writer, its document writes record an event in _kora_outbox
	// within the same transaction. Nil for sites (or deployments) without the outbox.
	SiteOutboxes map[string]outbox.Writer

	// SiteRealtimeProviders maps site name → NATS provider used to source realtime events.
	SiteRealtimeProviders map[string]*natsprovider.Provider

	// ScriptRunner executes JavaScript hooks (shared across all sites).
	ScriptRunner script.Runner

	// SiteScriptStores maps site name → *script.Store for script persistence.
	SiteScriptStores map[string]*script.Store

	// SiteSecretStores maps site name → *secret.Store for secret access from scripts.
	SiteSecretStores map[string]*secret.Store

	// ScriptHTTPAllowlist controls which domains scripts can call via kora.http.
	ScriptHTTPAllowlist []string

	// SiteWebhookWorkers maps site name → *webhook.Worker for webhook delivery.
	SiteWebhookWorkers map[string]*webhook.Worker

	// AsyncHookSink receives after_* hooks for fire-and-forget execution.
	AsyncHookSink orm.AsyncHookSink

	// SiteStorages maps site name → storage backend for attachments. Storage is a
	// fallback used when no site-specific backend is configured.
	SiteStorages map[string]storage.Backend
	Storage      storage.Backend

	// KernelCommands carries config-defined command resources (KERNEL-008)
	// loaded at startup from application configuration. Nil = built-ins only;
	// GET /api/v1/kernel/_registry then reports an empty list.
	KernelCommands *kernel.CommandRegistry
}

// NewHandler creates a new API handler.
func NewHandler(registry *doctype.Registry, txManager *orm.TxManager) *Handler {
	return &Handler{
		Registry:      registry,
		TxManager:     txManager,
		AuthProviders: auth.NewProviderRegistry(),
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

// siteStorage returns the storage backend for the current request's site.
func (h *Handler) siteStorage(c *gin.Context) storage.Backend {
	if h.SiteStorages != nil {
		if siteName := c.GetString("site_name"); siteName != "" {
			if b, ok := h.SiteStorages[siteName]; ok && b != nil {
				return b
			}
		}
	}
	if h.Storage != nil {
		return h.Storage
	}
	if fallbackStorage == nil {
		fallbackStorage, _ = storage.New(storage.Config{Backend: "local"})
	}
	return fallbackStorage
}

// siteAnalyticsWorker returns the analytics worker for the current request's site, or nil.
func (h *Handler) siteAnalyticsWorker(c *gin.Context) *analytics.Worker {
	if w, ok := c.Get("site_analytics_worker"); ok {
		if worker, ok := w.(*analytics.Worker); ok {
			return worker
		}
	}
	return nil
}

// invalidateAnalyticsForDoctype clears the analytics worker's metrics cache
// and triggers regeneration for the given doctype after a config change.
func (h *Handler) invalidateAnalyticsForDoctype(c *gin.Context, doctype string) {
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.InvalidateMetrics(doctype)
	}
}

// siteTx returns a TxManager for the current request's site database and registry.
func (h *Handler) siteTx(c *gin.Context) *orm.TxManager {
	db, _ := c.Get("site_db")
	reg, _ := c.Get("site_registry")
	siteName, _ := c.Get("site_name")
	user, _ := c.Get("user")
	userRole, _ := c.Get("user_role")
	if db != nil && reg != nil {
		if sqlDB, ok := db.(*sql.DB); ok {
			if r, ok := reg.(*doctype.Registry); ok {
				tm := &orm.TxManager{DB: sqlDB, Registry: r, Dialect: h.TxManager.Dialect}
				tm.Context = c.Request.Context()
				if createdAt, ok := c.Get("session_created_at"); ok {
					if ts, ok := createdAt.(time.Time); ok && !ts.IsZero() {
						tm.Context = context.WithValue(tm.Context, "session_created_at", ts)
					}
				}
				if siteNameStr, ok := siteName.(string); ok {
					tm.SiteName = siteNameStr
				}
				if h.SiteEventBuses != nil {
					if siteNameStr, ok := siteName.(string); ok {
						if bus, exists := h.SiteEventBuses[siteNameStr]; exists {
							tm.EventBus = bus
						}
					}
				}
				// Wire the transactional outbox writer for this site.
				if h.SiteOutboxes != nil {
					if siteNameStr, ok := siteName.(string); ok {
						if w, exists := h.SiteOutboxes[siteNameStr]; exists {
							tm.Outbox = w
						}
					}
				}
				// Wire script runner and store.
				tm.ScriptRunner = h.ScriptRunner
				if h.SiteScriptStores != nil {
					if siteNameStr, ok := siteName.(string); ok {
						if store, exists := h.SiteScriptStores[siteNameStr]; exists {
							tm.ScriptStore = store
						}
					}
				}
				// Wire async hook queue.
				tm.AsyncHookSink = h.AsyncHookSink

				// Create script provider for this request (bridges JS → engine).
				if h.ScriptRunner != nil {
					var ss *secret.Store
					if h.SiteSecretStores != nil {
						if siteNameStr, ok := siteName.(string); ok {
							ss = h.SiteSecretStores[siteNameStr]
						}
					}
					if siteNameStr, ok := siteName.(string); ok {
						tm.ScriptProvider = NewScriptProvider(tm, r, siteNameStr, ss, h.ScriptHTTPAllowlist)
					}
				}
				// Set current user context for scripts.
				if u, ok := user.(string); ok {
					tm.CurrentUser = u
				}
				if r, ok := userRole.(string); ok {
					tm.CurrentUserRole = r
				}
				return tm
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
	if def > 0 {
		APIDefaultLimit = def
	}
	if max > 0 {
		APIMaxLimit = max
	}
}

// internalError logs the real error server-side and returns a generic 500 response.
// This prevents sensitive DB/internal details from leaking to API clients.
func internalError(c *gin.Context, msg string, err error) {
	slog.Error(msg, "error", err)
	writeError(c, http.StatusInternalServerError, "server.internal_error", "An internal error occurred", nil)
}

// Meta holds response metadata.
type Meta struct {
	ConfigVersion int    `json:"config_version,omitempty"`
	DocType       string `json:"doctype,omitempty"`
	Total         int    `json:"total,omitempty"`
}

// Response is the standard API response envelope.
type Response struct {
	Data any   `json:"data,omitempty"`
	Meta *Meta `json:"meta,omitempty"`
}

// ErrorResponse is the standard error response envelope.
type ErrorResponse struct {
	Error any   `json:"error"`
	Meta  *Meta `json:"meta,omitempty"`
}

// ErrorBody is the machine-readable error envelope returned to the frontend.
type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func writeError(c *gin.Context, status int, code, message string, details map[string]any) {
	c.JSON(status, ErrorResponse{
		Error: ErrorBody{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}

func badRequestError(c *gin.Context, code, message string, details map[string]any) {
	writeError(c, http.StatusBadRequest, code, message, details)
}

func notFoundError(c *gin.Context, code, message string, details map[string]any) {
	writeError(c, http.StatusNotFound, code, message, details)
}

func conflictError(c *gin.Context, code, message string, details map[string]any) {
	writeError(c, http.StatusConflict, code, message, details)
}

// --- List Handler ---

// checkPerm is a helper that checks permission for the current user and returns
// whether the operation is owner-scoped. Returns true if forbidden (and writes response).
func checkPerm(c *gin.Context, registry *doctype.Registry, docType, operation string) (ownerOnly bool, forbidden bool) {
	// Extension auth: check scoped api_permissions.
	authType := c.GetString("auth_type")
	if authType == "extension" || authType == "channel_session" {
		permsKey := "extension_permissions"
		if authType == "channel_session" {
			permsKey = "channel_permissions"
		}
		permsVal, exists := c.Get(permsKey)
		perms, ok := permsVal.([]doctype.Permission)
		if !exists || !ok || !auth.HasExtensionPermission(perms, docType, operation) {
			c.JSON(http.StatusForbidden, ErrorResponse{
				Error: map[string]string{
					"message": fmt.Sprintf("Token does not have %s permission on %s", operation, docType),
				},
			})
			return false, true
		}
		return false, false
	}

	userRoles := c.GetStringSlice("user_roles")
	if len(userRoles) == 0 {
		// Fallback: if no roles set, allow (bootstrapping / system user).
		return false, false
	}
	allowed, ownerScoped := registry.CanUser(userRoles, docType, operation)
	if !allowed {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: map[string]string{
				"message": fmt.Sprintf("Permission denied: cannot %s on %s", operation, docType),
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
		notFoundError(c, "resource.doctype_not_found", fmt.Sprintf("DocType %q not found", doctypeName), map[string]any{"doctype": doctypeName})
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
	result := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		item := docToMap(doc, dt, h.siteRegistry(c), requestedFields)
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
		if errors.Is(err, orm.ErrNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error: map[string]string{"message": "Document not found"},
			})
			return
		}
		slog.Warn("document get failed", "doctype", doctypeName, "name", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to load document"},
		})
		return
	}

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c), nil),
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
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
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
				badRequestError(c, "validation.invalid_child_table", fmt.Sprintf("Field %s: %s", key, err.Error()), map[string]any{"field": key})
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
	setTemplatePackHash(dt, doc)

	// Run validate hooks (scripts can reject with throw).
	if err := h.siteTx(c).RunHooksForValidate(dt, doc, nil); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Validate.
	validationErrs := doctype.ValidateDocument(dt, doc, h.Registry, nil)
	if validationErrs.HasErrors() {
		writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"fields": validationErrorDetails(validationErrs)})
		return
	}

	// Get current user.
	owner := c.GetString("user")
	if owner == "" {
		owner = "system"
	}

	// Insert.
	if err := h.siteTx(c).Insert(dt, doc, owner, owner); err != nil {
		var valErr *doctype.ValidationError
		if errors.As(err, &valErr) {
			writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"fields": validationErrorDetails(doctype.ValidationErrors{valErr})})
			return
		}
		internalError(c, "insert failed", err)
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}

	c.JSON(http.StatusCreated, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c), nil),
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
		notFoundError(c, "resource.doctype_not_found", fmt.Sprintf("DocType %q not found", doctypeName), map[string]any{"doctype": doctypeName})
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
		notFoundError(c, "resource.document_not_found", "Document not found", map[string]any{"doctype": doctypeName, "name": name})
		return
	}

	// Parse request body.
	var rawData map[string]any
	if err := c.ShouldBindJSON(&rawData); err != nil {
		slog.Warn("invalid JSON in update", "error", err)
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
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
				badRequestError(c, "validation.invalid_child_table", fmt.Sprintf("Field %s: %s", key, err.Error()), map[string]any{"field": key})
				return
			}
			doc.Set(key, children)
		} else if field != nil && field.ReadOnly {
			// Silently ignore read-only fields.
		} else {
			doc.Set(key, val)
		}
	}
	setTemplatePackHash(dt, doc)

	// Template Pack hashes are server-owned. A PUT that resubmits unchanged
	// child rows must still persist a repaired/stale config_hash.
	packHashNeedsSave := dt.Name == "Template Pack" &&
		doc.GetString("config_hash") != oldDoc.GetString("config_hash")
	if h.canShortCircuitNoopUpdate() && !resourceUpdateChanged(dt, oldDoc, rawData) && !packHashNeedsSave {
		c.JSON(http.StatusOK, Response{
			Data: docToMap(oldDoc, dt, h.siteRegistry(c), nil),
			Meta: &Meta{DocType: doctypeName},
		})
		return
	}

	// Run validate hooks (scripts can reject with throw).
	if err := h.siteTx(c).RunHooksForValidate(dt, doc, oldDoc); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Validate.
	validationErrs := doctype.ValidateDocument(dt, doc, h.Registry, oldDoc)
	if validationErrs.HasErrors() {
		writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"fields": validationErrorDetails(validationErrs)})
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
			writeError(c, http.StatusBadRequest, "validation.failed", "Validation failed", map[string]any{"fields": validationErrorDetails(doctype.ValidationErrors{valErr})})
			return
		}
		internalError(c, "save failed", err)
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c), nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

func (h *Handler) canShortCircuitNoopUpdate() bool {
	return h.ScriptRunner == nil
}

func resourceUpdateChanged(dt *doctype.DocType, oldDoc *doctype.Document, rawData map[string]any) bool {
	for key, newVal := range rawData {
		field := dt.GetField(key)
		if field == nil || field.ReadOnly {
			continue
		}
		if field.Fieldtype == "Table" {
			return true
		}
		if !resourceFieldValuesEqual(field, oldDoc.Get(key), newVal) {
			return true
		}
	}
	return false
}

func resourceFieldValuesEqual(field *doctype.Field, oldVal, newVal any) bool {
	if oldVal == nil || newVal == nil {
		return oldVal == newVal
	}

	switch field.Fieldtype {
	case "Int":
		oldInt, okOld := anyToInt64(oldVal)
		newInt, okNew := anyToInt64(newVal)
		return okOld && okNew && oldInt == newInt
	case "Float", "Currency", "Percent":
		oldFloat, okOld := anyToFloat64(oldVal)
		newFloat, okNew := anyToFloat64(newVal)
		return okOld && okNew && oldFloat == newFloat
	case "Check":
		oldBool, okOld := anyToBool(oldVal)
		newBool, okNew := anyToBool(newVal)
		return okOld && okNew && oldBool == newBool
	case "JSON":
		return jsonValuesEqual(oldVal, newVal)
	default:
		return reflect.DeepEqual(oldVal, newVal)
	}
}

func anyToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) {
			return int64(n), true
		}
	case []byte:
		return anyToInt64(string(n))
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

func anyToFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case []byte:
		return anyToFloat64(string(n))
	case string:
		parsed, err := strconv.ParseFloat(n, 64)
		return parsed, err == nil
	}
	return 0, false
}

func anyToBool(v any) (bool, bool) {
	switch b := v.(type) {
	case bool:
		return b, true
	case int64:
		return b != 0, true
	case float64:
		return b != 0, true
	case []byte:
		return anyToBool(string(b))
	case string:
		if b == "1" {
			return true, true
		}
		if b == "0" {
			return false, true
		}
		parsed, err := strconv.ParseBool(b)
		return parsed, err == nil
	}
	return false, false
}

func jsonValuesEqual(oldVal, newVal any) bool {
	var oldDecoded any
	if s, ok := oldVal.(string); ok {
		if err := json.Unmarshal([]byte(s), &oldDecoded); err == nil {
			oldVal = oldDecoded
		}
	}
	var newDecoded any
	if s, ok := newVal.(string); ok {
		if err := json.Unmarshal([]byte(s), &newDecoded); err == nil {
			newVal = newDecoded
		}
	}
	return reflect.DeepEqual(oldVal, newVal)
}

// --- Delete Handler ---

// HandleDelete handles DELETE /api/resource/{doctype}/{name}
func (h *Handler) HandleDelete(c *gin.Context) {
	doctypeName := c.Param("doctype")
	name := c.Param("name")

	dt := h.siteRegistry(c).Get(doctypeName)
	if dt == nil {
		notFoundError(c, "resource.doctype_not_found", fmt.Sprintf("DocType %q not found", doctypeName), map[string]any{"doctype": doctypeName})
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
		notFoundError(c, "resource.document_not_found", "Document not found", map[string]any{"doctype": doctypeName, "name": name})
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}

	c.JSON(http.StatusOK, Response{
		Data: map[string]string{"message": "deleted"},
		Meta: &Meta{DocType: doctypeName},
	})
}

// --- Helpers ---

// docToMap converts a Document to a map for JSON serialization, including system fields.
func docToMap(doc *doctype.Document, dt *doctype.DocType, registry *doctype.Registry, requestedFields []string) map[string]any {
	result := make(map[string]any, len(dt.DataFields())+2)
	result["name"] = doc.Name
	result["doc_status"] = doc.DocStatus

	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			children := doc.GetTable(f.Fieldname)
			if children != nil {
				childDT := dtRegistryLookup(registry, dt, f.Fieldname)
				childMaps := make([]map[string]any, 0, len(children))
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

func validationErrorDetails(errors doctype.ValidationErrors) any {
	if len(errors) == 1 {
		return map[string]any{
			"code":    errors[0].Type,
			"message": errors[0].Message,
			"field":   errors[0].Field,
			"doctype": errors[0].DocType,
		}
	}

	var messages []map[string]any
	for _, e := range errors {
		messages = append(messages, map[string]any{
			"code":    e.Type,
			"message": e.Message,
			"field":   e.Field,
			"doctype": e.DocType,
		})
	}
	return messages
}

// RegisterRoutes registers all CRUD routes for all DocTypes in the registry on a full Engine.
func RegisterRoutes(router *gin.Engine, registry *doctype.Registry, txManager *orm.TxManager) {
	handler := NewHandler(registry, txManager)
	RegisterRoutesOnGroup(router.Group("/api"), registry, txManager)

	// Health check — used by Docker HEALTHCHECK and load balancers.
	// Verifies DB connectivity for readiness probes.
	router.GET("/api/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{"message": "pong"})
	})
	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, healthPayload(c))
	})

	// File upload endpoint.
	router.POST("/api/upload", handler.HandleUpload)

	_ = handler
}

func healthPayload(c *gin.Context) gin.H {
	db, _ := c.Get("site_db")
	status := "ok"
	dbStatus := "connected"
	if sqlDB, ok := db.(*sql.DB); ok {
		if err := sqlDB.Ping(); err != nil {
			dbStatus = "disconnected"
			status = "degraded"
		}
	} else {
		dbStatus = "unknown"
	}
	return gin.H{
		"status": status,
		"db":     dbStatus,
		// Async hook overflow count (RFC Phase 1: no silent drops).
		"async_hook_enqueue_failed": orm.HookEnqueueFailedCount(),
	}
}

// RegisterRoutesOnGroup registers all CRUD routes on an existing RouterGroup.
// This allows the caller to apply middleware (e.g., auth) before the group.
func RegisterRoutesOnGroup(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager) {
	RegisterRoutesOnGroupWithAnalytics(apiGroup, registry, txManager, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

// RegisterPublicRoutesOnGroup registers unauthenticated, read-only public
// delivery routes. Access is controlled by DocType public_access config.
func RegisterPublicRoutesOnGroup(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteStorages map[string]storage.Backend) {
	handler := NewHandler(registry, txManager)
	handler.SiteStorages = siteStorages
	public := apiGroup.Group("/public/resource")
	{
		public.GET("/:doctype", handler.HandlePublicList)
		public.GET("/:doctype/:name", handler.HandlePublicGet)
	}

	// Public view routes (unauthenticated, three-layer security check).
	apiGroup.GET("/v", handler.HandlePublicView)
	apiGroup.POST("/v", handler.HandlePublicCreate)
	apiGroup.GET("/public/files/*path", handler.HandlePublicFileServe)

	// DigiTax cannot use a Kora session/CSRF token. The handler authenticates
	// this callback with the site's digitax_webhook_secret instead.
	apiGroup.POST("/webhooks/digitax/etims", handler.HandleDigiTaxWebhook)
}

// RegisterRoutesOnGroupWithAnalytics registers all CRUD routes with optional
// analytics event propagation. siteBuses maps site name → EventBus; if nil or
// empty, analytics event emission is a no-op.
func RegisterRoutesOnGroupWithAnalytics(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteBuses map[string]analytics.EventBus, realtimeProviders map[string]*natsprovider.Provider, scriptRunner script.Runner, siteScriptStores map[string]*script.Store, siteSecretStores map[string]*secret.Store, httpAllowlist []string, siteWebhookWorkers map[string]*webhook.Worker, asyncHookSink orm.AsyncHookSink, siteOutboxes map[string]outbox.Writer, siteStorages map[string]storage.Backend, kernelCommands *kernel.CommandRegistry) {
	handler := NewHandler(registry, txManager)
	handler.SiteEventBuses = siteBuses
	handler.SiteRealtimeProviders = realtimeProviders
	handler.ScriptRunner = scriptRunner
	handler.SiteScriptStores = siteScriptStores
	handler.SiteSecretStores = siteSecretStores
	handler.ScriptHTTPAllowlist = httpAllowlist
	handler.SiteWebhookWorkers = siteWebhookWorkers
	handler.AsyncHookSink = asyncHookSink
	handler.SiteOutboxes = siteOutboxes
	handler.SiteStorages = siteStorages
	handler.KernelCommands = kernelCommands

	// File attachments: upload + authenticated serving (Range-aware for audio/video).
	apiGroup.POST("/upload", handler.HandleUpload)
	apiGroup.GET("/files/*path", handler.HandleFileServe)
	apiGroup.DELETE("/files/*path", handler.HandleFileDelete)

	resource := apiGroup.Group("/resource")
	{
		resource.GET("/:doctype", handler.HandleList)
		resource.POST("/:doctype", handler.HandleCreate)
		resource.GET("/:doctype/:name", handler.HandleGet)
		resource.PUT("/:doctype/:name", handler.HandleUpdate)
		resource.DELETE("/:doctype/:name", handler.HandleDelete)
		resource.POST("/:doctype/:name/workflow_action", handler.HandleWorkflowAction)
	}

	// Operation kernel: canonical command path shared by every adapter
	// (first vertical slice — record.create, record.update) plus the
	// config-defined command registry (KERNEL-008).
	apiGroup.POST("/kernel/:command", handler.HandleKernelOperation)
	apiGroup.GET("/kernel/_registry", handler.HandleKernelRegistry)

	// OpenAPI docs.
	apiGroup.GET("/openapi.json", handler.HandleOpenAPI)
	apiGroup.GET("/swagger-ui", handler.HandleSwaggerUI)

	// AI Chat.
	apiGroup.POST("/chat", handler.HandleChat)
	aiGroup := apiGroup.Group("/ai")
	{
		aiGroup.POST("/approvals/:id/grant", handler.HandleAIGrantApproval)
		aiGroup.POST("/runs/:id/cancel", handler.HandleAICancel)
		aiGroup.POST("/runs/:id/resume", handler.HandleAIResume)
		aiGroup.POST("/retention/cleanup", handler.HandleAIRetentionCleanup)
	}

	channel := apiGroup.Group("/internal/channel")
	{
		channel.POST("/sessions/issue", handler.HandleChannelSessionIssue)
		channel.POST("/sessions/revoke", handler.HandleChannelSessionRevoke)
		channel.GET("/tools", handler.HandleChannelTools)
		channel.POST("/query", handler.HandleChannelQuery)
		channel.POST("/mutate", handler.HandleChannelMutate)
	}

	// Custom API methods (user-defined scripts).
	// Scripts are registered as api_method in _kora_script, accessible at /api/method/{name}.
	method := apiGroup.Group("/method")
	{
		method.POST("/:name", handler.HandleMethod)
		method.GET("/:name", handler.HandleMethod)
	}

	// RFC-native page manifest management endpoints.
	pageManifests := apiGroup.Group("/system/page-manifests")
	{
		pageManifests.GET("", handler.HandleSystemPageManifests)
		pageManifests.POST("", handler.HandleSystemPageManifestCreate)
		pageManifests.GET("/:name", handler.HandleSystemPageManifest)
		pageManifests.PUT("/:name", handler.HandleSystemPageManifestUpdate)
		pageManifests.DELETE("/:name", handler.HandleSystemPageManifestDelete)
	}

	// RFC-native runtime page route resolution.
	apiGroup.GET("/page-manifests", handler.HandlePageManifestByRoute)

	// System config endpoints.
	system := apiGroup.Group("/system/config")
	{
		system.GET("/versions", handler.HandleConfigVersions)
		system.GET("/versions/:id", handler.HandleConfigVersion)
		system.GET("/diff", handler.HandleConfigDiff)
	}

	// System schema/navigation endpoints.
	RegisterSystemRoutes(apiGroup, handler)

	// Script management (CRUD for _kora_script).
	scripts := apiGroup.Group("/system/script")
	{
		scripts.GET("", handler.HandleScriptList)
		scripts.POST("", handler.HandleScriptCreate)
		scripts.GET("/:name", handler.HandleScriptGet)
		scripts.PUT("/:name", handler.HandleScriptUpdate)
		scripts.DELETE("/:name", handler.HandleScriptDelete)
		scripts.POST("/:name/validate", handler.HandleScriptValidate)
		scripts.GET("/:name/executions", handler.HandleScriptExecutions)
	}

	// Extension management (CRUD for _kora_extension).
	ext := apiGroup.Group("/system/extension")
	{
		ext.GET("", handler.HandleExtensionList)
		ext.POST("", handler.HandleExtensionCreate)
		ext.GET("/:name", handler.HandleExtensionGet)
		ext.PUT("/:name", handler.HandleExtensionUpdate)
		ext.DELETE("/:name", handler.HandleExtensionDelete)
		ext.GET("/:name/deliveries", handler.HandleExtensionDeliveries)
		ext.POST("/:name/replay", handler.HandleExtensionReplay)
		ext.POST("/:name/rotate-secret", handler.HandleExtensionRotateSecret)
	}

	// Analytics endpoints (no-op if siteBuses is empty).
	RegisterAnalyticsRoutes(apiGroup, registry, txManager.DB, siteBuses, txManager.Dialect)
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

	// Execute on_transition actions BEFORE state change (can abort).
	transition := h.siteRegistry(c).Workflows.GetTransition(doctypeName, currentState, req.Action)
	if transition != nil {
		if err := h.executeWorkflowActionsSync(c, transition.OnTransition, doctypeName, doc, userRole); err != nil {
			// Run on_failure actions if transition is blocked by a script.
			h.executeWorkflowActions(c, transition.OnFailure, doctypeName, doc, userRole)
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Transition blocked: %v", err)},
			})
			return
		}
	}

	// Apply transition.
	newState, newDocStatus, err := h.siteRegistry(c).Workflows.ApplyTransition(doctypeName, currentState, req.Action, userRole, doc)
	if err != nil {
		// Run on_failure actions if transition validation fails.
		if transition != nil {
			h.executeWorkflowActions(c, transition.OnFailure, doctypeName, doc, userRole)
		}
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

	// Capture pre-mutation state for analytics (so worker can detect state transition).
	oldFields := make(map[string]any)
	for k, v := range doc.Fields {
		oldFields[k] = v
	}
	oldDocStatus := doc.DocStatus
	if statusField != nil {
		doc.Set(statusField.Fieldname, newState)
	}
	doc.DocStatus = newDocStatus

	// Save.
	modifiedBy := c.GetString("user")
	if modifiedBy == "" {
		modifiedBy = "system"
	}
	if err := h.siteTx(c).Save(dt, doc, modifiedBy, owner, &doctype.Document{Fields: oldFields, DocStatus: oldDocStatus}); err != nil {
		internalError(c, "workflow save failed", err)
		return
	}

	// Dispatch workflow notifications.
	dispatchNotifications(h.Registry, doctypeName, newState, doc)

	// Execute workflow on_success actions (best-effort, after state change committed).
	if transition != nil {
		h.executeWorkflowActions(c, transition.OnSuccess, doctypeName, doc, userRole)
	}

	c.JSON(http.StatusOK, Response{
		Data: docToMap(doc, dt, h.siteRegistry(c), nil),
		Meta: &Meta{DocType: doctypeName},
	})
}

// executeWorkflowActionsSync runs workflow on_transition actions synchronously.
// Returns the first error encountered (which aborts the transition).
func (h *Handler) executeWorkflowActionsSync(c *gin.Context, actions []doctype.WorkflowAction, doctypeName string, doc *doctype.Document, userRole string) error {
	if len(actions) == 0 || h.ScriptRunner == nil {
		return nil
	}
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	user, _ := c.Get("user")
	userStr, _ := user.(string)

	for _, action := range actions {
		if action.Type != "script" || action.Script == "" {
			continue
		}
		if h.SiteScriptStores == nil {
			continue
		}
		store, exists := h.SiteScriptStores[siteNameStr]
		if !exists || store == nil {
			continue
		}

		scripts, err := store.LoadWorkflowActionScripts(siteNameStr, action.Script)
		if err != nil {
			return fmt.Errorf("loading workflow action %q: %w", action.Script, err)
		}

		for _, rec := range scripts {
			execReq := script.ExecuteRequest{
				Script:     rec.Script,
				ScriptType: script.TypeWorkflowAction,
				ScriptName: rec.Name,
				DocType:    doctypeName,
				Document:   doc.Fields,
				User:       userStr,
				UserRoles:  []string{userRole},
				Site:       siteNameStr,
				Timeout:    time.Duration(rec.TimeoutMs) * time.Millisecond,
				Provider:   nil, // on_transition scripts validate/reject; CRUD not needed here
			}

			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(rec.TimeoutMs)*time.Millisecond)
			defer cancel()

			_, execErr := h.ScriptRunner.Execute(ctx, execReq)
			if execErr != nil {
				if store != nil {
					_ = store.LogExecution(siteNameStr, rec, doctypeName, doc.Name, "", userStr, 0, "error", execErr.Error())
				}
				return fmt.Errorf("script %q: %w", rec.Name, execErr)
			}
			if store != nil {
				_ = store.LogExecution(siteNameStr, rec, doctypeName, doc.Name, "", userStr, 0, "success", "")
			}
		}
	}
	return nil
}

// executeWorkflowActions runs workflow action scripts (best-effort, errors logged).
func (h *Handler) executeWorkflowActions(c *gin.Context, actions []doctype.WorkflowAction, doctypeName string, doc *doctype.Document, userRole string) {
	if len(actions) == 0 || h.ScriptRunner == nil {
		return
	}
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	user, _ := c.Get("user")
	userStr, _ := user.(string)

	for _, action := range actions {
		if action.Type != "script" || action.Script == "" {
			continue
		}

		// Look up the script.
		if h.SiteScriptStores == nil {
			continue
		}
		store, exists := h.SiteScriptStores[siteNameStr]
		if !exists || store == nil {
			continue
		}

		scripts, err := store.LoadWorkflowActionScripts(siteNameStr, action.Script)
		if err != nil {
			slog.Warn("loading workflow action script", "action", action.Script, "error", err)
			continue
		}

		for _, rec := range scripts {
			execReq := script.ExecuteRequest{
				Script:     rec.Script,
				ScriptType: script.TypeWorkflowAction,
				ScriptName: rec.Name,
				DocType:    doctypeName,
				Document:   doc.Fields,
				User:       userStr,
				UserRoles:  []string{userRole},
				Site:       siteNameStr,
				Timeout:    time.Duration(rec.TimeoutMs) * time.Millisecond,
			}

			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(rec.TimeoutMs)*time.Millisecond)
			defer cancel()

			result, execErr := h.ScriptRunner.Execute(ctx, execReq)
			durationMs := 0
			status := "success"
			var errMsg string
			if result != nil {
				durationMs = int(result.Duration.Milliseconds())
			}
			if execErr != nil {
				status = "error"
				errMsg = execErr.Error()
				slog.Warn("workflow action script failed", "action", action.Script, "script", rec.Name, "error", execErr)
			}
			if store != nil {
				_ = store.LogExecution(siteNameStr, rec, doctypeName, doc.Name, "", userStr, durationMs, status, errMsg)
			}
		}
	}
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
		"SELECT id, site, version, created_at, created_by, label, COALESCE(status, CASE WHEN is_active = 1 THEN 'Active' ELSE 'Superseded' END) as status FROM _kora_config_version ORDER BY version DESC LIMIT 50",
	)
	if err != nil {
		internalError(c, "config versions query failed", err)
		return
	}
	defer rows.Close()

	var versions []map[string]any
	for rows.Next() {
		var id, site, createdBy, label, createdAt, status string
		var version int
		if err := rows.Scan(&id, &site, &version, &createdAt, &createdBy, &label, &status); err != nil {
			continue
		}
		versions = append(versions, map[string]any{
			"id": id, "site": site, "version": version,
			"created_at": createdAt, "created_by": createdBy,
			"label": label, "status": status,
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
		writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
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
		writeError(c, http.StatusBadRequest, "validation.required_field", "from and to required", map[string]any{"fields": []string{"from", "to"}})
		return
	}
	// First check if the "to" version has a stored change_list.
	var changeList string
	h.siteTx(c).DB.QueryRow("SELECT COALESCE(change_list, '') FROM _kora_config_version WHERE id = ?", toID).Scan(&changeList)
	if changeList != "" {
		var diff doctype.ConfigDiffFull
		if err := json.Unmarshal([]byte(changeList), &diff); err == nil && diff.Doctypes != nil && len(diff.Doctypes.Changes) > 0 {
			c.JSON(http.StatusOK, Response{Data: diff.Doctypes})
			return
		}
	}
	// Fallback: parse config columns (handles JSON and s-expression).
	var fromConfig, toConfig string
	h.siteTx(c).DB.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", fromID).Scan(&fromConfig)
	h.siteTx(c).DB.QueryRow("SELECT config FROM _kora_config_version WHERE id = ?", toID).Scan(&toConfig)
	if fromConfig == "" || toConfig == "" {
		writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
		return
	}
	fromSnapshot, fromErr := doctype.ParseConfig(fromConfig)
	toSnapshot, toErr := doctype.ParseConfig(toConfig)
	if fromErr != nil || toErr != nil {
		var from, to []*doctype.DocType
		yaml.Unmarshal([]byte(fromConfig), &from)
		yaml.Unmarshal([]byte(toConfig), &to)
		diff := doctype.DiffConfigs(from, to)
		c.JSON(http.StatusOK, Response{Data: diff})
		return
	}
	diff := doctype.DiffConfigs(fromSnapshot.DocTypes, toSnapshot.DocTypes)
	c.JSON(http.StatusOK, Response{Data: diff})
}

// HandleUpload handles file uploads via multipart form.
// POST /upload
// Stores files via the configured storage backend and returns a file reference
// (path + metadata) for storage in an Attach / Attach Image / Attach Audio field.
func (h *Handler) HandleUpload(c *gin.Context) {
	maxBytes := maxUploadBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	// Use a modest in-memory threshold; larger uploads spill to temp files.
	if err := c.Request.ParseMultipartForm(10 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, ErrorResponse{
				Error: map[string]string{"message": "That file is too large to upload."},
			})
			return
		}
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "No file provided"},
		})
		return
	}

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

	// Sniff the first bytes to validate content (never trust headers alone).
	sniff := make([]byte, 512)
	n, _ := io.ReadFull(file, sniff)
	sniff = sniff[:n]

	// fieldtype + accept (optional) from the uploader drive MIME/extension rules.
	if fieldtype, accept := c.Request.FormValue("fieldtype"), c.Request.FormValue("accept"); fieldtype != "" || accept != "" {
		if err := validateUploadType(fieldtype, accept, header.Filename, sniff); err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": err.Error()},
			})
			return
		}
	}

	// Build a site-scoped key: sites/<site>/files/<YYYY>/<MM>/<filename>.
	now := time.Now()
	filename := sanitizeFilename(header.Filename)
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	dir := filepath.Join("sites", siteName, "files",
		fmt.Sprintf("%04d", now.Year()),
		fmt.Sprintf("%02d", now.Month()))
	key := filepath.Join(dir, filename)

	backend := h.siteStorage(c)

	// Avoid collisions by appending _1, _2, ... until the key is free.
	for i := 1; ; i++ {
		if _, err := backend.Head(c.Request.Context(), filepath.ToSlash(key)); err != nil {
			break
		}
		key = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
	}

	// Resolve a display MIME type: sniffed content first, extension fallback.
	mimeType := http.DetectContentType(sniff)
	if mimeType == "application/octet-stream" || mimeType == "" {
		if m := storage.MimeByExt(ext); m != "" {
			mimeType = m
		}
	}

	// Rewind, read the bytes once, and give image uploads a post-upload
	// optimization pass. This keeps the same URL but reduces payload size.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		internalError(c, "reading file", err)
		return
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		internalError(c, "reading upload", err)
		return
	}

	optimized, optimizedMime, optimizedOk := optimizeUploadedImage(raw, filename, mimeType)
	if optimizedOk {
		raw = optimized
		mimeType = optimizedMime
		header.Size = int64(len(raw))
	}

	meta, err := backend.Put(c.Request.Context(), filepath.ToSlash(key), bytes.NewReader(raw), int64(len(raw)), storage.FileMeta{
		Filename:   filename,
		MIMEType:   mimeType,
		UploadedBy: c.GetString("user"),
		UploadedAt: now,
	})
	if err != nil {
		internalError(c, "storing file", err)
		return
	}

	c.JSON(http.StatusCreated, Response{
		Data: map[string]any{
			"path":      meta.Key,
			"filename":  meta.Filename,
			"key":       meta.Key,
			"mime_type": meta.MIMEType,
			"size":      meta.Size,
			"checksum":  meta.Checksum,
		},
	})
}

// optimizeUploadedImage tries to reduce upload size for browser-rendered images
// without changing the public path. It returns the original bytes when no
// improvement is possible or when the file is not a raster image.
func optimizeUploadedImage(raw []byte, filename, mimeType string) ([]byte, string, bool) {
	if len(raw) < 32*1024 {
		return raw, mimeType, false
	}

	if strings.EqualFold(filepath.Ext(filename), ".svg") || mimeType == "image/svg+xml" {
		return raw, mimeType, false
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mimeType, false
	}

	var buf bytes.Buffer
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
			return raw, mimeType, false
		}
		if buf.Len() >= len(raw) {
			return raw, mimeType, false
		}
		return buf.Bytes(), "image/jpeg", true
	case "png":
		if hasAlphaChannel(img) {
			enc := png.Encoder{CompressionLevel: png.BestCompression}
			if err := enc.Encode(&buf, img); err != nil {
				return raw, mimeType, false
			}
			if buf.Len() >= len(raw) {
				return raw, mimeType, false
			}
			return buf.Bytes(), "image/png", true
		}
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
			return raw, mimeType, false
		}
		if buf.Len() >= len(raw) {
			return raw, mimeType, false
		}
		return buf.Bytes(), "image/jpeg", true
	default:
		return raw, mimeType, false
	}
}

func hasAlphaChannel(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0xffff {
				return true
			}
		}
	}
	return false
}

// HandleFileServe serves an uploaded attachment with HTTP Range support so browsers
// can seek within audio/video. Access is enforced by the SiteGuard middleware.
// GET /files/*path
func (h *Handler) HandleFileServe(c *gin.Context) {
	h.serveStoredFile(c)
}

// HandlePublicFileServe serves a site attachment without auth for public assets.
// GET /public/files/*path
func (h *Handler) HandlePublicFileServe(c *gin.Context) {
	h.serveStoredFile(c)
}

func (h *Handler) serveStoredFile(c *gin.Context) {
	key, err := fileKeyForSite(c)
	if err != nil {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: map[string]string{"message": "Invalid file path"},
		})
		return
	}

	backend := h.siteStorage(c)
	meta, err := backend.Head(c.Request.Context(), key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error: map[string]string{"message": "File not found"},
			})
			return
		}
		internalError(c, "statting file", err)
		return
	}

	ext := filepath.Ext(key)
	ctype := meta.MIMEType
	if ctype == "" {
		ctype = storage.MimeByExt(ext)
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	c.Header("Content-Type", ctype)
	c.Header("X-Content-Type-Options", "nosniff")
	if !storage.IsInlineSafe(ext) {
		safeName := strings.ReplaceAll(meta.Filename, `"`, "")
		c.Header("Content-Disposition", `attachment; filename="`+safeName+`"`)
	}

	offset, length := int64(0), int64(-1)
	status := http.StatusOK
	if rangeHeader := c.GetHeader("Range"); rangeHeader != "" {
		if start, end, ok := parseRange(rangeHeader, meta.Size); ok {
			offset = start
			length = end - start + 1
			status = http.StatusPartialContent
			c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, meta.Size))
			c.Header("Content-Length", strconv.FormatInt(length, 10))
		} else {
			c.Header("Content-Length", strconv.FormatInt(meta.Size, 10))
		}
	} else {
		c.Header("Content-Length", strconv.FormatInt(meta.Size, 10))
	}

	rc, err := backend.Open(c.Request.Context(), key, offset, length)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error: map[string]string{"message": "File not found"},
			})
			return
		}
		internalError(c, "opening file", err)
		return
	}
	defer rc.Close()

	c.Status(status)
	if _, err := io.Copy(c.Writer, rc); err != nil {
		slog.Error("streaming file", "error", err)
	}
}

// HandleFileDelete removes an uploaded attachment.
// DELETE /files/*path
func (h *Handler) HandleFileDelete(c *gin.Context) {
	key, err := fileKeyForSite(c)
	if err != nil {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: map[string]string{"message": "Invalid file path"},
		})
		return
	}
	backend := h.siteStorage(c)
	if err := backend.Delete(c.Request.Context(), key); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error: map[string]string{"message": "File not found"},
			})
			return
		}
		internalError(c, "deleting file", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]string{"deleted": key}})
}

// fileKeyForSite extracts and validates a site-scoped storage key from the request path.
func fileKeyForSite(c *gin.Context) (string, error) {
	return fileKeyForSiteReference(c, c.Param("path"))
}

// fileKeyForSiteReference normalizes both current site-scoped keys and legacy
// attachment values that contain only the date/filename suffix.
func fileKeyForSiteReference(c *gin.Context, reference string) (string, error) {
	siteName := c.GetString("site_name")
	if siteName == "" {
		siteName = "default"
	}
	rel := filepath.Clean(strings.TrimPrefix(strings.TrimSpace(reference), "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid file path")
	}
	// Never serve internal sidecar metadata files.
	if strings.HasSuffix(strings.ToLower(rel), ".meta.json") {
		return "", fmt.Errorf("invalid file path")
	}
	base := filepath.Join("sites", siteName, "files")
	if rel == base || strings.HasPrefix(rel, base+string(os.PathSeparator)) {
		return filepath.ToSlash(rel), nil
	}
	// Do not allow a reference scoped to another site to be silently remapped.
	if strings.HasPrefix(rel, "sites"+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid file path")
	}
	return filepath.ToSlash(filepath.Join(base, rel)), nil
}

// parseRange parses a single "bytes=start-end" range against size. It returns
// (start, end, ok) where end is inclusive. It returns ok=false for invalid,
// multi-range, or unsatisfiable requests.
func parseRange(s string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(s, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(s, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startStr := strings.TrimSpace(spec[:dash])
	endStr := strings.TrimSpace(spec[dash+1:])

	var start, end int64
	if startStr == "" {
		// Suffix range: last N bytes.
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		start = size - n
		end = size - 1
	} else {
		var err error
		start, err = strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false
		}
		if endStr == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil || end < start {
				return 0, 0, false
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return start, end, true
}

// maxUploadBytes returns the per-file upload limit in bytes (KORA_MAX_UPLOAD_MB, default 50 MB).
func maxUploadBytes() int64 {
	if v := os.Getenv("KORA_MAX_UPLOAD_MB"); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			return mb << 20
		}
	}
	return 50 << 20
}

// sanitizeFilename strips path separators and unsafe characters from an uploaded filename.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == string(os.PathSeparator) {
		return "file"
	}
	return name
}

// validateUploadType enforces per-fieldtype or per-field accept rules on an upload.
// If accept is non-empty, it takes precedence over the fieldtype defaults.
func validateUploadType(fieldtype, accept, filename string, sniffed []byte) error {
	ext := strings.ToLower(filepath.Ext(filename))
	if accept != "" {
		return validateAgainstAccept(accept, ext, sniffed)
	}
	switch fieldtype {
	case "Attach Image":
		if ct := http.DetectContentType(sniffed); !strings.HasPrefix(ct, "image/") {
			return fmt.Errorf("Please choose an image file.")
		}
	case "Attach Audio":
		if !audioExts[ext] {
			return fmt.Errorf("Please choose an audio file (MP3, WAV, OGG, M4A, or FLAC).")
		}
	}
	return nil
}

// validateAgainstAccept checks an upload against a comma/newline-separated accept list
// of extensions (".pdf") and MIME types ("image/*", "application/pdf").
func validateAgainstAccept(accept, ext string, sniffed []byte) error {
	mime := http.DetectContentType(sniffed)
	normalized := strings.ReplaceAll(accept, "\n", ",")
	var friendly []string
	for _, p := range strings.Split(normalized, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		friendly = append(friendly, p)
		if strings.HasPrefix(p, ".") {
			if ext == strings.ToLower(p) {
				return nil
			}
			continue
		}
		if strings.Contains(p, "/") {
			if p == mime || (strings.HasSuffix(p, "/*") && strings.HasPrefix(mime, strings.TrimSuffix(p, "*"))) {
				return nil
			}
		}
	}
	return fmt.Errorf("Please choose a file in one of these formats: %s.", strings.Join(friendly, ", "))
}

// audioExts lists supported audio extensions for Attach Audio validation.
var audioExts = map[string]bool{
	".mp3": true, ".wav": true, ".ogg": true, ".oga": true,
	".m4a": true, ".aac": true, ".flac": true, ".opus": true, ".webm": true,
}

// HandleMethod handles POST/GET /api/method/{name} — user-defined custom API endpoints.
// Scripts registered with script_type='api_method' and matching method_path are executed.
func (h *Handler) HandleMethod(c *gin.Context) {
	methodName := c.Param("name")
	if methodName == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "method name is required"},
		})
		return
	}

	// Get site context.
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)

	// Look up the script.
	if h.SiteScriptStores == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("method %q not found", methodName)},
		})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("method %q not found", methodName)},
		})
		return
	}

	rec, err := store.LoadMethodScript(siteNameStr, methodName)
	if err != nil {
		slog.Error("loading method script", "method", methodName, "site", siteNameStr, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to load method"},
		})
		return
	}
	if rec == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("method %q not found", methodName)},
		})
		return
	}

	// Parse request body.
	var reqBody map[string]any
	if c.Request.Method == "POST" && c.Request.Body != nil {
		if err := c.ShouldBindJSON(&reqBody); err != nil {
			reqBody = make(map[string]any) // GET or empty body
		}
	}
	if reqBody == nil {
		reqBody = make(map[string]any)
	}

	// Get user context.
	user, _ := c.Get("user")
	userRole, _ := c.Get("user_role")
	userStr, _ := user.(string)
	userRoleStr, _ := userRole.(string)

	// Execute the script.
	if h.ScriptRunner == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Script runner not available"},
		})
		return
	}

	// Build a provider for API method scripts (enables kora.getDoc, kora.http, etc.)
	tx := h.siteTx(c)
	var ss *secret.Store
	if h.SiteSecretStores != nil {
		ss = h.SiteSecretStores[siteNameStr]
	}
	provider := NewScriptProvider(tx, h.siteRegistry(c), siteNameStr, ss, h.ScriptHTTPAllowlist)

	execReq := script.ExecuteRequest{
		Script:     rec.Script,
		ScriptType: script.TypeAPIMethod,
		ScriptName: rec.Name,
		DocType:    rec.DocType,
		Document:   reqBody,
		User:       userStr,
		UserRoles:  []string{userRoleStr},
		Site:       siteNameStr,
		Timeout:    time.Duration(rec.TimeoutMs) * time.Millisecond,
		Provider:   provider,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(rec.TimeoutMs)*time.Millisecond)
	defer cancel()

	result, err := h.ScriptRunner.Execute(ctx, execReq)
	durationMs := 0
	if result != nil {
		durationMs = int(result.Duration.Milliseconds())
	}

	// Log execution.
	if store != nil {
		_ = store.LogExecution(siteNameStr, *rec, "", "", "", userStr, durationMs,
			func() string {
				if err != nil {
					return "error"
				}
				return "success"
			}(),
			func() string {
				if err != nil {
					return err.Error()
				}
				return ""
			}())
	}

	if err != nil {
		slog.Warn("custom method error", "method", methodName, "error", err)
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": err.Error()},
		})
		return
	}

	// Return the script's result or success.
	if result != nil && result.Result != nil {
		c.JSON(http.StatusOK, Response{Data: result.Result})
		return
	}
	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "ok"}})
}
