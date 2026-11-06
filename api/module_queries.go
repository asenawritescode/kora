package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

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
	var request contract.PublishedQueryRequest
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Validate() != nil {
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
	var inputErr *plugin.QueryInputError
	if errors.As(err, &inputErr) {
		var details map[string]any
		if inputErr.Field != "" {
			details = map[string]any{"field": inputErr.Field}
		}
		writeError(c, http.StatusBadRequest, "validation.invalid_query_request", inputErr.Message, details)
		return
	}
	if err != nil {
		slog.Error("published query failed", "module", moduleID, "query", queryName, "error", err)
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
	if src, ok := roles.(interface{ QuerySource(string) (string, bool) }); ok {
		if source, ok := src.QuerySource(resource); ok && !queryOwnsSource(roles, source, query) {
			srcDecision, srcErr := policy.Decide(execution, roles, source, "read")
			if srcErr != nil {
				return nil, srcErr
			}
			decision.OwnerOnly = decision.OwnerOnly || srcDecision.OwnerOnly
		}
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

// queryOwnsSource reports whether the query's source DocType is owned by the
// query's own module: the query grant and allowed_fields are then the whole
// read contract (decision D7 on #225). Unowned or other-module sources keep
// the source-read check.
func queryOwnsSource(roles policy.RolePolicy, source string, query contract.PublishedQuery) bool {
	reg, ok := roles.(interface{ Get(string) *doctype.DocType })
	if !ok {
		return false
	}
	dt := reg.Get(source)
	return dt != nil && dt.ModuleOwnerID != "" && dt.ModuleOwnerID == string(query.OwnerModuleID)
}
