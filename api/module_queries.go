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
	actorScope, authErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), *query, scope)
	if authErr != nil {
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
	var result contract.QueryResult
	if actorScope == nil {
		result, err = plan.ExecutePublishedQuery(c.Request.Context(), scope, moduleID, queryName, request.Input, request.Cursor, request.Limit)
	} else {
		result, err = plan.ExecutePublishedQueryForActor(c.Request.Context(), scope, moduleID, queryName, *actorScope, request.Input, request.Cursor, request.Limit)
	}
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.query_failed", "Published query could not be completed", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: result})
}

func authorizePublishedQuery(execution policy.Execution, roles policy.RolePolicy, query contract.PublishedQuery, scope contract.BusinessScope) *contract.Error {
	_, authErr := authorizePublishedQueryActorScope(execution, roles, query, scope)
	return authErr
}

func authorizePublishedQueryActorScope(execution policy.Execution, roles policy.RolePolicy, query contract.PublishedQuery, scope contract.BusinessScope) (*plugin.PublishedQueryActorScope, *contract.Error) {
	if !execution.Valid() || query.OwnerModuleID == "" || query.Name == "" || contract.ValidateResourceBusinessScope(scope, query.ScopePolicy) != nil {
		return nil, contract.NewError(contract.CodePermissionDenied, "published query scope is invalid")
	}
	resource := "module." + string(query.OwnerModuleID) + ".query." + query.Name
	decision, authErr := policy.DecideForBusinessScope(execution, roles, resource, "read", scope, query.ScopePolicy == contract.ScopeEntityOwned)
	if authErr != nil {
		return nil, authErr
	}
	if decision.OwnerOnly && query.OwnerField == "" {
		return nil, contract.NewError(contract.CodePermissionDenied, "published query does not declare an owner field for this grant")
	}
	allowed := make(map[string]bool, len(query.AllowedFields))
	for _, field := range query.AllowedFields {
		allowed[field] = true
	}
	if decision.OwnerOnly && !allowed[query.OwnerField] {
		return nil, contract.NewError(contract.CodePermissionDenied, "published query owner field is outside its field contract")
	}
	for _, field := range decision.AssignedFields {
		if !allowed[field] {
			return nil, contract.NewError(contract.CodePermissionDenied, "published query assignment field is outside its field contract")
		}
	}
	if !decision.OwnerOnly && len(decision.AssignedFields) == 0 {
		return nil, nil
	}
	actor := execution.Actor()
	subject := strings.TrimSpace(actor.SubjectUserID)
	if subject == "" {
		subject = strings.TrimSpace(actor.PrincipalID)
	}
	if subject == "" {
		return nil, contract.NewError(contract.CodePermissionDenied, "published query assignment scope requires a verified actor subject")
	}
	return &plugin.PublishedQueryActorScope{BusinessScope: scope, SubjectID: subject, OwnerOnly: decision.OwnerOnly, OwnerField: query.OwnerField, AssignedFields: append([]string(nil), decision.AssignedFields...)}, nil
}
