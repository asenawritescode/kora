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

type publishedProjectionRebuildRequest struct {
	Inputs map[string]json.RawMessage `json:"inputs,omitempty"`
}

// HandlePublishedModuleProjection serves the latest complete snapshot under
// the caller's exact business scope. Stale snapshots remain readable and carry
// their freshness state so reports cannot silently turn source outages into 0.
func (h *Handler) HandlePublishedModuleProjection(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	moduleID := contract.ModuleID(strings.TrimSpace(c.Param("module")))
	projectionID := strings.TrimSpace(c.Param("projection"))
	services := h.runtimeService(c, c.GetString("site_name"))
	if moduleID == "" || projectionID == "" || services.ResolveModuleBindings == nil || services.PublishedProjections == nil {
		writeError(c, http.StatusServiceUnavailable, "module.projection_unavailable", "Published projection is unavailable", nil)
		return
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published projection is unavailable", nil)
		return
	}
	definition, scopePolicy, ok := publishedProjectionDefinition(plan, scope, moduleID, projectionID)
	if !ok {
		writeError(c, http.StatusNotFound, "module.projection_not_found", "Published projection not found", nil)
		return
	}
	if !authorizePublishedProjection(execution, h.siteRegistry(c), definition, scope, scopePolicy, "read") {
		writeError(c, http.StatusForbidden, "permission.denied", "Published projection access is not granted", nil)
		return
	}
	snapshot, err := services.PublishedProjections.Load(c.Request.Context(), definition, scope, "published_sources")
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.projection_unavailable", "Published projection has no retained snapshot", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: contract.QueryResult{Data: snapshot.Data, SourceRevision: snapshot.SourceRevision, AsOf: snapshot.AsOf, Freshness: snapshot.Freshness}})
}

// HandleRebuildPublishedModuleProjection runs an idempotent, revision-pinned
// snapshot rebuild. Data is published only after every source page validates.
func (h *Handler) HandleRebuildPublishedModuleProjection(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	moduleID := contract.ModuleID(strings.TrimSpace(c.Param("module")))
	projectionID := strings.TrimSpace(c.Param("projection"))
	services := h.runtimeService(c, c.GetString("site_name"))
	if moduleID == "" || projectionID == "" || services.ResolveModuleBindings == nil || services.PublishedProjections == nil {
		writeError(c, http.StatusServiceUnavailable, "module.projection_unavailable", "Published projection is unavailable", nil)
		return
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.provider_unavailable", "Published projection is unavailable", nil)
		return
	}
	definition, scopePolicy, ok := publishedProjectionDefinition(plan, scope, moduleID, projectionID)
	if !ok {
		writeError(c, http.StatusNotFound, "module.projection_not_found", "Published projection not found", nil)
		return
	}
	if !authorizePublishedProjection(execution, h.siteRegistry(c), definition, scope, scopePolicy, "write") {
		writeError(c, http.StatusForbidden, "permission.denied", "Published projection rebuild is not granted", nil)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 65536)
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 65537))
	decoder.DisallowUnknownFields()
	var request publishedProjectionRebuildRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid projection rebuild request", nil)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid projection rebuild request", nil)
		return
	}
	if err := plan.RebuildPublishedProjection(c.Request.Context(), services.PublishedProjections, scope, moduleID, projectionID, request.Inputs); err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.projection_stale", "Projection rebuild failed; any prior snapshot remains available with stale freshness", nil)
		return
	}
	c.JSON(http.StatusAccepted, Response{Data: gin.H{"projection_id": projectionID, "status": "fresh"}})
}

func publishedProjectionDefinition(plan plugin.BindingPlan, scope contract.BusinessScope, owner contract.ModuleID, id string) (contract.ProjectionDefinition, contract.ResourceScopeKind, bool) {
	definition, ok := plan.Projections[plugin.PublishedProjectionKey{OwnerModuleID: owner, ID: id}]
	if !ok || definition.Validate() != nil {
		return contract.ProjectionDefinition{}, "", false
	}
	scopePolicy := plan.ProjectionScopes[plugin.PublishedProjectionKey{OwnerModuleID: owner, ID: id}]
	if contract.ValidateResourceBusinessScope(scope, scopePolicy) != nil {
		return contract.ProjectionDefinition{}, "", false
	}
	return definition, scopePolicy, true
}

func authorizePublishedProjection(execution policy.Execution, roles policy.RolePolicy, definition contract.ProjectionDefinition, scope contract.BusinessScope, scopePolicy contract.ResourceScopeKind, operation string) bool {
	if !execution.Valid() || definition.OwnerModuleID == "" || definition.ID == "" || contract.ValidateResourceBusinessScope(scope, scopePolicy) != nil {
		return false
	}
	resource := "module." + string(definition.OwnerModuleID) + ".projection." + definition.ID
	decision, err := policy.DecideForBusinessScope(execution, roles, resource, operation, scope, scopePolicy == contract.ScopeEntityOwned)
	return err == nil && !decision.OwnerOnly && len(decision.AssignedFields) == 0
}
