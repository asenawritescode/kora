package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/policy"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// analyticsToolDef returns the function definition for get_analytics_insights.
func analyticsToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_analytics_insights",
			"description": "Get pre-computed analytics for a DocType. Shows counts, distributions, sums, and trends from the last 30 days — no raw data scanning. Use doctype=\"all\" to list available doctypes. Use to answer: How many X? What are the most common Y? What's the trend for Z?",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"doctype": map[string]any{
						"type":        "string",
						"description": "DocType name (e.g. \"Product\") or \"all\" to list available doctypes",
					},
				},
				"required": []string{"doctype"},
			},
		},
	}
}

func analyticsCatalogToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_analytics_catalog",
			"description": "Describe the site's available analytics models, measures, dimensions, formats, and approved reports. Use this before answering an unfamiliar reporting question so the answer uses the site's actual business data vocabulary.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func analyticsQueryToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "query_analytics",
			"description": "Answer a reporting question with governed, pre-computed analytics rollups. Query only models, measures, and dimensions returned by get_analytics_catalog. Use a time range; defaults to the last 30 days when omitted. A request is limited to 5 queries, 20 measures and filters total, 200 rows per query, and a 10-year range. Results include rows suitable for explanation or charts.",
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"queries": map[string]any{
						"type":     "array",
						"minItems": 1,
						"maxItems": analytics.MaxSemanticQueries,
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"model":      map[string]any{"type": "string", "description": "Analytics model from the catalog"},
								"measures":   map[string]any{"type": "array", "minItems": 1, "maxItems": analytics.MaxSemanticMeasures, "items": map[string]any{"type": "string", "maxLength": 128}, "description": "Measures from the model, such as count or a numeric total"},
								"dimensions": map[string]any{"type": "array", "maxItems": 1, "items": map[string]any{"type": "string", "maxLength": 128}, "description": "At most one time or category dimension"},
								"filters":    map[string]any{"type": "array", "maxItems": analytics.MaxSemanticFilters, "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"field": map[string]any{"type": "string", "maxLength": 128}, "operator": map[string]any{"type": "string", "maxLength": 16}, "values": map[string]any{"type": "array", "minItems": 1, "maxItems": analytics.MaxSemanticFilterValues, "items": map[string]any{"type": "string", "maxLength": analytics.MaxSemanticFilterBytes}}}, "required": []string{"field", "operator", "values"}}},
							},
							"required": []string{"model", "measures"},
						},
					},
					"from":        map[string]any{"type": "string", "format": "date", "description": "Inclusive ISO date; query ranges are limited to 10 years"},
					"to":          map[string]any{"type": "string", "format": "date", "description": "Inclusive ISO date; query ranges are limited to 10 years"},
					"granularity": map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}},
					"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": analytics.MaxSemanticResultRows},
				},
				"required": []string{"queries"},
			},
		},
	}
}

type analyticsQueryToolArgs struct {
	Queries     []analytics.ModelQuery `json:"queries"`
	From        string                 `json:"from,omitempty"`
	To          string                 `json:"to,omitempty"`
	Granularity string                 `json:"granularity,omitempty"`
	Limit       int                    `json:"limit,omitempty"`
}

func decodeAnalyticsQueryToolArgs(args map[string]any) (analyticsQueryToolArgs, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return analyticsQueryToolArgs{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var parsed analyticsQueryToolArgs
	if err := decoder.Decode(&parsed); err != nil {
		return analyticsQueryToolArgs{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return analyticsQueryToolArgs{}, fmt.Errorf("analytics query arguments contain trailing data")
	}
	return parsed, nil
}

func analyticsReportsToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "list_analytics_reports",
			"description": "List approved, configured reports for this site, including their questions and visual intent. Use this when the user asks about an existing report or dashboard.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func runReportToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "run_analytics_report",
			"description": "Run one approved report definition using its governed semantic queries and return chart-ready data. Use list_analytics_reports first when the report name is unknown.",
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Approved report name"},
					"from":        map[string]any{"type": "string", "format": "date"},
					"to":          map[string]any{"type": "string", "format": "date"},
					"granularity": map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}},
				},
				"required": []string{"name"},
			},
		},
	}
}

func buildAnalyticsCatalog(reg *doctype.Registry, reports []analytics.ReportDefinition) (*analytics.SemanticCatalog, error) {
	docTypes := make([]*doctype.DocType, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		docTypes = append(docTypes, reg.Get(name))
	}
	catalog := analytics.BuildSemanticCatalog(docTypes)
	if err := analytics.AddReportModels(catalog, reports); err != nil {
		return nil, err
	}
	if err := analytics.ValidateReportModels(reg, reports); err != nil {
		return nil, err
	}
	catalog.Reports = reports
	if err := catalog.Validate(); err != nil {
		return nil, err
	}
	return catalog, nil
}

func analyticsCatalogJSON(reg *doctype.Registry, reports []analytics.ReportDefinition, execution policy.Execution, scopes ...contract.BusinessScope) string {
	catalog, err := buildAnalyticsCatalog(reg, reports)
	if err != nil {
		return fmt.Sprintf("Analytics catalog is temporarily unavailable: %v", err)
	}
	catalog = filterAnalyticsCatalog(reg, execution, catalog, scopes...)
	data, err := json.Marshal(map[string]any{
		"meaning": "Models are business entities. Measures are numeric facts that can be counted or totaled. Dimensions are ways to group those facts. Related report models read governed live records; other models use Kigae's analytics rollups.",
		"catalog": catalog,
	})
	if err != nil {
		return fmt.Sprintf("Could not format analytics catalog: %v", err)
	}
	return string(data)
}

func executeAnalyticsCatalog(reg *doctype.Registry, tx *orm.TxManager, siteName string, execution policy.Execution) string {
	return analyticsCatalogJSON(reg, loadAnalyticsReports(tx, siteName), execution, tx.BusinessScope)
}

func filterAnalyticsCatalog(reg *doctype.Registry, execution policy.Execution, catalog *analytics.SemanticCatalog, scopes ...contract.BusinessScope) *analytics.SemanticCatalog {
	filtered := &analytics.SemanticCatalog{Models: []analytics.SemanticModel{}, Reports: []analytics.ReportDefinition{}}
	if catalog == nil {
		return filtered
	}
	models := make(map[string]analytics.SemanticModel, len(catalog.Models))
	for _, model := range catalog.Models {
		models[model.Name] = model
		if analyticsModelReadable(reg, execution, model, scopes...) {
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
		for modelName := range queried {
			model, exists := models[modelName]
			if !exists || !analyticsModelReadable(reg, execution, model, scopes...) {
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

func analyticsModelReadable(reg *doctype.Registry, execution policy.Execution, model analytics.SemanticModel, scopes ...contract.BusinessScope) bool {
	if reg == nil || !execution.Valid() {
		return false
	}
	if model.ProjectionOwner != "" {
		if model.ProjectionID == "" || model.PublishedQuery == "" || len(model.AllowedFields) == 0 {
			return false
		}
		scope := contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindTenant}
		if len(scopes) > 0 {
			scope = scopes[0]
		}
		decision, err := policy.DecideForBusinessScope(execution, reg, "module."+model.ProjectionOwner+".projection."+model.ProjectionID, "read", scope, model.ProjectionScopePolicy == contract.ScopeEntityOwned)
		if err != nil || decision.OwnerOnly && model.ProjectionOwnerField == "" {
			return false
		}
		allowed := make(map[string]bool, len(model.AllowedFields))
		for _, field := range model.AllowedFields {
			allowed[field] = true
		}
		for _, field := range decision.AssignedFields {
			if !allowed[field] {
				return false
			}
		}
		return true
	}
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
		decision, err := policy.Decide(execution, reg, source, "read")
		if err != nil || decision.OwnerOnly || len(decision.AssignedFields) > 0 {
			return false
		}
	}
	sourceDocType := func(source string) *doctype.DocType {
		if model.Relation == nil {
			return reg.Get(model.SourceDoctype)
		}
		switch source {
		case "parent":
			return reg.Get(model.Relation.ParentDoctype)
		case "child":
			return reg.Get(model.Relation.ChildDoctype)
		default:
			return nil
		}
	}
	readableField := func(dt *doctype.DocType, name string) bool {
		if dt == nil || strings.TrimSpace(name) == "" {
			return false
		}
		field := dt.GetField(name)
		if field == nil {
			// Generated models may use the canonical creation timestamp even
			// when it is not declared in user DocType YAML.
			return name == "creation"
		}
		return !field.Hidden && field.Fieldtype != "Password" && !field.IsLayoutField() && field.Fieldtype != "Table"
	}
	for _, measure := range model.Measures {
		if measure.Field != "" && !readableField(sourceDocType(measure.Source), measure.Field) {
			return false
		}
	}
	for _, dimension := range model.Dimensions {
		if !readableField(sourceDocType(dimension.Source), dimension.Field) {
			return false
		}
		if dimension.LinkDoctype != "" && !readableField(reg.Get(dimension.LinkDoctype), dimension.LinkLabelField) {
			return false
		}
	}
	if model.Relation != nil {
		for _, filter := range model.Relation.Filters {
			if !readableField(sourceDocType(filter.Source), filter.Field) {
				return false
			}
		}
	}
	return len(checked) > 0
}

func executeAnalyticsQuery(reg *doctype.Registry, tx *orm.TxManager, siteName string, args map[string]any, execution policy.Execution) string {
	if tx == nil || tx.DB == nil {
		return "Analytics is not available because this site has no database connection."
	}
	parsed, err := decodeAnalyticsQueryToolArgs(args)
	if err != nil {
		return "I could not understand that analytics question. Choose a model and measures from the analytics catalog."
	}
	now := time.Now()
	if parsed.From == "" {
		parsed.From = now.AddDate(0, 0, -30).Format("2006-01-02")
	}
	if parsed.To == "" {
		parsed.To = now.Format("2006-01-02")
	}
	if parsed.Granularity == "" {
		parsed.Granularity = "day"
	}
	if parsed.Limit == 0 {
		parsed.Limit = analytics.MaxSemanticResultRows
	}
	request := analytics.SemanticRequest{
		Queries: parsed.Queries, Time: analytics.QueryTime{From: parsed.From, To: parsed.To, Granularity: parsed.Granularity}, Limit: parsed.Limit,
	}
	reports := loadAnalyticsReports(tx, siteName)
	catalog, err := buildAnalyticsCatalog(reg, reports)
	if err != nil {
		return fmt.Sprintf("Analytics catalog is temporarily unavailable: %v", err)
	}
	if err := request.Validate(catalog); err != nil {
		return fmt.Sprintf("I could not run that report: %v. Use the analytics catalog to choose valid models, measures, and dimensions.", err)
	}
	actorScopes, err := authorizeRelatedAnalytics(reg, reports, request, execution, tx.BusinessScope)
	if err != nil {
		return err.Error()
	}
	result, err := (&analytics.QueryEngine{DB: tx.DB, SiteName: siteName, Dialect: tx.Dialect, BusinessScope: tx.BusinessScope, ProjectionActorScopes: actorScopes}).ResolveSemanticQuery(catalog, request, reg)
	if err != nil {
		return fmt.Sprintf("Analytics query failed: %v", err)
	}
	data, _ := json.Marshal(map[string]any{
		"meaning": "Explain the result in business terms, state the period and grouping, and distinguish zero results from missing data. Do not imply causation from a count or trend alone.",
		"result":  result,
	})
	return string(data)
}

func loadAnalyticsReports(tx *orm.TxManager, siteName string) []analytics.ReportDefinition {
	if tx == nil || tx.DB == nil {
		return []analytics.ReportDefinition{}
	}
	var configJSON string
	err := tx.DB.QueryRow(kdb.Rebind(tx.Dialect, `SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1`), siteName).Scan(&configJSON)
	if err != nil {
		return []analytics.ReportDefinition{}
	}
	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		return []analytics.ReportDefinition{}
	}
	reports := make([]analytics.ReportDefinition, 0, len(snapshot.Reports))
	for _, raw := range snapshot.Reports {
		var report analytics.ReportDefinition
		if json.Unmarshal(raw, &report) == nil {
			reports = append(reports, report)
		}
	}
	return reports
}

func executeAnalyticsReports(reg *doctype.Registry, tx *orm.TxManager, siteName string, execution policy.Execution) string {
	reports := loadAnalyticsReports(tx, siteName)
	catalog, err := buildAnalyticsCatalog(reg, reports)
	if err != nil {
		return "Approved analytics reports are temporarily unavailable."
	}
	filtered := filterAnalyticsCatalog(reg, execution, catalog, tx.BusinessScope)
	data, _ := json.Marshal(map[string]any{
		"meaning": "These are approved report definitions. Each report describes governed queries and intended visuals; it is not arbitrary SQL.",
		"reports": filtered.Reports,
	})
	return string(data)
}

func executeAnalyticsReport(reg *doctype.Registry, tx *orm.TxManager, siteName string, args map[string]any, execution policy.Execution) string {
	name, _ := args["name"].(string)
	var report *analytics.ReportDefinition
	for _, candidate := range loadAnalyticsReports(tx, siteName) {
		if candidate.Name == name {
			report = &candidate
			break
		}
	}
	if report == nil {
		return fmt.Sprintf("Approved report %q was not found. Use list_analytics_reports first.", name)
	}
	queryArgs := map[string]any{"queries": report.Queries, "from": args["from"], "to": args["to"], "granularity": args["granularity"]}
	return executeAnalyticsQuery(reg, tx, siteName, queryArgs, execution)
}

func checkRelatedAnalyticsPermissions(reg *doctype.Registry, reports []analytics.ReportDefinition, request analytics.SemanticRequest, execution policy.Execution) error {
	_, err := authorizeRelatedAnalytics(reg, reports, request, execution)
	return err
}

func authorizeRelatedAnalytics(reg *doctype.Registry, reports []analytics.ReportDefinition, request analytics.SemanticRequest, execution policy.Execution, scopes ...contract.BusinessScope) (map[string]analytics.ProjectionActorScope, error) {
	if reg == nil || reg.Permissions == nil || !execution.Valid() {
		return nil, fmt.Errorf("verified execution is required to query related report records")
	}
	catalog, err := buildAnalyticsCatalog(reg, reports)
	if err != nil {
		return nil, fmt.Errorf("analytics catalog is unavailable: %w", err)
	}
	queriedModels := make(map[string]bool, len(request.Queries))
	for _, query := range request.Queries {
		queriedModels[query.Model] = true
	}
	checked := make(map[string]bool)
	matched := make(map[string]bool, len(queriedModels))
	actorScopes := make(map[string]analytics.ProjectionActorScope)
	scope := contract.BusinessScope{TenantID: execution.Tenant(), Kind: contract.ScopeKindTenant}
	if len(scopes) > 0 {
		scope = scopes[0]
	}
	for _, model := range catalog.Models {
		if !queriedModels[model.Name] {
			continue
		}
		if !analyticsModelReadable(reg, execution, model, scopes...) {
			return nil, fmt.Errorf("The analytics model %q is unavailable for this actor.", model.Name)
		}
		matched[model.Name] = true
		if model.ProjectionOwner != "" {
			decision, err := policy.DecideForBusinessScope(execution, reg, "module."+model.ProjectionOwner+".projection."+model.ProjectionID, "read", scope, model.ProjectionScopePolicy == contract.ScopeEntityOwned)
			if err != nil {
				return nil, fmt.Errorf("The analytics model %q is unavailable for this actor.", model.Name)
			}
			if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
				subject := strings.TrimSpace(execution.Actor().SubjectUserID)
				if subject == "" {
					subject = strings.TrimSpace(execution.Actor().PrincipalID)
				}
				if subject == "" {
					return nil, fmt.Errorf("The analytics model %q has no verified row-scope subject.", model.Name)
				}
				actorScopes[model.Name] = analytics.ProjectionActorScope{SubjectID: subject, OwnerOnly: decision.OwnerOnly, OwnerField: model.ProjectionOwnerField, AssignedFields: append([]string(nil), decision.AssignedFields...)}
			}
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
			decision, err := policy.Decide(execution, reg, name, "read")
			if err != nil {
				return nil, fmt.Errorf("You do not have permission to read %s records for this report.", name)
			}
			if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
				return nil, fmt.Errorf("This report requires unscoped read access to its source records.")
			}
		}
	}
	for modelName := range queriedModels {
		if !matched[modelName] {
			return nil, fmt.Errorf("The analytics model %q is unavailable.", modelName)
		}
	}
	return actorScopes, nil
}

// executeAnalyticsInsights runs an analytics query against rollup tables.
func executeAnalyticsInsights(tx *orm.TxManager, reg *doctype.Registry, doctypeName, siteName string, execution policy.Execution) string {
	if tx == nil || tx.DB == nil {
		return "Analytics not available — no database connection."
	}
	if doctypeName != "all" {
		if err := authorizeAnalyticsInsight(reg, execution, doctypeName); err != nil {
			return err.Error()
		}
	}
	readable := make(map[string]struct{})
	if doctypeName == "all" {
		if reg == nil || !execution.Valid() {
			return "Permission denied: verified execution is required for analytics."
		}
		for _, candidate := range reg.Names() {
			if authorizeAnalyticsInsight(reg, execution, candidate) == nil {
				readable[candidate] = struct{}{}
			}
		}
		if len(readable) == 0 {
			return "Permission denied: no unscoped analytics source is readable."
		}
	}

	// "all" → list doctypes with data.
	if doctypeName == "" || doctypeName == "all" {
		rows, err := tx.DB.Query(
			kdb.Rebind(tx.Dialect, "SELECT DISTINCT doctype FROM _kora_analytics_daily WHERE site = ? LIMIT 20"),
			siteName,
		)
		if err != nil {
			return "Analytics not available. Create documents first."
		}
		defer rows.Close()
		var dts []string
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				continue
			}
			if doctypeName != "all" {
				dts = append(dts, d)
				continue
			}
			if _, allowed := readable[d]; allowed {
				dts = append(dts, d)
			}
		}
		if len(dts) == 0 {
			return "No analytics data yet. Create documents and metrics will appear within seconds."
		}
		return fmt.Sprintf("Doctypes with analytics data: %s. Use get_analytics_insights with a specific doctype name to see its metrics.", strings.Join(dts, ", "))
	}

	// Get pre-computed metrics for this doctype.
	from := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	to := time.Now().Format("2006-01-02")
	rows, err := tx.DB.Query(
		kdb.Rebind(tx.Dialect, `SELECT metric, dimension, SUM(value)
		 FROM _kora_analytics_daily
		 WHERE site = ? AND doctype = ?
		   AND date >= ? AND date <= ?
		 GROUP BY metric, dimension
		 ORDER BY metric, SUM(value) DESC`),
		siteName, doctypeName, from, to,
	)
	if err != nil {
		return fmt.Sprintf("Error querying analytics: %v", err)
	}
	defer rows.Close()

	type entry struct {
		M, D string
		V    float64
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.M, &e.D, &e.V); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return fmt.Sprintf("No analytics data for %s yet. Create some %s documents first.", doctypeName, doctypeName)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Analytics for %s (last 30 days):\n\n", doctypeName))
	cur := ""
	for _, e := range entries {
		if e.M != cur {
			cur = e.M
			label := formatAnalyticsLabel(cur, doctypeName)
			sb.WriteString(fmt.Sprintf("  %s:\n", label))
		}
		if e.D != "" {
			d := e.D
			if idx := strings.Index(d, "="); idx >= 0 {
				d = d[idx+1:]
			}
			sb.WriteString(fmt.Sprintf("    %s: %.0f\n", d, e.V))
		} else {
			sb.WriteString(fmt.Sprintf("    Total: %.0f\n", e.V))
		}
	}
	return sb.String()
}

func authorizeAnalyticsInsight(reg *doctype.Registry, execution policy.Execution, doctypeName string) error {
	if reg == nil || reg.Get(doctypeName) == nil || !execution.Valid() {
		return fmt.Errorf("Permission denied: verified read access is required for analytics source %q.", doctypeName)
	}
	decision, err := policy.Decide(execution, reg, doctypeName, "read")
	if err != nil {
		return fmt.Errorf("Permission denied: you cannot read analytics for %s.", doctypeName)
	}
	if decision.OwnerOnly || len(decision.AssignedFields) > 0 {
		return fmt.Errorf("Permission denied: owner-scoped records cannot be queried through aggregate analytics for %s.", doctypeName)
	}
	return nil
}

func formatAnalyticsLabel(metric, doctype string) string {
	prefix := strings.ToLower(strings.ReplaceAll(doctype, " ", "_")) + "_"
	name := strings.TrimPrefix(metric, prefix)
	name = strings.ReplaceAll(name, "_", " ")
	return cases.Title(language.English, cases.NoLower).String(name)
}
