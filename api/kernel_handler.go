package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	"github.com/gin-gonic/gin"
)

// kernelRequest is the wire shape for POST /api/v1/kernel/:command.
type kernelRequest struct {
	IdempotencyKey  string          `json:"idempotency_key"`
	CorrelationID   string          `json:"correlation_id"`
	CausationID     string          `json:"causation_id"`
	ExpectedVersion string          `json:"expected_version"`
	Payload         json.RawMessage `json:"payload"`
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
	default:
		return kernel.SourceHTTP
	}
}

// siteOutboxWriter resolves the current site's outbox writer from request
// context, falling back to the site map on the handler.
func (h *Handler) siteOutboxWriter(c *gin.Context) outbox.Writer {
	siteName, _ := c.Get("site_name")
	if s, ok := siteName.(string); ok && (h.SiteOutboxes != nil || h.RuntimeServices != nil) {
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
	commands := h.KernelCommands
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
	c.JSON(http.StatusOK, Response{Data: json.RawMessage(result.Data), Meta: &Meta{DocType: commandName}})
}

func (h *Handler) executeKernelOperation(c *gin.Context, commandName string, req kernelRequest) (contract.CommandResult, *contract.Error) {
	return h.executeKernelOperationWithReadOnlyFields(c, commandName, req, false)
}

// executeKernelOperationWithReadOnlyFields is reserved for trusted internal
// adapters whose domain contract must update UI-read-only fields. The public
// kernel endpoint never derives this privilege from request data.
func (h *Handler) executeKernelOperationWithReadOnlyFields(c *gin.Context, commandName string, req kernelRequest, allowReadOnlyFields bool) (contract.CommandResult, *contract.Error) {
	site := c.GetString("site_name")
	if site == "" {
		return contract.CommandResult{}, contract.NewError(contract.CodeValidationFailed, "no tenant context")
	}
	dbVal, _ := c.Get("site_db")
	sqlDB, ok := dbVal.(*sql.DB)
	if !ok || sqlDB == nil {
		return contract.CommandResult{}, contract.NewError(contract.CodeDependencyUnavailable, "site database unavailable")
	}
	reg := h.siteRegistry(c)
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = c.GetHeader("Idempotency-Key")
	}
	authType := c.GetString("auth_type")
	user := c.GetString("user")
	if commandName == kernel.CommandPublicFormSubmit {
		user = "public"
	} else if authType == "extension" {
		user = "extension:" + c.GetString("extension_name")
	} else if authType == "channel_session" {
		user = "channel:" + c.GetString("channel_sender_address")
	}
	if user == "" {
		user = "system"
	}

	// Delegated credentials carry DocType grants instead of human roles. Check
	// those grants before entering the shared kernel so its bootstrap fallback
	// can never widen an extension or agent token.
	if authType == "extension" || authType == "channel_session" {
		var target struct {
			DocType string `json:"doctype"`
		}
		if err := json.Unmarshal(req.Payload, &target); err != nil || target.DocType == "" {
			return contract.CommandResult{}, contract.NewError(contract.CodeValidationFailed, "payload.doctype is required")
		}
		dt := reg.Get(target.DocType)
		if dt == nil {
			return contract.CommandResult{}, contract.NewError(contract.CodeNotFound, "DocType not found: "+target.DocType)
		}
		operation := ""
		switch commandName {
		case kernel.CommandRecordCreate:
			operation = "create"
		case kernel.CommandRecordUpdate:
			operation = "write"
		case kernel.CommandRecordDelete:
			operation = "delete"
		case kernel.CommandRecordWorkflowTransition:
			operation = "submit"
		}
		if operation == "" {
			return contract.CommandResult{}, contract.NewError(contract.CodePermissionDenied, "delegated token cannot execute this command")
		}
		if _, forbidden := checkPerm(c, reg, dt.Name, operation); forbidden {
			return contract.CommandResult{}, contract.NewError(contract.CodePermissionDenied, "not permitted")
		}
	}
	opCtx := kernel.OperationContext{
		Site:                site,
		User:                user,
		Roles:               c.GetStringSlice("user_roles"),
		UserRole:            c.GetString("user_role"),
		Source:              sourceForAuthType(authType),
		CorrelationID:       req.CorrelationID,
		CausationID:         req.CausationID,
		ExpectedVersion:     req.ExpectedVersion,
		IdempotencyKey:      req.IdempotencyKey,
		AllowReadOnlyFields: allowReadOnlyFields,
	}
	principalType := contract.PrincipalHuman
	if commandName == kernel.CommandPublicFormSubmit {
		principalType = contract.PrincipalPublic
	} else if authType == "service" {
		principalType = contract.PrincipalService
	} else if authType == "extension" {
		principalType = contract.PrincipalService
	} else if authType == "channel_session" {
		principalType = contract.PrincipalAgent
	}
	opCtx.Actor = contract.ActorContext{
		PrincipalID:     user,
		PrincipalType:   principalType,
		SubjectUserID:   user,
		Site:            site,
		Roles:           opCtx.Roles,
		AuthenticatedAt: time.Now(),
	}

	txManager := h.siteTx(c)
	txManager.CurrentUserRole = c.GetString("user_role")
	k := kernel.New(h.siteDialect(c), h.siteOutboxWriter(c))
	k.TxManager = txManager
	k.Commands = h.KernelCommands
	result, cerr := k.Execute(c.Request.Context(), sqlDB, reg, kernel.Operation{
		Context:  opCtx,
		Command:  commandName,
		Payload:  req.Payload,
		Deadline: time.Now().Add(30 * time.Second),
	})
	return result, cerr
}

func (h *Handler) executeResourceMutation(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, status int) {
	doc, cerr := h.runKernelResourceMutation(c, commandName, doctypeName, name, data)
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

func (h *Handler) runKernelResourceMutationWithKey(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, idempotencyKey string) (*doctype.Document, *contract.Error) {
	return h.runKernelResourceMutationWithOptions(c, commandName, doctypeName, name, data, idempotencyKey, false)
}

func (h *Handler) runKernelTrustedReadOnlyResourceMutationWithKey(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, idempotencyKey string) (*doctype.Document, *contract.Error) {
	return h.runKernelResourceMutationWithOptions(c, commandName, doctypeName, name, data, idempotencyKey, true)
}

func (h *Handler) runKernelResourceMutationWithOptions(c *gin.Context, commandName, doctypeName, name string, data json.RawMessage, idempotencyKey string, allowReadOnlyFields bool) (*doctype.Document, *contract.Error) {
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
	result, cerr := h.executeKernelOperationWithReadOnlyFields(c, commandName, kernelRequest{Payload: rawPayload, IdempotencyKey: idempotencyKey}, allowReadOnlyFields)
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

func (h *Handler) runKernelWorkflowTransition(c *gin.Context, doctypeName, name, action string, expectedRevision uint64) (*doctype.Document, *contract.Error) {
	payload, err := json.Marshal(kernel.RecordWorkflowTransitionPayload{
		Doctype: doctypeName, Name: name, Action: action, ExpectedRevision: expectedRevision,
	})
	if err != nil {
		return nil, contract.NewError(contract.CodeInternal, "encoding workflow transition failed")
	}
	result, cerr := h.executeKernelOperation(c, kernel.CommandRecordWorkflowTransition, kernelRequest{Payload: payload})
	if cerr != nil {
		return nil, cerr
	}
	if result.Replayed {
		c.Header("X-Kora-Replay", "true")
	}
	var operation kernel.ResultData
	if err := json.Unmarshal(result.Data, &operation); err != nil {
		return nil, contract.NewError(contract.CodeInternal, "decoding workflow result failed")
	}
	doc := orm.DocumentFromMap(h.siteRegistry(c), doctypeName, operation.Document)
	if doc == nil {
		return nil, contract.NewError(contract.CodeInternal, "kernel did not return the workflow document")
	}
	return doc, nil
}

func (h *Handler) writeWorkflowKernelError(c *gin.Context, cerr *contract.Error) {
	status := http.StatusBadRequest
	switch cerr.Type {
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused:
		status = http.StatusConflict
	case contract.CodeInternal, contract.CodeDependencyUnavailable:
		status = http.StatusInternalServerError
	}
	c.JSON(status, ErrorResponse{Error: map[string]string{"message": cerr.Message}})
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
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused:
		status = http.StatusConflict
	case contract.CodeDeadlineExceeded:
		status = http.StatusGatewayTimeout
	}
	var details map[string]any
	if len(cerr.Details) > 0 {
		_ = json.Unmarshal(cerr.Details, &details)
	}
	writeError(c, status, "kernel."+string(cerr.Type), cerr.Message, details)
}
