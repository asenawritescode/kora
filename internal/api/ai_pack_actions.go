package api

import (
	"context"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/gin-gonic/gin"
)

// NewAIModuleOperations returns the tenant's published operations for one request as the AI
// pack-action tools see them.
func NewAIModuleOperations(h *Handler, c *gin.Context) ai.ModuleOperations {
	return aiModuleOperations{h: h, c: c}
}

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
	return m.execute(ctx, publishedOperationRun{Execution: execution, Scope: scope, Ref: ref, Request: request})
}

func (m aiModuleOperations) Preview(ctx context.Context, execution policy.Execution, scope contract.BusinessScope, ref string, request contract.ExecutePublishedOperation) (contract.OperationReceipt, error) {
	return m.execute(ctx, publishedOperationRun{Execution: execution, Scope: scope, Ref: ref, Request: request, Preview: true})
}

// execute is the one path of Run and Preview: the same operation core, with the run's options.
func (m aiModuleOperations) execute(ctx context.Context, run publishedOperationRun) (contract.OperationReceipt, error) {
	if err := run.Request.Validate(); err != nil {
		return contract.OperationReceipt{}, ai.OperationError{Code: "validation.invalid_operation_request", Message: "Invalid published operation request"}
	}
	run.Services = m.h.runtimeService(m.c, m.c.GetString("site_name"))
	run.Roles = m.h.siteRegistry(m.c)
	receipt, err := runPublishedOperation(ctx, run)
	if err != nil {
		failure := operationFailureFrom(err)
		field, _ := failure.Details["field"].(string)
		return contract.OperationReceipt{}, ai.OperationError{Code: failure.Code, Message: failure.Message, Field: field}
	}
	return receipt, nil
}
