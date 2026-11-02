package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

type publishedModuleQueryRequest struct {
	Input  json.RawMessage `json:"input"`
	Cursor string          `json:"cursor,omitempty"`
	Limit  uint16          `json:"limit,omitempty"`
}

// HandlePublishedModuleQuery executes an owner-published query through the
// tenant's pinned module provider plan. Actor authorization is performed
// against the owner module's published query resource before dispatch.
func (h *Handler) HandlePublishedModuleQuery(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	moduleID, queryName := contract.ModuleID(strings.TrimSpace(c.Param("module"))), strings.TrimSpace(c.Param("query"))
	if moduleID == "" || queryName == "" {
		writeError(c, http.StatusNotFound, "module.query_not_found", "Published query not found", nil)
		return
	}
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.ResolveModuleBindings == nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published query provider is unavailable", nil)
		return
	}
	queryValue, exists := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: moduleID, Name: queryName}]
	if !exists {
		writeError(c, http.StatusNotFound, "module.query_not_found", "Published query not found", nil)
		return
	}
	query := &queryValue
	if authErr := authorizePublishedQuery(execution, h.siteRegistry(c), *query, scope); authErr != nil {
		writeError(c, http.StatusForbidden, "permission.denied", "Published query access is not granted", nil)
		return
	}
	var request publishedModuleQueryRequest
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || len(request.Input) == 0 || len(request.Input) > 65536 {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid published query request", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid published query request", nil)
		return
	}
	result, err := plan.ExecutePublishedQuery(c.Request.Context(), scope, moduleID, queryName, request.Input, request.Cursor, request.Limit)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.query_failed", "Published query could not be completed", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func authorizePublishedQuery(execution policy.Execution, roles policy.RolePolicy, query contract.PublishedQuery, scope contract.BusinessScope) *contract.Error {
	if !execution.Valid() || query.OwnerModuleID == "" || query.Name == "" || contract.ValidateResourceBusinessScope(scope, query.ScopePolicy) != nil {
		return contract.NewError(contract.CodePermissionDenied, "published query scope is invalid")
	}
	resource := "module." + string(query.OwnerModuleID) + ".query." + query.Name
	decision, authErr := policy.DecideForBusinessScope(execution, roles, resource, "read", scope, query.ScopePolicy == contract.ScopeEntityOwned)
	if authErr != nil {
		return authErr
	}
	// Published query providers must apply row scopes themselves. The generic
	// result validator cannot safely retrofit owner or assignment filters after
	// aggregation, so those grants fail closed until the query contract carries
	// an explicit row-scope binding.
	if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
		return contract.NewError(contract.CodePermissionDenied, "published query requires an unscoped actor grant")
	}
	return nil
}
