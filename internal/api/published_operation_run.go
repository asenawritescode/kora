package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/policy"
)

// publishedOperationRun is one execution of a published operation for a verified
// execution. The HTTP route and the AI pack-action tools both run it through
// runPublishedOperation, so the checks and the error mapping live in one place.
type publishedOperationRun struct {
	Services  SiteRuntimeService
	Roles     policy.RolePolicy
	Execution policy.Execution
	Scope     contract.BusinessScope
	Ref       string
	Request   contract.ExecutePublishedOperation
}

// operationFailure is a refused published operation with the HTTP status and the
// stable code it maps to.
type operationFailure struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (f *operationFailure) Error() string { return f.Code + ": " + f.Message }

func refuseOperation(status int, code, message string) *operationFailure {
	return &operationFailure{Status: status, Code: code, Message: message}
}

// runPublishedOperation checks the request against the catalog, the business scope
// and the role matrix, then executes it through the module executor. A refusal is
// returned as *operationFailure.
func runPublishedOperation(ctx context.Context, run publishedOperationRun) (contract.OperationReceipt, error) {
	snapshot, operation, ok := publishedOperationByRef(run.Services, run.Ref)
	if !ok {
		return contract.OperationReceipt{}, refuseOperation(http.StatusNotFound, "module.operation_not_found", "Published operation not found")
	}
	if run.Request.InputSchema != operation.spec.Input {
		return contract.OperationReceipt{}, refuseOperation(http.StatusBadRequest, "validation.invalid_operation_schema", "Input schema reference does not match the published operation")
	}
	if contract.ValidateResourceBusinessScope(run.Scope, operation.published.ScopePolicy) != nil {
		return contract.OperationReceipt{}, refuseOperation(http.StatusForbidden, "permission.denied", "Published operation scope is not permitted")
	}
	if _, authErr := authorizePublishedOperation(run.Execution, run.Roles, operation.owner, operation.published, run.Scope); authErr != nil {
		return contract.OperationReceipt{}, refuseOperation(http.StatusForbidden, "permission.denied", "Published operation access is not granted")
	}
	if run.Services.ExecutePublishedOperation == nil {
		return contract.OperationReceipt{}, refuseOperation(http.StatusServiceUnavailable, "module.operation_unavailable", "Published operation executor is unavailable")
	}
	receipt, err := run.Services.ExecutePublishedOperation(ctx, run.Execution, run.Scope, operation.owner, operation.published, run.Request.InputSchema, run.Request.Meta, run.Request.Input)
	if err != nil {
		return contract.OperationReceipt{}, operationFailureFrom(err)
	}
	if receipt.Revision == "" {
		receipt.Revision = strconv.FormatUint(snapshot.Revision, 10)
	}
	if validatePublishedOperationReceipt(receipt, operation, run.Scope) != nil {
		return contract.OperationReceipt{}, refuseOperation(http.StatusInternalServerError, "module.invalid_operation_receipt", "Published operation returned an invalid receipt")
	}
	return receipt, nil
}

// operationFailureFrom maps an executor error to its HTTP status, code and details.
// An error that is already an *operationFailure passes through unchanged.
func operationFailureFrom(err error) *operationFailure {
	var failure *operationFailure
	if errors.As(err, &failure) && failure != nil {
		return failure
	}
	var typed *contract.Error
	if !errors.As(err, &typed) || typed == nil {
		return refuseOperation(http.StatusServiceUnavailable, "module.operation_failed", "Published operation could not be completed")
	}
	var details map[string]any
	if len(typed.Details) > 0 {
		_ = json.Unmarshal(typed.Details, &details)
	}
	if typed.Field != "" {
		if details == nil {
			details = map[string]any{}
		}
		details["field"] = typed.Field
	}
	return &operationFailure{Status: statusForCode(typed.Type), Code: string(typed.Type), Message: typed.Message, Details: details}
}

// statusForCode maps a contract error code to its HTTP status.
func statusForCode(code contract.Code) int {
	status := http.StatusInternalServerError
	switch code {
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
	case contract.CodeDeadlineExceeded, contract.CodeUnsupportedVersion, contract.CodeInternal:
		status = http.StatusInternalServerError
	}
	return status
}
