package api

import (
	"context"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

// aiModuleOperations gives the AI pack-action tools the tenant's published operations for
// one request. Both methods reuse the catalog build and the operation run of the HTTP routes.
type aiModuleOperations struct {
	h *Handler
	c *gin.Context
}

func (m aiModuleOperations) Catalog(execution policy.Execution, scope contract.BusinessScope) (contract.ApplicationCatalog, error) {
	return m.h.buildApplicationCatalog(m.c, execution, scope)
}

func (m aiModuleOperations) Run(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	if err := request.Validate(); err != nil {
		return contract.OperationReceipt{}, ai.OperationError{Code: "validation.invalid_operation_request", Message: "Invalid published operation request"}
	}
	receipt, err := runPublishedOperation(ctx, publishedOperationRun{
		Services: m.h.runtimeService(m.c, m.c.GetString("site_name")), Roles: m.h.siteRegistry(m.c),
		Execution: execution, Scope: scope, Ref: ref, Request: request,
	})
	if err != nil {
		failure := operationFailureFrom(err)
		field, _ := failure.Details["field"].(string)
		return contract.OperationReceipt{}, ai.OperationError{Code: failure.Code, Message: failure.Message, Field: field}
	}
	return receipt, nil
}
