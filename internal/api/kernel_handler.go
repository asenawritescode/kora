package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

// kernelRequest is the wire shape for POST /api/v1/kernel/:command.
type kernelRequest struct {
	IdempotencyKey  string `json:"idempotency_key"`
	CorrelationID   string `json:"correlation_id"`
	CausationID     string `json:"causation_id"`
	ExpectedVersion string `json:"expected_version"`
	// Internal adapter-only concurrency fence; never accepted from the public JSON body.
	ExpectedConfigRevision uint64          `json:"-"`
	Payload                json.RawMessage `json:"payload"`
}

// sourceForAuthType maps the gin auth context onto a kernel Source. The
// kernel treats every source identically; the mapping is audit metadata.
func sourceForAuthType(authType string) kernel.Source {
	switch authType {
	case "service":
		return kernel.SourceIntegration
	case "extension":
		return kernel.SourceIntegration
	case "channel_session":
		return kernel.SourceUI
	case "provider_callback":
		return kernel.SourceIntegration
	default:
		return kernel.SourceHTTP
	}
}

// siteOutboxWriter resolves the current site's outbox writer from request
// context, falling back to the site map on the handler.
func (h *Handler) siteOutboxWriter(c *gin.Context) outbox.Writer {
	siteName, _ := c.Get("site_name")
	if s, ok := siteName.(string); ok {
		if w := h.runtimeService(c, s).Outbox; w != nil {
			return w
		}
	}
	return nil
}

// HandleKernelRegistry lists config-defined command resources available on
// this runtime (KERNEL-008 introspection): name, version, permission
// operation, input record, and touched records with their required
// operations. MCP/AI/SDK catalogs derive from this surface.
func (h *Handler) HandleKernelRegistry(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	site, _ := siteName.(string)
	commands := h.kernelCommands(h.runtimeService(c, site))
	if commands == nil {
		commands = kernel.NewCommandRegistry()
	}
	type entry struct {
		Name       string   `json:"name"`
		Version    int      `json:"version"`
		Permission string   `json:"permission"`
		Input      string   `json:"input_record"`
		Touched    []string `json:"touched_records"`
	}
	out := []entry{}
	for _, def := range commands.List() {
		touched := def.TouchedRecords()
		if touched == nil {
			touched = []string{}
		}
		out = append(out, entry{
			Name:       def.FullName(),
			Version:    def.Version,
			Permission: def.PermOperation(),
			Input:      def.Input.Record,
			Touched:    touched,
		})
	}
	c.JSON(http.StatusOK, gin.H{"site": site, "commands": out})
}

// HandleKernelOperation routes a canonical command through the operation
// kernel (first vertical slice: record.create, record.update). All sources
// share this path; there is no per-adapter mutation logic here.
func (h *Handler) HandleKernelOperation(c *gin.Context) {
	var req kernelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "validation.invalid_json", "Invalid request format", nil)
		return
	}
	commandName := c.Param("command")
	result, cerr := h.executeKernelOperation(c, commandName, req)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	if result.Replayed {
		c.Header("X-Kora-Replay", "true")
	}
	c.JSON(http.StatusOK, Response{Data: result.Data, Meta: &Meta{DocType: commandName}})
}

func (h *Handler) executeKernelOperation(c *gin.Context, commandName string, req kernelRequest) (contract.CommandResult, *contract.Error) {
	return h.executeKernelOperationWithReadOnlyFields(c, commandName, req, false)
}

// executeKernelOperationWithReadOnlyFields is reserved for trusted internal
// adapters whose domain contract must update UI-read-only fields. The public
// kernel endpoint never derives this privilege from request data.
func (h *Handler) executeKernelOperationWithReadOnlyFields(c *gin.Context, commandName string, req kernelRequest, allowReadOnlyFields bool) (contract.CommandResult, *contract.Error) {
	k, op, sqlDB, reg, cerr := h.buildKernelOperation(c, commandName, req, allowReadOnlyFields)
	if cerr != nil {
		return contract.CommandResult{}, cerr
	}
	return k.Execute(c.Request.Context(), sqlDB, reg, op)
}

// buildKernelOperation resolves identity, tenant services and the base
// Operation shared by single-command and bulk execution.
func (h *Handler) buildKernelOperation(c *gin.Context, commandName string, req kernelRequest, allowReadOnlyFields bool) (*kernel.Kernel, kernel.Operation, *sql.DB, *doctype.Registry, *contract.Error) {
	fail := func(e *contract.Error) (*kernel.Kernel, kernel.Operation, *sql.DB, *doctype.Registry, *contract.Error) {
		return nil, kernel.Operation{}, nil, nil, e
	}
	site := c.GetString("site_name")
	if site == "" {
		return fail(contract.NewError(contract.CodeValidationFailed, "no tenant context"))
	}
	dbVal, _ := c.Get("site_db")
	sqlDB, ok := dbVal.(*sql.DB)
	if !ok || sqlDB == nil {
		return fail(contract.NewError(contract.CodeDependencyUnavailable, "site database unavailable"))
	}
	reg := h.siteRegistry(c)
	if commands := h.kernelCommands(h.runtimeService(c, site)); commands != nil {
		for _, ownerCommand := range commands.OwnerCommands() {
			if ownerCommand.FullName() == commandName {
				return fail(contract.NewError(contract.CodePermissionDenied, "owner module commands must be invoked through their published capability"))
			}
		}
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = c.GetHeader("Idempotency-Key")
	}
	authType := c.GetString("auth_type")
	execution, verified := requestExecution(c)
	if commandName == kernel.CommandPublicFormSubmit {
		tenantID := c.GetString("site_id")
		if tenantID == "" {
			tenantID = site
		}
		publicExecution, err := policy.NewVerifiedExecution(contract.TenantID(tenantID), contract.ActorContext{
			PrincipalID: "public-form", PrincipalType: contract.PrincipalPublic, Site: site, AuthenticatedAt: time.Now().UTC(),
		})
		if err != nil {
			return fail(contract.NewError(contract.CodeUnauthenticated, "public form execution identity is invalid"))
		}
		execution, verified = publicExecution, true
	}
	if !verified {
		return fail(contract.NewError(contract.CodeUnauthenticated, "verified execution identity is required"))
	}
	actor := execution.Actor()
	user := actor.SubjectUserID
	if user == "" {
		user = actor.PrincipalID
	}

	// Delegated credentials carry DocType grants instead of human roles. Check
	// those grants before entering the shared kernel so its bootstrap fallback
	// can never widen an extension or agent token.
	if authType == "extension" || authType == "channel_session" {
		var target struct {
			DocType string `json:"doctype"`
		}
		if err := json.Unmarshal(req.Payload, &target); err != nil || target.DocType == "" {
			return fail(contract.NewError(contract.CodeValidationFailed, "payload.doctype is required"))
		}
		dt := reg.Get(target.DocType)
		if dt == nil {
			return fail(contract.NewError(contract.CodeNotFound, "DocType not found: "+target.DocType))
		}
		operation := ""
		switch commandName {
		case kernel.CommandRecordCreate:
			operation = "create"
		case kernel.CommandRecordUpdate:
			operation = "write"
		case kernel.CommandRecordDelete:
			operation = "delete"
		case kernel.CommandWorkflowStatusTransition:
			operation = "submit"
		}
		if operation == "" {
			return fail(contract.NewError(contract.CodePermissionDenied, "delegated token cannot execute this command"))
		}
		if _, forbidden := checkPerm(c, reg, dt.Name, operation); forbidden {
			return fail(contract.NewError(contract.CodePermissionDenied, "not permitted"))
		}
	}
	opCtx := kernel.OperationContext{
		Site:                   site,
		TenantID:               execution.Tenant(),
		BusinessScope:          businessScopeFromRequest(c, execution),
		Execution:              execution,
		User:                   user,
		Roles:                  actor.Roles,
		Source:                 sourceForAuthType(authType),
		CorrelationID:          req.CorrelationID,
		CausationID:            req.CausationID,
		ExpectedVersion:        req.ExpectedVersion,
		ExpectedConfigRevision: req.ExpectedConfigRevision,
		ConfigRevision:         c.GetUint64("site_config_revision"),
		IdempotencyKey:         req.IdempotencyKey,
		AllowReadOnlyFields:    allowReadOnlyFields,
	}
	services := h.runtimeService(c, site)
	if len(actor.Roles) > 0 {
		opCtx.UserRole = actor.Roles[0]
	}
	opCtx.Actor = actor

	txManager := h.siteTx(c)
	opCtx.BusinessReferenceValidator = txManager.BusinessReferenceValidator
	txManager.CurrentUserRole = opCtx.UserRole
	k := kernel.New(h.siteDialect(c), h.siteOutboxWriter(c))
	k.TxManager = txManager
	k.Commands = h.kernelCommands(services)
	k.CommandSchemas = services.CommandSchemas
	return k, kernel.Operation{
		Context:  opCtx,
		Command:  commandName,
		Payload:  req.Payload,
		Deadline: time.Now().Add(30 * time.Second),
	}, sqlDB, reg, nil
}

func (h *Handler) executeResourceMutation(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, status int) {
	h.executeResourceMutationWithKey(c, commandName, doctypeName, name, data, status, "")
}

func (h *Handler) executeResourceMutationWithKey(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, status int, idempotencyKey string, expectedVersions ...string) {
	doc, cerr := h.runKernelResourceMutationWithKey(c, commandName, doctypeName, name, data, idempotencyKey, expectedVersions...)
	if cerr != nil {
		h.writeKernelError(c, cerr)
		return
	}
	if commandName == kernel.CommandRecordDelete {
		c.JSON(http.StatusOK, Response{Data: map[string]string{"message": "deleted"}, Meta: &Meta{DocType: doctypeName}})
		return
	}
	dt := h.siteRegistry(c).Get(doctypeName)
	c.JSON(status, Response{Data: docToMap(doc, dt, h.siteRegistry(c), nil), Meta: &Meta{DocType: doctypeName}})
}

func (h *Handler) runKernelResourceMutation(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage) (*doctype.Document, *contract.Error) {
	return h.runKernelResourceMutationWithKey(c, commandName, doctypeName, name, data, "")
}

func (h *Handler) runKernelResourceMutationWithKey(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, idempotencyKey string, expectedVersions ...string) (*doctype.Document, *contract.Error) {
	return h.runKernelResourceMutationWithOptions(c, commandName, doctypeName, name, data, idempotencyKey, false, expectedVersions...)
}

func (h *Handler) runKernelResourceMutationWithOptions(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, idempotencyKey string, allowReadOnlyFields bool, expectedVersions ...string) (*doctype.Document, *contract.Error) {
	payload := map[string]any{"doctype": doctypeName}
	if name != "" {
		payload["name"] = name
	}
	if data != nil {
		payload["data"] = data
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, contract.NewError(contract.CodeInternal, "encoding resource mutation failed")
	}
	expectedVersion := ""
	if len(expectedVersions) > 0 {
		expectedVersion = expectedVersions[0]
	}
	result, cerr := h.executeKernelOperationWithReadOnlyFields(c, commandName, kernelRequest{Payload: rawPayload, IdempotencyKey: idempotencyKey, ExpectedVersion: expectedVersion}, allowReadOnlyFields)
	if cerr != nil {
		return nil, cerr
	}
	if result.Replayed {
		c.Header("X-Kora-Replay", "true")
	}

	if commandName == kernel.CommandRecordDelete {
		return nil, nil
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return nil, contract.NewError(contract.CodeInternal, "decoding kernel result failed")
	}
	doc := orm.DocumentFromMap(h.siteRegistry(c), doctypeName, operation.Document)
	if doc == nil {
		return nil, contract.NewError(contract.CodeInternal, "kernel did not return the resource document")
	}
	// DocumentFromMap omits system fields used by business forms. Preserve the
	// kernel's persisted modified timestamp here so REST responses can issue a
	// version token that the next write will accept.
	if modified, ok := operation.Document["modified"]; ok {
		doc.Set("modified", modified)
	}
	return doc, nil
}

func (h *Handler) runKernelMutationBundle(c *gin.Context, payload kernel.RecordMutationBundlePayload, idempotencyKey string) (*kernel.ResultData, bool, *contract.Error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, false, contract.NewError(contract.CodeInternal, "encoding record bundle failed")
	}
	result, cerr := h.executeKernelOperation(c, kernel.CommandRecordMutateBundle, kernelRequest{
		Payload: rawPayload, IdempotencyKey: idempotencyKey,
	})
	if cerr != nil {
		return nil, false, cerr
	}
	if result.Replayed {
		c.Header("X-Kora-Replay", "true")
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return nil, false, contract.NewError(contract.CodeInternal, "decoding record bundle result failed")
	}
	return &operation, result.Replayed, nil
}

func (h *Handler) writeKernelError(c *gin.Context, cerr *contract.Error) {
	status := http.StatusInternalServerError
	switch cerr.Type {
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodeValidationFailed:
		status = http.StatusBadRequest
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeStaleSubject, contract.CodeLeaseLost, contract.CodeOutcomeUnknown, contract.CodeUnsupportedVersion:
		status = http.StatusConflict
	case contract.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	case contract.CodeDependencyUnavailable:
		status = http.StatusServiceUnavailable
	case contract.CodeDeadlineExceeded:
		status = http.StatusGatewayTimeout
	}
	var details map[string]any
	if len(cerr.Details) > 0 {
		var value any
		if err := json.Unmarshal(cerr.Details, &value); err == nil {
			switch typed := value.(type) {
			case map[string]any:
				details = typed
			case []any:
				// ValidationErrors are serialized as an array. Keep the standard
				// error envelope object-shaped while retaining those field details.
				details = map[string]any{"fields": typed}
			default:
				details = map[string]any{"value": typed}
			}
		}
	}
	writeError(c, status, "kernel."+string(cerr.Type), cerr.Message, details)
}

// kernelCommands prefers the tenant generation's registry, which includes
// commands from the tenant's active configuration, over the process default.
func (h *Handler) kernelCommands(services SiteRuntimeService) *kernel.CommandRegistry {
	if services.KernelCommands != nil {
		return services.KernelCommands
	}
	return h.KernelCommands
}
