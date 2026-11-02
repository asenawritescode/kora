package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/policy"
	"github.com/gin-gonic/gin"
)

func analyticsPermissionScope(c *gin.Context, registry *doctype.Registry) string {
	roles := []string(nil)
	execution, valid := requestExecution(c)
	if valid {
		roles = append(roles, execution.Actor().Roles...)
	}
	sort.Strings(roles)
	var extensionPermissions, channelPermissions []doctype.Permission
	if value, ok := c.Get("extension_permissions"); ok {
		extensionPermissions, _ = value.([]doctype.Permission)
	}
	if value, ok := c.Get("channel_permissions"); ok {
		channelPermissions, _ = value.([]doctype.Permission)
	}
	policyDigest := ""
	if registry != nil && registry.Permissions != nil {
		policyDigest = registry.Permissions.PolicyDigest()
	}
	var grants []policy.Grant
	var executionTenant contract.TenantID
	if valid {
		grants = execution.Grants()
		executionTenant = execution.Tenant()
	}
	payload, err := json.Marshal(struct {
		Roles                []string               `json:"roles"`
		PolicyDigest         string                 `json:"policy_digest"`
		ExtensionPermissions []doctype.Permission   `json:"extension_permissions,omitempty"`
		ChannelPermissions   []doctype.Permission   `json:"channel_permissions,omitempty"`
		Grants               []policy.Grant         `json:"grants,omitempty"`
		Scope                contract.BusinessScope `json:"scope"`
		Tenant               contract.TenantID      `json:"tenant"`
	}{roles, policyDigest, extensionPermissions, channelPermissions, grants, businessScopeFromRequest(c, execution), executionTenant})
	if err != nil {
		return "unavailable"
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func checkRelatedReportPermissions(c *gin.Context, registry *doctype.Registry, report *analytics.ReportDefinition, catalog *analytics.SemanticCatalog) bool {
	if report == nil || catalog == nil {
		return false
	}
	queries := make([]analytics.ModelQuery, 0, len(report.Queries))
	queried := make(map[string]bool)
	for _, query := range report.Queries {
		queried[query.Model] = true
		queries = append(queries, analytics.ModelQuery{Model: query.Model})
	}
	// Resolve authorization against the effective catalog, not only models
	// embedded in the report. Reports commonly reference shared catalog models
	// by name; omitting report.Models must never skip their source permissions.
	modelsByName := make(map[string]analytics.SemanticModel, len(queried))
	for _, model := range catalog.Models {
		if queried[model.Name] {
			modelsByName[model.Name] = model
		}
	}
	for _, model := range report.Models {
		if queried[model.Name] {
			if _, exists := modelsByName[model.Name]; !exists {
				modelsByName[model.Name] = model
			}
		}
	}
	if len(modelsByName) != len(queried) {
		// An unresolved model cannot be checked for source permissions. Deny
		// rather than treating absence from the effective catalog as permission.
		return false
	}
	models := make([]analytics.SemanticModel, 0, len(modelsByName))
	for _, model := range modelsByName {
		models = append(models, model)
	}
	return checkRelatedModelPermissions(c, registry, models, analytics.AnalyticsQueryRequest{Queries: queries})
}

func checkRelatedModelPermissions(c *gin.Context, registry *doctype.Registry, models []analytics.SemanticModel, request analytics.AnalyticsQueryRequest) bool {
	queried := make(map[string]bool, len(request.Queries))
	for _, query := range request.Queries {
		queried[query.Model] = true
	}
	checked := make(map[string]bool)
	for _, model := range models {
		if !queried[model.Name] {
			continue
		}
		doctypes := []string{model.SourceDoctype}
		if model.Relation != nil {
			doctypes = append(doctypes, model.Relation.ParentDoctype, model.Relation.ChildDoctype)
		}
		for _, dimension := range model.Dimensions {
			if dimension.LinkDoctype != "" {
				doctypes = append(doctypes, dimension.LinkDoctype)
			}
		}
		for _, name := range doctypes {
			if name == "" || checked[name] {
				continue
			}
			checked[name] = true
			if !checkUnscopedAnalyticsRead(c, registry, name) {
				return false
			}
		}
	}
	return true
}

func checkUnscopedAnalyticsRead(c *gin.Context, registry *doctype.Registry, doctypeName string) bool {
	if doctypeName == "" {
		return true
	}
	if dt := registry.Get(doctypeName); dt != nil && dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		c.JSON(http.StatusForbidden, ErrorResponse{Error: map[string]string{
			"code":    "permission.entity_scope_unsupported",
			"message": "Analytics queries cannot read entity-owned sources through an unscoped projection",
		}})
		return false
	}
	scope, denied := checkPerm(c, registry, doctypeName, "read")
	if denied {
		return false
	}
	if scope.Owner || len(scope.AssignedFields) > 0 {
		c.JSON(http.StatusForbidden, ErrorResponse{Error: map[string]string{
			"code":    "permission.row_scope_unsupported",
			"message": "Analytics queries require unscoped read access to source records",
		}})
		return false
	}
	return true
}

func analyticsSourceReadable(c *gin.Context, registry *doctype.Registry, doctypeName string) bool {
	if doctypeName == "" || registry == nil {
		return false
	}
	if dt := registry.Get(doctypeName); dt != nil && dt.BusinessScopePolicy == contract.ScopeEntityOwned {
		return false
	}
	execution, valid := requestExecution(c)
	if !valid {
		return false
	}
	decision, err := policy.Decide(execution, registry, doctypeName, "read")
	return err == nil && !decision.OwnerOnly && len(decision.AssignedFields) == 0
}

func filterReadableAnalyticsMetrics(c *gin.Context, registry *doctype.Registry, metrics []*analytics.Metric) []*analytics.Metric {
	visible := make([]*analytics.Metric, 0, len(metrics))
	for _, metric := range metrics {
		if metric != nil && analyticsSourceReadable(c, registry, metric.DocType) {
			visible = append(visible, metric)
		}
	}
	return visible
}

func analyticsModelSourcesReadable(c *gin.Context, registry *doctype.Registry, model analytics.SemanticModel) bool {
	sources := []string{model.SourceDoctype}
	if model.Relation != nil {
		sources = append(sources, model.Relation.ParentDoctype, model.Relation.ChildDoctype)
	}
	for _, dimension := range model.Dimensions {
		if dimension.LinkDoctype != "" {
			sources = append(sources, dimension.LinkDoctype)
		}
	}
	checked := map[string]bool{}
	for _, source := range sources {
		if source == "" || checked[source] {
			continue
		}
		checked[source] = true
		if !analyticsSourceReadable(c, registry, source) {
			return false
		}
	}
	return len(checked) > 0
}

func filterReadableAnalyticsCatalog(c *gin.Context, registry *doctype.Registry, catalog *analytics.SemanticCatalog) *analytics.SemanticCatalog {
	filtered := &analytics.SemanticCatalog{Models: []analytics.SemanticModel{}, Reports: []analytics.ReportDefinition{}}
	if catalog == nil {
		return filtered
	}
	readable := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		readable[model.Name] = analyticsModelSourcesReadable(c, registry, model)
		if readable[model.Name] {
			filtered.Models = append(filtered.Models, model)
		}
	}
	for _, report := range catalog.Reports {
		queried := make(map[string]bool)
		for _, query := range report.Queries {
			queried[query.Model] = true
		}
		if len(queried) == 0 {
			continue
		}
		visible := true
		for model := range queried {
			if !readable[model] {
				visible = false
				break
			}
		}
		if visible {
			filtered.Reports = append(filtered.Reports, report)
		}
	}
	return filtered
}

func filterReadableAnalyticsReports(c *gin.Context, registry *doctype.Registry, reports []analytics.ReportDefinition) []analytics.ReportDefinition {
	docTypes := make([]*doctype.DocType, 0)
	if registry != nil {
		for _, name := range registry.Names() {
			docTypes = append(docTypes, registry.Get(name))
		}
	}
	catalog := analytics.BuildSemanticCatalog(docTypes)
	if err := analytics.AddReportModels(catalog, reports); err != nil {
		return []analytics.ReportDefinition{}
	}
	catalog.Reports = reports
	return filterReadableAnalyticsCatalog(c, registry, catalog).Reports
}
