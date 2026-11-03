package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// The envelope is slightly larger than the maximum 1 MiB input field.
const publishedOperationBodyLimit = (1 << 20) + 4096

func (h *Handler) HandleExecutePublishedOperation(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	operationRef := strings.TrimSpace(c.Param("operation"))
	if operationRef == "" {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	var request contract.ExecutePublishedOperation
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, publishedOperationBodyLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Validate() != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_request", "Invalid published operation request", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_request", "Invalid published operation request", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	snapshot, operation, ok := publishedOperationByRef(services, operationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	if request.InputSchema != operation.spec.Input {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_schema", "Input schema reference does not match the published operation", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return
	}
	if services.ExecutePublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_unavailable", "Published operation executor is unavailable", nil)
		return
	}
	receipt, err := services.ExecutePublishedOperation(c.Request.Context(), execution, scope, operation.owner, operation.published, request.InputSchema, request.Meta, request.Input)
	if err != nil {
		operationErrorResponse(c, err)
		return
	}
	if receipt.Revision == "" {
		receipt.Revision = strconv.FormatUint(snapshot.Revision, 10)
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

func (h *Handler) HandleLookupPublishedOperation(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	var request contract.LookupPublishedOperation
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Validate() != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_lookup", "Invalid published operation lookup", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_operation_lookup", "Invalid published operation lookup", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	_, operation, ok := publishedOperationByRef(services, request.OperationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_not_found", "Published operation not found", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation access is not granted", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if services.LookupPublishedOperation == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_lookup_unavailable", "Published operation lookup is unavailable", nil)
		return
	}
	receipt, found, err := services.LookupPublishedOperation(c.Request.Context(), execution, scope, operation.owner, operation.published, request.IdempotencyKey)
	if err != nil {
		operationErrorResponse(c, err)
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "No operation receipt exists for this intent", nil)
		return
	}
	if contract.ValidateResourceBusinessScope(scope, operation.published.ScopePolicy) != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published operation scope is not permitted", nil)
		return
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

func (h *Handler) HandleGetPublishedOperationReceipt(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	id, err := contract.ParseOperationID(strings.TrimSpace(c.Param("execution")))
	if err != nil {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LookupPublishedOperationByID == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_lookup_unavailable", "Published operation lookup is unavailable", nil)
		return
	}
	receipt, found, lookupErr := services.LookupPublishedOperationByID(c.Request.Context(), execution, scope, id)
	if lookupErr != nil {
		operationErrorResponse(c, lookupErr)
		return
	}
	if !found {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	_, operation, ok := publishedOperationByRef(services, receipt.OperationRef)
	if !ok {
		writeError(c, http.StatusNotFound, "module.operation_receipt_not_found", "Operation receipt not found", nil)
		return
	}
	if _, authErr := authorizePublishedOperation(execution, h.siteRegistry(c), operation.owner, operation.published, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Operation receipt access is not granted", nil)
		return
	}
	if err := validatePublishedOperationReceipt(receipt, operation, scope); err != nil {
		writeError(c, http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: receipt})
}

func validatePublishedOperationReceipt(receipt contract.OperationReceipt, operation registeredPublishedOperation, scope contract.BusinessScope) error {
	if receipt.Validate() != nil || receipt.BusinessScope != scope || receipt.OperationRef != contract.PublishedOperationRef(operation.owner, operation.published.Capability, operation.published.Operation) {
		return errors.New("receipt identity does not match the published operation")
	}
	if receipt.Status == contract.ReceiptCompleted && (receipt.OutputSchema == nil || *receipt.OutputSchema != operation.spec.Output) {
		return errors.New("receipt output schema does not match the published operation")
	}
	return nil
}

type registeredPublishedOperation struct {
	owner     contract.ModuleID
	published contract.PublishedOperation
	spec      contract.OperationSpec
}

func publishedOperationByRef(services SiteRuntimeService, ref string) (contract.ModuleSnapshot, registeredPublishedOperation, bool) {
	if services.LoadModuleSnapshot == nil {
		return contract.ModuleSnapshot{}, registeredPublishedOperation{}, false
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil {
		return contract.ModuleSnapshot{}, registeredPublishedOperation{}, false
	}
	for _, module := range snapshot.Modules {
		for _, published := range module.PublishedOperations {
			if contract.PublishedOperationRef(module.ID, published.Capability, published.Operation) != ref {
				continue
			}
			for _, offer := range module.Provides {
				if offer.Name == published.Capability {
					for _, spec := range offer.Operations {
						if spec.Name == published.Operation {
							return snapshot, registeredPublishedOperation{module.ID, published, spec}, true
						}
					}
				}
			}
		}
	}
	return snapshot, registeredPublishedOperation{}, false
}

func authorizePublishedOperation(execution policy.Execution, roles policy.RolePolicy, owner contract.ModuleID, published contract.PublishedOperation, scope contract.BusinessScope) (policy.Decision, *contract.Error) {
	if published.ScopePolicy != contract.ScopeTenantShared && published.ScopePolicy != contract.ScopeEntityOwned {
		return policy.Decision{}, contract.NewError(contract.CodePermissionDenied, "published operation scope policy is invalid")
	}
	resource := "module." + string(owner) + ".operation." + published.Name
	return policy.DecideForBusinessScope(execution, roles, resource, "write", scope, published.ScopePolicy == contract.ScopeEntityOwned)
}

func operationErrorResponse(c *gin.Context, err error) {
	var typed *contract.Error
	if !errors.As(err, &typed) || typed == nil {
		writeError(c, http.StatusServiceUnavailable, "module.operation_failed", "Published operation could not be completed", nil)
		return
	}
	status := http.StatusInternalServerError
	switch typed.Type {
	case contract.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case contract.CodePermissionDenied:
		status = http.StatusForbidden
	case contract.CodeValidationFailed:
		status = http.StatusBadRequest
	case contract.CodeNotFound:
		status = http.StatusNotFound
	case contract.CodeConflict, contract.CodeIdempotencyKeyReused, contract.CodeLeaseLost, contract.CodeStaleSubject, contract.CodeOutcomeUnknown:
		status = http.StatusConflict
	case contract.CodeDependencyUnavailable:
		status = http.StatusServiceUnavailable
	case contract.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	}
	var details map[string]any
	if len(typed.Details) > 0 {
		_ = json.Unmarshal(typed.Details, &details)
	}
	writeError(c, status, string(typed.Type), typed.Message, details)
}
