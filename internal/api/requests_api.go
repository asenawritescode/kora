package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/requests"
	"github.com/gin-gonic/gin"
)

const requestBodyLimit = 96 << 10

// RegisterRequestRoutes exposes the generated v2 request contract. Callers
// must attach the normal tenant authentication, CSRF, routing, and admission
// middleware to the group before registering these routes.
func RegisterRequestRoutes(group *gin.RouterGroup, services *SiteRuntimeServices) {
	h := &requestHandler{services: services}
	group.GET("/requests", h.list)
	group.POST("/requests", h.submit)
	group.GET("/requests/:id", h.get)
	group.POST("/requests/:id/responses", h.respond)
	group.POST("/requests/:id/reassignments", h.reassign)
	group.POST("/requests/:id/cancel", h.cancel)
	group.GET("/requests/:id/history", h.history)
	group.GET("/requests/:id/responses/:decision", h.response)
	group.POST("/requests/:id/read", h.markRead)
}

type requestHandler struct{ services *SiteRuntimeServices }

func (h *requestHandler) service(c *gin.Context) (*requests.Service, policy.Execution, bool) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, string(contract.CodeUnauthenticated), "A verified tenant session is required.", nil)
		return nil, policy.Execution{}, false
	}
	services := RuntimeServiceForContext(c, h.services, c.GetString("site_name"))
	if services.Requests == nil {
		writeError(c, http.StatusServiceUnavailable, string(contract.CodeDependencyUnavailable), "Request service is unavailable for this tenant.", nil)
		return nil, policy.Execution{}, false
	}
	return services.Requests, execution, true
}

func (h *requestHandler) submit(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	var command contract.SubmitRequest
	if !decodeRequestBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	result, err := service.Submit(c.Request.Context(), execution, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: result})
}

func (h *requestHandler) get(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	result, err := service.Get(c.Request.Context(), execution, id)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) list(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	limit := uint64(25)
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			badRequestError(c, string(contract.CodeValidationFailed), "limit must be an integer from 1 to 100", nil)
			return
		}
		limit = parsed
	}
	if limit == 0 || limit > 100 {
		badRequestError(c, string(contract.CodeValidationFailed), "limit must be an integer from 1 to 100", nil)
		return
	}
	filter := contract.RequestFilter{Page: contract.PageRequest{Limit: uint16(limit), Cursor: contract.PageCursor(c.Query("cursor"))}, View: contract.InboxView(c.DefaultQuery("view", string(contract.InboxAction)))}
	if raw := c.Query("kind"); raw != "" {
		kind := contract.RequestKind(raw)
		filter.Kind = &kind
	}
	if raw := c.Query("overdue_only"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			badRequestError(c, string(contract.CodeValidationFailed), "overdue_only must be a boolean", nil)
			return
		}
		filter.OverdueOnly = value
	}
	result, err := service.List(c.Request.Context(), execution, filter)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) respond(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	var command contract.RespondRequest
	if !decodeRequestBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	request, err := service.Get(c.Request.Context(), execution, id)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	var result contract.ResponseReceipt
	if request.Kind == contract.RequestApproval {
		runtime := RuntimeServiceForContext(c, h.services, c.GetString("site_name"))
		if runtime.ApprovalApplier == nil {
			writeError(c, http.StatusServiceUnavailable, string(contract.CodeDependencyUnavailable), "Governed approval application is unavailable for this tenant.", nil)
			return
		}
		result, err = runtime.ApprovalApplier.ApproveAndApply(c.Request.Context(), execution, id, command)
	} else {
		result, err = service.Respond(c.Request.Context(), execution, id, command)
	}
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) reassign(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	var command contract.ReassignRequest
	if !decodeRequestBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	result, err := service.Reassign(c.Request.Context(), execution, id, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) cancel(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	var command contract.CancelRequest
	if !decodeRequestBody(c, &command) {
		return
	}
	if !requestIdempotencyKeyMatches(c, command.Meta.IdempotencyKey) {
		return
	}
	result, err := service.Cancel(c.Request.Context(), execution, id, command)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) history(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	page, ok := requestPage(c)
	if !ok {
		return
	}
	result, err := service.History(c.Request.Context(), execution, id, page)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) response(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	request, ok := requestID(c)
	if !ok {
		return
	}
	decision, err := contract.ParseDecisionID(c.Param("decision"))
	if err != nil {
		badRequestError(c, string(contract.CodeValidationFailed), "decision ID is invalid", nil)
		return
	}
	result, err := service.Response(c.Request.Context(), execution, request, decision)
	if err != nil {
		writeRequestError(c, err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func (h *requestHandler) markRead(c *gin.Context) {
	service, execution, ok := h.service(c)
	if !ok {
		return
	}
	id, ok := requestID(c)
	if !ok {
		return
	}
	if err := service.MarkRead(c.Request.Context(), execution, id); err != nil {
		writeRequestError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func requestPage(c *gin.Context) (contract.PageRequest, bool) {
	limit := uint64(25)
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 16)
		if err != nil {
			badRequestError(c, string(contract.CodeValidationFailed), "limit must be an integer from 1 to 100", nil)
			return contract.PageRequest{}, false
		}
		limit = parsed
	}
	if limit == 0 || limit > 100 {
		badRequestError(c, string(contract.CodeValidationFailed), "limit must be an integer from 1 to 100", nil)
		return contract.PageRequest{}, false
	}
	return contract.PageRequest{Limit: uint16(limit), Cursor: contract.PageCursor(c.Query("cursor"))}, true
}

func requestID(c *gin.Context) (contract.RequestID, bool) {
	id, err := contract.ParseRequestID(c.Param("id"))
	if err != nil {
		badRequestError(c, string(contract.CodeValidationFailed), "request ID is invalid", nil)
		return "", false
	}
	return id, true
}

func requestIdempotencyKeyMatches(c *gin.Context, bodyKey contract.IdempotencyKey) bool {
	header := c.GetHeader("Idempotency-Key")
	if header == "" {
		return true
	}
	parsed, err := contract.ParseIdempotencyKey(header)
	if err != nil || parsed != bodyKey {
		badRequestError(c, string(contract.CodeValidationFailed), "Idempotency-Key must match the command body key", nil)
		return false
	}
	return true
}

func decodeRequestBody(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, requestBodyLimit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		status, message := http.StatusBadRequest, "Request body is invalid."
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status, message = http.StatusRequestEntityTooLarge, "Request body exceeds the allowed size."
		}
		writeError(c, status, string(contract.CodeValidationFailed), message, nil)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, string(contract.CodeValidationFailed), "Request body must contain exactly one JSON value.", nil)
		return false
	}
	return true
}

func writeRequestError(c *gin.Context, err error) {
	var apiError *contract.Error
	if !errors.As(err, &apiError) {
		internalError(c, "request operation failed", err)
		return
	}
	status := http.StatusInternalServerError
	switch apiError.Type {
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeValidationFailed:
		status = http.StatusUnprocessableEntity
	case contract.CodeUnsupportedVersion:
		status = http.StatusBadRequest
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeStaleSubject, contract.CodeLeaseLost:
		status = http.StatusConflict
	case contract.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	case contract.CodeDependencyUnavailable, contract.CodeOutcomeUnknown:
		status = http.StatusServiceUnavailable
	case contract.CodeDeadlineExceeded:
		status = http.StatusGatewayTimeout
	case contract.CodeInternal:
		status = http.StatusInternalServerError
	}
	if status >= 500 {
		writeError(c, status, string(apiError.Type), "Request operation failed.", nil)
		return
	}
	writeError(c, status, string(apiError.Type), apiError.Message, nil)
}
