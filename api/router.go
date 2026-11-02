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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/asenawritescode/kora/auth"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/asenawritescode/kora/org"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/script"
	"github.com/asenawritescode/kora/secret"
	"github.com/asenawritescode/kora/storage"
	"github.com/asenawritescode/kora/webhook"
	"github.com/gin-gonic/gin"
)

// BinaryVersion is set at startup from cli.Version to avoid circular imports.
// cli/serve.go sets this to its Version value during initialization.
var BinaryVersion = "dev"

// fallbackStorage is used when no site-specific storage backend is configured.
var fallbackStorage storage.Backend
var fallbackStorageOnce sync.Once
var fallbackStorageErr error

// Handler holds dependencies for API handlers.
// Registry and TxManager are fallbacks; handlers read site context from the request.
type Handler struct {
	Registry            *doctype.Registry
	TxManager           *orm.TxManager
	AuthProviders       *auth.ProviderRegistry
	RuntimeServices     *SiteRuntimeServices
	RealtimeConnections *RealtimeConnectionOwner

	// SiteEventBuses maps site name → EventBus for analytics/realtime services.
	SiteEventBuses map[string]eventstream.Bus

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
	AgentStores    map[string]*org.AgentStore
}

func (h *Handler) runtimeService(c *gin.Context, site string) SiteRuntimeService {
	// A router-pinned snapshot belongs to the loaded tenant generation and is
	// authoritative even when compatibility registries are configured.
	if c != nil {
		if value, ok := c.Get("site_runtime_services"); ok {
			if services, ok := value.(SiteRuntimeService); ok {
				return services
			}
		}
	}
	if h.RuntimeServices != nil {
		if services, ok := h.RuntimeServices.Get(site); ok {
			return services
		}
	}
	return h.legacyRuntimeService(site)
}

func (h *Handler) legacyRuntimeService(site string) SiteRuntimeService {
	return SiteRuntimeService{
		EventBus: h.SiteEventBuses[site], Realtime: h.SiteRealtimeProviders[site],
		ScriptStore: h.SiteScriptStores[site], SecretStore: h.SiteSecretStores[site],
		WebhookWorker: h.SiteWebhookWorkers[site], Outbox: h.SiteOutboxes[site], Storage: h.SiteStorages[site],
	}
}

// linkedDocumentResolver binds declarative linked constraints to the current
// site's transaction. The doctype package receives only this narrow resolver,
// so generated constraints cannot obtain arbitrary database access.
func (h *Handler) linkedDocumentResolver(c *gin.Context) doctype.LinkedDocumentResolver {
	return func(target *doctype.DocType, name string) (*doctype.Document, error) {
		if target == nil || strings.TrimSpace(name) == "" {
			return nil, nil
		}
		return h.siteTx(c).GetDoc(target, name, "")
	}
}

// NewHandler creates a new API handler.
func NewHandler(registry *doctype.Registry, txManager *orm.TxManager) *Handler {
	return &Handler{
		Registry:      registry,
		TxManager:     txManager,
		AuthProviders: auth.NewProviderRegistry(),
		AgentStores:   map[string]*org.AgentStore{},
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
func (h *Handler) siteStorage(c *gin.Context) (storage.Backend, error) {
	if c != nil {
		if siteName := c.GetString("site_name"); siteName != "" {
			if b := h.runtimeService(c, siteName).Storage; b != nil {
				return b, nil
			}
		}
	}
	if h.Storage != nil {
		return h.Storage, nil
	}
	fallbackStorageOnce.Do(func() {
		fallbackStorage, fallbackStorageErr = storage.New(storage.Config{Backend: "local"})
	})
	return fallbackStorage, fallbackStorageErr
}

// siteAnalyticsWorker returns the analytics worker for the current request's site, or nil.
type analyticsWorker interface {
	InvalidateMetrics(string)
	InvalidateAllMetrics()
	Flush() error
}

func (h *Handler) siteAnalyticsWorker(c *gin.Context) analyticsWorker {
	if w, ok := c.Get("site_analytics_worker"); ok {
		if worker, ok := w.(analyticsWorker); ok {
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
	execution, verified := requestExecution(c)
	user, userRole := "", ""
	var userRoles []string
	if verified {
		actor := execution.Actor()
		user, userRoles = actor.SubjectUserID, append([]string(nil), actor.Roles...)
		if user == "" {
			user = actor.PrincipalID
		}
		if len(userRoles) > 0 {
			userRole = userRoles[0]
		}
	}
	if db != nil && reg != nil {
		if sqlDB, ok := db.(*sql.DB); ok {
			if r, ok := reg.(*doctype.Registry); ok {
				tm := &orm.TxManager{DB: sqlDB, Registry: r, Dialect: h.siteDialect(c)}
				tm.Context = c.Request.Context()
				if verified {
					// Durable after-hooks must be captured with the verified tenant
					// identity and the exact configuration revision pinned by the
					// site router for this request.
					tm.AsyncHookTenantID = execution.Tenant()
					tm.AsyncHookConfigRevision = c.GetUint64("site_config_revision")
				}
				if createdAt, ok := c.Get("session_created_at"); ok {
					if ts, ok := createdAt.(time.Time); ok && !ts.IsZero() {
						tm.Context = context.WithValue(tm.Context, "session_created_at", ts)
					}
				}
				if siteNameStr, ok := siteName.(string); ok {
					tm.SiteName = siteNameStr
				}
				// Wire the transactional outbox writer for this site.
				if siteNameStr, ok := siteName.(string); ok {
					services := h.runtimeService(c, siteNameStr)
					tm.Outbox = services.Outbox
					tm.WorkflowStatusPolicies = services.WorkflowStatusPolicies
				}
				// Wire script runner and store.
				tm.ScriptRunner = h.ScriptRunner
				if siteNameStr, ok := siteName.(string); ok {
					services := h.runtimeService(c, siteNameStr)
					tm.ScriptStore = services.ScriptStore
					tm.AsyncHookInvocations = services.AsyncHookInvocations
				}
				// Wire async hook queue.
				tm.AsyncHookSink = h.AsyncHookSink

				// Create script provider for this request (bridges JS → engine).
				if h.ScriptRunner != nil {
					var ss *secret.Store
					if siteNameStr, ok := siteName.(string); ok {
						ss = h.runtimeService(c, siteNameStr).SecretStore
					}
					if siteNameStr, ok := siteName.(string); ok {
						tm.ScriptProviderFactory = ScriptProviderFactory(r, siteNameStr, ss, h.ScriptHTTPAllowlist)
						tm.ScriptProvider = tm.ScriptProviderFactory(tm)
					}
				}
				// Set current user context for scripts.
				tm.CurrentUser = user
				tm.CurrentUserRole = userRole
				tm.CurrentUserRoles = append([]string(nil), userRoles...)
				tm.Execution = execution
				return tm
			}
		}
	}
	return h.TxManager
}

// siteDialect selects the dialect belonging to the runtime bound to this
// request. The handler-level dialect remains a fallback for tests and
// single-site deployments without SiteRouter context.
func (h *Handler) siteDialect(c *gin.Context) kdb.Dialect {
	var fallback kdb.Dialect
	if h.TxManager != nil {
		fallback = h.TxManager.Dialect
	}
	return siteDialectFromContext(c, fallback)
}

// siteQuery converts dialect-neutral placeholders for the current tenant DB.
// Keep raw API SQL portable until each call site is moved onto the query builder.
func (h *Handler) siteQuery(c *gin.Context, query string) string {
	return kdb.Rebind(h.siteDialect(c), query)
}

// siteDatabaseName is only needed by MySQL schema-inspection APIs. PostgreSQL
// and LibSQL introspect the connected database directly and ignore this arg.
func (h *Handler) siteDatabaseName(c *gin.Context, database *sql.DB) string {
	if h.siteDialect(c).DriverName() != "mysql" || database == nil {
		return ""
	}
	var name string
	_ = database.QueryRow("SELECT DATABASE()").Scan(&name)
	return name
}

func siteDialectFromContext(c *gin.Context, fallback kdb.Dialect) kdb.Dialect {
	if dbType := strings.TrimSpace(c.GetString("site_db_type")); dbType != "" {
		return kdb.Resolve(dbType)
	}
	if fallback != nil {
		return fallback
	}
	return kdb.Resolve("")
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
	var referenceErr *doctype.ReferenceValidationError
	if errors.As(err, &referenceErr) {
		writeError(c, http.StatusBadRequest, "configuration.references_invalid", "Configuration contains unresolved references", map[string]any{"findings": referenceErr.Findings})
		return
	}
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
func checkPerm(c *gin.Context, registry *doctype.Registry, docType, operation string) (policy.RowScope, bool) {
	// Resource routes accept both the canonical DocType name ("Lead") and its
	// URL-safe alias ("lead"). The permission matrix is keyed by the canonical
	// name, so resolve aliases before asking the policy engine. Without this,
	// non-admin users can be denied on ordinary CRUD routes despite having a
	// matching tenant permission.
	if registry != nil {
		if resolved := registry.Get(docType); resolved != nil {
			docType = resolved.Name
		}
	}
	// Authorization consumes only the verified execution installed by the
	// authentication adapter. Roleless sessions do not receive an implicit
	// bootstrap/admin grant.
	execution, valid := requestExecution(c)
	if !valid {
		c.JSON(http.StatusForbidden, ErrorResponse{Error: map[string]string{"message": "verified execution is required"}})
		return policy.RowScope{}, true
	}
	dt := registry.Get(docType)
	businessScope := contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindTenant}
	if entityID := strings.TrimSpace(c.GetHeader("X-Kora-Legal-Entity-ID")); entityID != "" {
		businessScope.Kind = contract.ScopeKindLegalEntity
		businessScope.LegalEntityID = entityID
	}
	if err := businessScope.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "invalid legal entity scope"}})
		return policy.RowScope{}, true
	}

	authType := c.GetString("auth_type")
	if authType == "extension" || authType == "channel_session" {
		decision, decideErr := policy.DecideForBusinessScope(execution, registry, docType, operation, businessScope, dt != nil && dt.BusinessScopePolicy == contract.ScopeEntityOwned)
		if decideErr != nil {
			c.JSON(http.StatusForbidden, ErrorResponse{
				Error: map[string]string{
					"message": fmt.Sprintf("Token does not have %s permission on %s", operation, docType),
				},
			})
			return policy.RowScope{}, true
		}
		return requestRowScope(c, decision, businessScope), false
	}
	decision, decideErr := policy.DecideForBusinessScope(execution, registry, docType, operation, businessScope, dt != nil && dt.BusinessScopePolicy == contract.ScopeEntityOwned)
	if decideErr != nil {
		c.JSON(http.StatusForbidden, ErrorResponse{
			Error: map[string]string{
				"message": fmt.Sprintf("Permission denied: cannot %s on %s", operation, docType),
			},
		})
		return policy.RowScope{}, true
	}
	return requestRowScope(c, decision, businessScope), false
}

func requestRowScope(c *gin.Context, decision policy.Decision, business contract.BusinessScope) policy.RowScope {
	if !decision.OwnerOnly && len(decision.AssignedFields) == 0 {
		return policy.RowScope{Business: business}
	}
	user, _ := requestUserID(c)
	return policy.RowScope{UserID: user, Owner: decision.OwnerOnly, AssignedFields: append([]string(nil), decision.AssignedFields...), Business: business}
}

func businessScopeFromRequest(c *gin.Context, execution policy.Execution) contract.BusinessScope {
	scope := contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindTenant}
	if c != nil {
		if entityID := strings.TrimSpace(c.GetHeader("X-Kora-Legal-Entity-ID")); entityID != "" {
			scope.Kind = contract.ScopeKindLegalEntity
			scope.LegalEntityID = entityID
		}
	}
	return scope
}

// HandleList handles GET /api/resource/{doctype}
func (h *Handler) HandleList(c *gin.Context) {
	doctypeName := c.Param("doctype")
	registry := h.siteRegistry(c)
	dt := registry.Get(doctypeName)
	if dt == nil {
		notFoundError(c, "resource.doctype_not_found", fmt.Sprintf("DocType %q not found", doctypeName), map[string]any{"doctype": doctypeName})
		return
	}

	// Check read permission.
	scope, forbidden := checkPerm(c, registry, doctypeName, "read")
	if forbidden {
		return
	}

	// Parse query parameters.
	limitRaw := c.DefaultQuery("limit", strconv.Itoa(APIDefaultLimit))
	limit, limitErr := strconv.Atoi(limitRaw)
	offsetRaw := c.DefaultQuery("offset", "0")
	offset, offsetErr := strconv.Atoi(offsetRaw)
	if limitErr != nil || offsetErr != nil || limit < 1 || offset < 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "limit must be a positive integer and offset must be a non-negative integer"}})
		return
	}
	orderBy := c.Query("order_by")
	filters := c.Query("filters")
	search := c.Query("search")
	if utf8.RuneCountInString(search) > 128 {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "search must not exceed 128 characters"}})
		return
	}

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

	docs, total, err := h.siteTx(c).GetListWithScope(dt, filters, orderBy, limit, offset, scope, requestedFields, search)
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

	registry := h.siteRegistry(c)
	dt := registry.Get(doctypeName)
	if dt == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": fmt.Sprintf("DocType %q not found", doctypeName)},
		})
		return
	}

	// Check read permission.
	scope, forbidden := checkPerm(c, registry, doctypeName, "read")
	if forbidden {
		return
	}
	doc, err := h.siteTx(c).GetDocWithScope(dt, name, scope)
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

	var rawData map[string]any
	if err := c.ShouldBindJSON(&rawData); err != nil {
		slog.Warn("invalid JSON in create", "error", err)
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
		return
	}
	data, err := json.Marshal(rawData)
	if err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > 256 {
		badRequestError(c, "validation.invalid_idempotency_key", "Idempotency-Key must be at most 256 bytes", nil)
		return
	}
	h.executeResourceMutationWithKey(c, kernel.CommandRecordCreate, doctypeName, "", data, http.StatusCreated, idempotencyKey)
	if c.Writer.Status() != http.StatusCreated {
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}
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

	var rawData map[string]any
	if err := c.ShouldBindJSON(&rawData); err != nil {
		slog.Warn("invalid JSON in update", "error", err)
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
		return
	}
	// Reads expose this opaque token for optimistic writes. Keep it outside the
	// document payload so it cannot be mistaken for a user field.
	expectedVersion := ""
	if rawVersion, exists := rawData["_version"]; exists {
		version, ok := rawVersion.(string)
		if !ok || strings.TrimSpace(version) == "" {
			badRequestError(c, "validation.invalid_version", "Record version is invalid. Refresh the record and try again.", nil)
			return
		}
		expectedVersion = strings.TrimSpace(version)
		delete(rawData, "_version")
	}

	data, err := json.Marshal(rawData)
	if err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if len(idempotencyKey) > 256 {
		badRequestError(c, "validation.invalid_idempotency_key", "Idempotency-Key must be at most 256 bytes", nil)
		return
	}
	h.executeResourceMutationWithKey(c, kernel.CommandRecordUpdate, doctypeName, name, data, http.StatusOK, idempotencyKey, expectedVersion)
	if c.Writer.Status() != http.StatusOK {
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}
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

	h.executeResourceMutation(c, kernel.CommandRecordDelete, doctypeName, name, nil, http.StatusOK)
	if c.Writer.Status() != http.StatusOK {
		return
	}
	h.invalidateAnalyticsForDoctype(c, doctypeName)
	if w := h.siteAnalyticsWorker(c); w != nil {
		w.Flush()
	}
}

// --- Helpers ---

// docToMap converts a Document to a map for JSON serialization, including system fields.
func docToMap(doc *doctype.Document, dt *doctype.DocType, registry *doctype.Registry, requestedFields []string) map[string]any {
	result := make(map[string]any, len(dt.DataFields())+2)
	result["name"] = doc.Name
	result["doc_status"] = doc.DocStatus
	// Offline clients must echo this token to the kernel's expected_version
	// field. It is derived from the same value checked inside the write transaction.
	result["_version"] = kernel.CanonicalVersion(doc.Get("modified"))

	for _, f := range dt.DataFields() {
		if f.Hidden || f.Fieldtype == "Password" {
			continue
		}
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

// RegisterPublicRoutesOnGroup registers public delivery and provider callback
// routes. Each route must enforce its own access policy; CRUD authentication is
// registered separately.
func RegisterPublicRoutesOnGroup(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteStorages map[string]storage.Backend) {
	RegisterPublicRoutesOnGroupWithRuntimeServices(apiGroup, registry, txManager, siteStorages, nil)
}

func RegisterPublicRoutesOnGroupWithRuntimeServices(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteStorages map[string]storage.Backend, runtimeServices *SiteRuntimeServices) {
	handler := NewHandler(registry, txManager)
	handler.SiteStorages = siteStorages
	handler.RuntimeServices = runtimeServices
	apiGroup.Use(WorkAdmissionMiddleware(runtimeServices))
	public := apiGroup.Group("/public/resource")
	{
		public.GET("/:doctype", handler.HandlePublicList)
		public.GET("/:doctype/:name", handler.HandlePublicGet)
	}

	// Public view routes (unauthenticated, three-layer security check).
	apiGroup.GET("/v", handler.HandlePublicView)
	apiGroup.POST("/v", handler.HandlePublicCreate)
	apiGroup.GET("/system/branding", handler.HandleSystemBranding)
	apiGroup.GET("/public/files/*path", handler.HandlePublicFileServe)

	// M-Pesa callback mutations require the per-tenant token in the callback
	// path. Keep the tokenless path registered for the migration window; its
	// handler rejects unauthenticated callbacks.
	apiGroup.POST("/webhooks/mpesa/stk", handler.HandleMPesaSTKCallback)
	apiGroup.POST("/webhooks/mpesa/stk/:token", handler.HandleMPesaSTKCallback)
}

// RegisterRoutesOnGroupWithAnalytics registers all CRUD routes with optional
// analytics event propagation. siteBuses maps site name → EventBus; if nil or
// empty, analytics event emission is a no-op.
func RegisterRoutesOnGroupWithAnalytics(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteBuses map[string]eventstream.Bus, realtimeProviders map[string]*natsprovider.Provider, scriptRunner script.Runner, siteScriptStores map[string]*script.Store, siteSecretStores map[string]*secret.Store, httpAllowlist []string, siteWebhookWorkers map[string]*webhook.Worker, asyncHookSink orm.AsyncHookSink, siteOutboxes map[string]outbox.Writer, siteStorages map[string]storage.Backend, kernelCommands *kernel.CommandRegistry) {
	RegisterRoutesOnGroupWithRuntimeServices(apiGroup, registry, txManager, siteBuses, realtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookSink, siteOutboxes, siteStorages, kernelCommands, nil)
}

// RegisterRoutesOnGroupWithRuntimeServices is the compatibility registration
// form of RegisterRoutesOnGroupWithAnalytics. Engine production composition
// uses tenant middleware's pinned service snapshot and the explicit cache
// owner below; external callers may continue to pass this fallback registry.
func RegisterRoutesOnGroupWithRuntimeServices(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteBuses map[string]eventstream.Bus, realtimeProviders map[string]*natsprovider.Provider, scriptRunner script.Runner, siteScriptStores map[string]*script.Store, siteSecretStores map[string]*secret.Store, httpAllowlist []string, siteWebhookWorkers map[string]*webhook.Worker, asyncHookSink orm.AsyncHookSink, siteOutboxes map[string]outbox.Writer, siteStorages map[string]storage.Backend, kernelCommands *kernel.CommandRegistry, runtimeServices *SiteRuntimeServices) {
	RegisterRoutesOnGroupWithAnalyticsCache(apiGroup, registry, txManager, siteBuses, realtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookSink, siteOutboxes, siteStorages, kernelCommands, runtimeServices, nil)
}

// RegisterRoutesOnGroupWithAnalyticsCache is the Engine composition form that
// supplies process-level analytics cache ownership separately from tenant
// runtime services.
func RegisterRoutesOnGroupWithAnalyticsCache(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteBuses map[string]eventstream.Bus, realtimeProviders map[string]*natsprovider.Provider, scriptRunner script.Runner, siteScriptStores map[string]*script.Store, siteSecretStores map[string]*secret.Store, httpAllowlist []string, siteWebhookWorkers map[string]*webhook.Worker, asyncHookSink orm.AsyncHookSink, siteOutboxes map[string]outbox.Writer, siteStorages map[string]storage.Backend, kernelCommands *kernel.CommandRegistry, runtimeServices *SiteRuntimeServices, analyticsCache *AnalyticsQueryCache) {
	RegisterRoutesOnGroupWithRealtimeOwner(apiGroup, registry, txManager, siteBuses, realtimeProviders, scriptRunner, siteScriptStores, siteSecretStores, httpAllowlist, siteWebhookWorkers, asyncHookSink, siteOutboxes, siteStorages, kernelCommands, runtimeServices, analyticsCache, nil)
}

// RegisterRoutesOnGroupWithRealtimeOwner installs the production routes with
// an explicit owner for hijacked WebSocket connections. The Engine closes
// these before retiring tenant runtime generations.
func RegisterRoutesOnGroupWithRealtimeOwner(apiGroup *gin.RouterGroup, registry *doctype.Registry, txManager *orm.TxManager, siteBuses map[string]eventstream.Bus, realtimeProviders map[string]*natsprovider.Provider, scriptRunner script.Runner, siteScriptStores map[string]*script.Store, siteSecretStores map[string]*secret.Store, httpAllowlist []string, siteWebhookWorkers map[string]*webhook.Worker, asyncHookSink orm.AsyncHookSink, siteOutboxes map[string]outbox.Writer, siteStorages map[string]storage.Backend, kernelCommands *kernel.CommandRegistry, runtimeServices *SiteRuntimeServices, analyticsCache *AnalyticsQueryCache, realtimeConnections *RealtimeConnectionOwner) {
	handler := NewHandler(registry, txManager)
	handler.RealtimeConnections = realtimeConnections
	apiGroup.Use(WorkAdmissionMiddleware(runtimeServices))
	handler.RuntimeServices = runtimeServices
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
	RegisterConversationRoutes(apiGroup, handler)

	// File attachments: upload + authenticated serving (Range-aware for audio/video).
	apiGroup.POST("/upload", handler.HandleUpload)
	apiGroup.GET("/files/*path", handler.HandleFileServe)
	apiGroup.DELETE("/files/*path", handler.HandleFileDelete)

	resource := apiGroup.Group("/resource")
	{
		resource.GET("/:doctype", handler.HandleList)
		resource.POST("/:doctype", handler.HandleCreate)
		resource.GET("/:doctype/:name", handler.HandleGet)
		resource.GET("/:doctype/:name/history", handler.HandleRecordHistory)
		resource.PUT("/:doctype/:name", handler.HandleUpdate)
		resource.DELETE("/:doctype/:name", handler.HandleDelete)
	}
	apiGroup.POST("/modules/:module/queries/:query", handler.HandlePublishedModuleQuery)
	apiGroup.GET("/modules/:module/projections/:projection", handler.HandlePublishedModuleProjection)
	apiGroup.POST("/modules/:module/projections/:projection/rebuild", handler.HandleRebuildPublishedModuleProjection)

	// Operation kernel: canonical command path shared by every adapter
	// (first vertical slice — record.create, record.update) plus the
	// config-defined command registry (KERNEL-008).
	apiGroup.POST("/kernel/:command", handler.HandleKernelOperation)
	apiGroup.GET("/kernel/_registry", handler.HandleKernelRegistry)
	if apiGroup.BasePath() == "/api/v1" {
		apiGroup.GET("/offline/capabilities", handler.HandleOfflineCapabilities)
		apiGroup.POST("/offline/push", handler.HandleOfflinePush)
		apiGroup.POST("/offline/bootstrap", handler.HandleOfflineBootstrap)
		apiGroup.POST("/offline/pull", handler.HandleOfflinePull)
	}

	// OpenAPI docs.
	apiGroup.GET("/openapi.json", handler.HandleOpenAPI)
	apiGroup.GET("/swagger-ui", handler.HandleSwaggerUI)

	// AI operations are exposed only on the canonical, versioned API. The old
	// /api/chat and /api/v1/chat paths are retired; callers use /api/v1/ai/chat
	// and its run/approval endpoints together.
	if apiGroup.BasePath() == "/api/v1" {
		aiGroup := apiGroup.Group("/ai")
		{
			aiGroup.GET("/chat", handler.HandleAIChatConversation)
			aiGroup.POST("/chat", handler.HandleChat)
			aiGroup.GET("/approvals", handler.HandleAIListApprovals)
			aiGroup.POST("/approvals/:id/grant", handler.HandleAIGrantApproval)
			aiGroup.POST("/runs/:id/cancel", handler.HandleAICancel)
			aiGroup.POST("/runs/:id/resume", handler.HandleAIResume)
			aiGroup.POST("/retention/cleanup", handler.HandleAIRetentionCleanup)
		}
	}

	channel := apiGroup.Group("/internal/channel")
	{
		channel.POST("/sessions/issue", handler.HandleChannelSessionIssue)
		channel.POST("/managed-client/rotate", handler.HandleManagedChannelClientRotate)
		channel.POST("/managed-client/revoke", handler.HandleManagedChannelClientRevoke)
		channel.POST("/sessions/revoke", handler.HandleChannelSessionRevoke)
		channel.GET("/tools", handler.HandleChannelTools)
		channel.POST("/query", handler.HandleChannelQuery)
		channel.POST("/mutate", handler.HandleChannelMutate)
	}
	apiGroup.GET("/internal/site/identity", handler.HandleManagedSiteIdentity)

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
	apiGroup.POST("/view/action/:actionId", handler.HandleViewAction)

	// System config endpoints.
	system := apiGroup.Group("/system/config")
	agents := apiGroup.Group("/system/agents")
	agents.GET("/:id", handler.HandleAgentManifest)
	agents.PUT("/:id", handler.HandleAgentManifest)
	agents.GET("/:id/effective-permissions", handler.HandleAgentEffectivePermissions)
	agents.GET("/:id/runs", handler.HandleAgentRuns)
	agents.POST("/:id/runs", handler.HandleAgentRuns)
	apiGroup.GET("/system/permissions/catalog", handler.HandleAgentPermissionCatalog)
	{
		system.GET("/versions", handler.HandleConfigVersions)
		system.GET("/versions/head/snapshot", handler.HandleConfigHeadSnapshot)
		system.GET("/versions/active/snapshot", handler.HandleActiveConfigSnapshot)
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
	RegisterAnalyticsRoutesWithQueryCache(apiGroup, registry, txManager.DB, siteBuses, realtimeProviders, txManager.Dialect, runtimeServices, analyticsCache)
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
		h.siteQuery(c, "SELECT version, label, config, changelog FROM _kora_config_version WHERE id = ?"), id,
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
	// Compare full snapshots. change_list is an execution-oriented S-expression
	// change array used by activation and is not the review API's ConfigDiffFull.
	var fromConfig, toConfig string
	if err := h.siteTx(c).DB.QueryRow(h.siteQuery(c, "SELECT config FROM _kora_config_version WHERE id = ?"), fromID).Scan(&fromConfig); err != nil {
		writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
		return
	}
	if err := h.siteTx(c).DB.QueryRow(h.siteQuery(c, "SELECT config FROM _kora_config_version WHERE id = ?"), toID).Scan(&toConfig); err != nil {
		writeError(c, http.StatusNotFound, "version.not_found", "Version not found", nil)
		return
	}
	fromSnapshot, fromErr := doctype.ParseConfig(fromConfig)
	toSnapshot, toErr := doctype.ParseConfig(toConfig)
	if fromErr != nil || toErr != nil {
		writeError(c, http.StatusInternalServerError, "version.parse_failed", "Failed to compare version snapshots", nil)
		return
	}
	diff := doctype.DiffFullSnapshots(fromSnapshot, toSnapshot)
	c.JSON(http.StatusOK, Response{Data: map[string]any{
		"changes":         diff.Doctypes.Changes,
		"section_changes": diff.SectionChanges,
		"diff_summary":    diff.Summary(),
		"is_breaking":     diff.Doctypes.IsBreaking,
	}})
}

// HandleUpload handles file uploads via multipart form.
// POST /upload
// Stores files via the configured storage backend and returns a file reference
// (path + metadata) for storage in an Attach / Attach Image / Attach Audio field.
func (h *Handler) HandleUpload(c *gin.Context) {
	maxBytes := maxUploadBytes()
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.UploadAdmission != nil {
		const conservativeProcessingCopies = 3
		reservationBytes := uint64(maxBytes) * conservativeProcessingCopies
		release, err := services.UploadAdmission.Acquire(c.Request.Context(), reservationBytes)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				c.Abort()
				return
			}
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "upload.capacity_exhausted", "Upload processing capacity is temporarily exhausted", nil)
			return
		}
		defer release()
	}
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

	backend, err := h.siteStorage(c)
	if err != nil {
		internalError(c, "initializing file storage", err)
		return
	}

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
		UploadedBy: currentUser(c),
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
	ReleaseTenantStreamAuthWork(c)
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

	backend, err := h.siteStorage(c)
	if err != nil {
		internalError(c, "initializing file storage", err)
		return
	}
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
	backend, err := h.siteStorage(c)
	if err != nil {
		internalError(c, "initializing file storage", err)
		return
	}
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
			if mb > 1024 {
				mb = 1024
			}
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
	store := h.runtimeService(c, siteNameStr).ScriptStore
	if store == nil {
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

	// Execute the script.
	if h.ScriptRunner == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Script runner not available"},
		})
		return
	}

	// Build a provider for API method scripts (enables kora.getDoc, kora.http, etc.)
	tx := h.siteTx(c)
	userStr, roles, verified := scriptIdentity(tx.Execution, siteNameStr)
	if !verified {
		c.JSON(http.StatusUnauthorized, ErrorResponse{Error: map[string]string{"code": "auth.unauthenticated", "message": "Verified script execution identity required"}})
		return
	}
	var ss *secret.Store
	ss = h.runtimeService(c, siteNameStr).SecretStore
	provider := NewScriptProvider(tx, h.siteRegistry(c), siteNameStr, ss, h.ScriptHTTPAllowlist)

	execReq := script.ExecuteRequest{
		Script:     rec.Script,
		ScriptType: script.TypeAPIMethod,
		ScriptName: rec.Name,
		DocType:    rec.DocType,
		Document:   reqBody,
		User:       userStr,
		UserRoles:  roles,
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
