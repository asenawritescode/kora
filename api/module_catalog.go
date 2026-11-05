package api

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/plugin"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

// HandleApplicationCatalog returns the actor-filtered discovery view of the
// tenant's currently installed module surfaces. It is descriptive only; each
// execution route continues to authorize independently.
func (h *Handler) HandleApplicationCatalog(c *gin.Context) {
	execution, valid := requestExecution(c)
	if !valid {
		writeError(c, http.StatusUnauthorized, "auth.unauthenticated", "Verified identity is required", nil)
		return
	}
	scope := businessScopeFromRequest(c, execution)
	catalog, err := h.buildApplicationCatalog(c, execution, scope)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "module.catalog_unavailable", "Module catalog is unavailable", nil)
		return
	}
	// A legal-entity catalog is an authorization decision, not just a
	// descriptive empty view.  Returning 200 with no surfaces for an entity
	// outside the actor's grants lets callers mistake an unauthorized scope
	// switch for a valid, empty workspace.  Tenant-shared surfaces remain
	// visible when they are authorized, so only a completely empty entity
	// catalog is rejected here.
	if scope.Kind == contract.ScopeKindLegalEntity &&
		len(catalog.PublishedQueries) == 0 &&
		len(catalog.PublishedProjections) == 0 &&
		len(catalog.PublishedOperations) == 0 {
		writeError(c, http.StatusForbidden, "permission.denied", "Published module access is not granted for this legal entity", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: catalog})
}

func (h *Handler) buildApplicationCatalog(c *gin.Context, execution policy.Execution, scope contract.BusinessScope) (contract.ApplicationCatalog, error) {
	services := h.runtimeService(c, c.GetString("site_name"))
	if services.LoadModuleSnapshot == nil || services.ResolveModuleBindings == nil {
		return contract.ApplicationCatalog{}, fmt.Errorf("module catalog runtime is unavailable")
	}
	snapshot, err := services.LoadModuleSnapshot()
	if err != nil || snapshot.TenantID != execution.Tenant() || snapshot.Revision == 0 {
		return contract.ApplicationCatalog{}, fmt.Errorf("module catalog snapshot is unavailable")
	}
	plan, err := services.ResolveModuleBindings(scope)
	if err != nil {
		return contract.ApplicationCatalog{}, fmt.Errorf("module catalog bindings are unavailable")
	}
	catalog := contract.ApplicationCatalog{Revision: snapshot.Revision, BusinessScope: scope, Modules: []contract.ApplicationCatalogModule{}, PublishedQueries: []contract.ApplicationCatalogQuery{}, PublishedProjections: []contract.ApplicationCatalogProjection{}, PublishedOperations: []contract.ApplicationCatalogOperation{}, Schemas: []contract.ModuleSchemaDocument{}, AllowedEntityChoices: []contract.BusinessScope{}, Blockers: []contract.ModuleBlocker{}}
	for _, blocker := range plan.Blockers {
		catalog.Blockers = append(catalog.Blockers, contract.ModuleBlocker{Code: blocker.Code, Count: 1})
	}
	installed := map[contract.ModuleID]contract.ModuleInstallation{}
	for _, installation := range snapshot.Installations {
		if installation.State != string(contract.ModuleRemoved) {
			installed[installation.ModuleID] = installation
		}
	}
	if scope.Kind == contract.ScopeKindTenant {
		catalog.AvailableScopes = append(catalog.AvailableScopes, scope)
		entityIDs := map[string]bool{}
		for _, grant := range execution.Grants() {
			if grant.Scope.Kind == contract.ScopeKindLegalEntity && grant.Scope.TenantID == execution.Tenant() {
				entityIDs[grant.Scope.LegalEntityID] = true
			}
		}
		for _, permission := range h.siteRegistry(c).Permissions.AllPermissions() {
			if containsString(execution.Actor().Roles, permission.Role) && permission.Scope.Kind == contract.ScopeKindLegalEntity && permission.Scope.TenantID == execution.Tenant() {
				entityIDs[permission.Scope.LegalEntityID] = true
			}
		}
		for entityID := range entityIDs {
			entityScope := contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindLegalEntity, LegalEntityID: entityID}
			allowed := false
			for _, module := range snapshot.Modules {
				for _, query := range module.Queries {
					if query.ScopePolicy == contract.ScopeEntityOwned && authorizePublishedQuery(execution, h.siteRegistry(c), query, entityScope) == nil {
						allowed = true
					}
				}
				for _, action := range module.PublishedOperations {
					resource := "module." + string(module.ID) + ".operation." + action.Name
					if _, authErr := policy.DecideForBusinessScope(execution, h.siteRegistry(c), resource, "write", entityScope, true); authErr == nil {
						allowed = true
					}
				}
			}
			if allowed {
				catalog.AllowedEntityChoices = append(catalog.AllowedEntityChoices, entityScope)
				catalog.AvailableScopes = append(catalog.AvailableScopes, entityScope)
			}
		}
		sort.Slice(catalog.AllowedEntityChoices, func(i, j int) bool {
			return catalog.AllowedEntityChoices[i].LegalEntityID < catalog.AllowedEntityChoices[j].LegalEntityID
		})
		sort.Slice(catalog.AvailableScopes, func(i, j int) bool {
			return catalog.AvailableScopes[i].LegalEntityID < catalog.AvailableScopes[j].LegalEntityID
		})
	}
	modules := map[contract.ModuleID]contract.ModuleDescriptor{}
	for _, module := range snapshot.Modules {
		installation, ok := installed[module.ID]
		if !ok || installation.State == string(contract.ModuleRemoved) {
			continue
		}
		modules[module.ID] = module
	}
	schemaDocs := map[contract.SchemaRef]contract.ModuleSchemaDocument{}
	for _, module := range modules {
		for _, schema := range module.Schemas {
			schemaDocs[schema.Ref] = schema
		}
	}
	usedSchemas := map[contract.SchemaRef]bool{}
	visibleQueries := map[string]bool{}
	for owner, module := range modules {
		moduleVisible := false
		for _, query := range module.Queries {
			actorScope, queryAuthErr := authorizePublishedQueryActorScope(execution, h.siteRegistry(c), query, scope)
			if query.OwnerModuleID != owner || queryAuthErr != nil {
				continue
			}
			if query.ScopePolicy == contract.ScopeEntityOwned && scope.Kind != contract.ScopeKindLegalEntity {
				continue
			}
			if _, exists := plan.QueryDefinitions[plugin.PublishedQueryKey{Scope: scope, OwnerModuleID: owner, Name: query.Name}]; !exists {
				continue
			}
			catalog.PublishedQueries = append(catalog.PublishedQueries, contract.ApplicationCatalogQuery{OwnerModuleID: owner, Name: query.Name, Capability: query.Capability, Route: "/api/v1/modules/" + string(owner) + "/queries/" + query.Name, InputSchema: query.InputSchema, OutputSchema: query.OutputSchema, ScopePolicy: query.ScopePolicy, AllowedFields: append([]string(nil), query.AllowedFields...), PageLimit: query.PageLimit, Presentation: query.Presentation, OfflineEligible: plan.CanReplayPublishedQueryOfflineForActor(scope, owner, query.Name, actorScope), OfflineFactKey: query.OfflineFactKey, ReferenceLookup: query.ReferenceLookup})
			visibleQueries[contract.PublishedOperationRef(owner, query.Capability, query.Operation)] = true
			moduleVisible = true
			usedSchemas[query.InputSchema], usedSchemas[query.OutputSchema] = true, true
		}
		for _, projection := range module.Projections {
			definition, projectionScope, exists := publishedProjectionDefinition(plan, scope, owner, projection.ID)
			if !exists || definition.ID != projection.ID || definition.OwnerModuleID != owner || projectionScope != projection.ScopePolicy || projection.ScopePolicy == contract.ScopeEntityOwned && scope.Kind != contract.ScopeKindLegalEntity {
				continue
			}
			if !authorizePublishedProjection(execution, h.siteRegistry(c), projection, scope, projectionScope, "read") {
				continue
			}
			catalog.PublishedProjections = append(catalog.PublishedProjections, contract.ApplicationCatalogProjection{OwnerModuleID: owner, ID: projection.ID, Route: "/api/v1/modules/" + string(owner) + "/projections/" + projection.ID, OutputSchema: projection.OutputSchema, ScopePolicy: projectionScope, AllowedFields: append([]string(nil), projection.AllowedFields...)})
			moduleVisible = true
			usedSchemas[projection.OutputSchema] = true
		}
		for _, published := range module.PublishedOperations {
			resource := "module." + string(owner) + ".operation." + published.Name
			if _, authErr := policy.DecideForBusinessScope(execution, h.siteRegistry(c), resource, "write", scope, published.ScopePolicy == contract.ScopeEntityOwned); authErr != nil {
				continue
			}
			var offer *contract.CapabilityOffer
			for i := range module.Provides {
				if module.Provides[i].Name == published.Capability {
					offer = &module.Provides[i]
					break
				}
			}
			if offer == nil {
				continue
			}
			var operation *contract.OperationSpec
			for i := range offer.Operations {
				if offer.Operations[i].Name == published.Operation {
					operation = &offer.Operations[i]
					break
				}
			}
			if operation == nil {
				continue
			}
			availability := contract.OperationUnavailable
			if installed[owner].State == string(contract.ModuleDisabled) {
				availability = contract.OperationDisabled
			} else if installed[owner].State == string(contract.ModuleDraining) {
				availability = contract.OperationDraining
			} else {
				for _, binding := range plan.Bindings {
					if binding.OwnerModuleID == owner && binding.Key.CapabilityName == published.Capability {
						availability = contract.OperationAvailable
						break
					}
				}
				for _, blocker := range plan.Blockers {
					if blocker.Key.ConsumerModuleID == owner && blocker.Key.CapabilityName == published.Capability {
						availability = contract.OperationUnavailable
					}
				}
			}
			permissions := []string{resource + ":write"}
			if len(published.AllowedCallers) == 0 {
				continue
			}
			catalog.PublishedOperations = append(catalog.PublishedOperations, contract.ApplicationCatalogOperation{OperationRef: contract.PublishedOperationRef(owner, published.Capability, published.Operation), OwnerModuleID: owner, Capability: published.Capability, Operation: published.Operation, InputSchema: operation.Input, OutputSchema: operation.Output, Availability: availability, Permissions: permissions, Presentation: published.Presentation, OfflineEligible: published.OfflineEligible, Checkout: published.Checkout})
			moduleVisible = true
			usedSchemas[operation.Input], usedSchemas[operation.Output] = true, true
		}
		if moduleVisible {
			catalog.Modules = append(catalog.Modules, contract.ApplicationCatalogModule{ID: owner, Version: installed[owner].Version, State: installed[owner].State})
		}
	}
	downgradeUnreachableLineEntry(&catalog, visibleQueries)
	for ref := range usedSchemas {
		if schema, ok := schemaDocs[ref]; ok {
			catalog.Schemas = append(catalog.Schemas, schema)
		}
	}
	sort.Slice(catalog.Modules, func(i, j int) bool { return catalog.Modules[i].ID < catalog.Modules[j].ID })
	sort.Slice(catalog.PublishedQueries, func(i, j int) bool {
		a, b := catalog.PublishedQueries[i], catalog.PublishedQueries[j]
		if a.OwnerModuleID != b.OwnerModuleID {
			return a.OwnerModuleID < b.OwnerModuleID
		}
		return a.Name < b.Name
	})
	sort.Slice(catalog.PublishedProjections, func(i, j int) bool {
		a, b := catalog.PublishedProjections[i], catalog.PublishedProjections[j]
		if a.OwnerModuleID != b.OwnerModuleID {
			return a.OwnerModuleID < b.OwnerModuleID
		}
		return a.ID < b.ID
	})
	sort.Slice(catalog.PublishedOperations, func(i, j int) bool {
		return catalog.PublishedOperations[i].OperationRef < catalog.PublishedOperations[j].OperationRef
	})
	sort.Slice(catalog.Schemas, func(i, j int) bool {
		if catalog.Schemas[i].Ref.ID != catalog.Schemas[j].Ref.ID {
			return catalog.Schemas[i].Ref.ID < catalog.Schemas[j].Ref.ID
		}
		return catalog.Schemas[i].Ref.Version < catalog.Schemas[j].Ref.Version
	})
	if err := catalog.Validate(); err != nil {
		return contract.ApplicationCatalog{}, fmt.Errorf("module catalog is invalid: %w", err)
	}
	return catalog, nil
}

func catalogQueryAllowed(execution policy.Execution, roles policy.RolePolicy, query contract.PublishedQuery, scope contract.BusinessScope) bool {
	return authorizePublishedQuery(execution, roles, query, scope) == nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// downgradeUnreachableLineEntry falls back to the form layout when the actor
// cannot execute the hint's lookup query.
func downgradeUnreachableLineEntry(catalog *contract.ApplicationCatalog, visible map[string]bool) {
	for i := range catalog.PublishedOperations {
		p := &catalog.PublishedOperations[i].Presentation
		if p.Layout == "line_entry" && (p.LineEntry == nil || !visible[p.LineEntry.LookupQuery]) {
			p.Layout, p.LineEntry = "form", nil
		}
	}
}
