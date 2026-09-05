package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
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
			"description": "Answer a reporting question with governed, pre-computed analytics rollups. Query only models, measures, and dimensions returned by get_analytics_catalog. Use a time range; defaults to the last 30 days when omitted. Results include rows suitable for explanation or charts.",
			"parameters": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"queries": map[string]any{
						"type":     "array",
						"maxItems": 5,
						"items": map[string]any{
							"type":                 "object",
							"additionalProperties": false,
							"properties": map[string]any{
								"model":      map[string]any{"type": "string", "description": "Analytics model from the catalog"},
								"measures":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Measures from the model, such as count or a numeric total"},
								"dimensions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "At most one time or category dimension"},
								"filters":    map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"field": map[string]any{"type": "string"}, "operator": map[string]any{"type": "string"}, "values": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "required": []string{"field", "operator", "values"}}},
							},
							"required": []string{"model", "measures"},
						},
					},
					"from":        map[string]any{"type": "string", "format": "date", "description": "Inclusive ISO date"},
					"to":          map[string]any{"type": "string", "format": "date", "description": "Inclusive ISO date"},
					"granularity": map[string]any{"type": "string", "enum": []string{"day", "week", "month", "quarter", "year"}},
					"limit":       map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				},
				"required": []string{"queries"},
			},
		},
	}
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

func buildAnalyticsCatalog(reg *doctype.Registry) *analytics.SemanticCatalog {
	docTypes := make([]*doctype.DocType, 0, len(reg.Names()))
	for _, name := range reg.Names() {
		docTypes = append(docTypes, reg.Get(name))
	}
	return analytics.BuildSemanticCatalog(docTypes)
}

func analyticsCatalogJSON(reg *doctype.Registry, reports []analytics.ReportDefinition) string {
	catalog := buildAnalyticsCatalog(reg)
	catalog.Reports = reports
	if err := catalog.Validate(); err != nil {
		return fmt.Sprintf("Analytics catalog is temporarily unavailable: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"meaning": "Models are business entities. Measures are numeric facts that can be counted or totaled. Dimensions are ways to group those facts. Values come from Kora's pre-computed analytics rollups.",
		"catalog": catalog,
	})
	if err != nil {
		return fmt.Sprintf("Could not format analytics catalog: %v", err)
	}
	return string(data)
}

func executeAnalyticsCatalog(reg *doctype.Registry, tx *orm.TxManager, siteName string) string {
	return analyticsCatalogJSON(reg, loadAnalyticsReports(tx, siteName))
}

func executeAnalyticsQuery(reg *doctype.Registry, tx *orm.TxManager, siteName string, args map[string]any) string {
	if tx == nil || tx.DB == nil {
		return "Analytics is not available because this site has no database connection."
	}
	var request analytics.AnalyticsQueryRequest
	data, err := json.Marshal(args)
	if err != nil || json.Unmarshal(data, &request) != nil {
		return "I could not understand that analytics question. Choose a model and measures from the analytics catalog."
	}
	now := time.Now()
	if request.Time.From == "" {
		request.Time.From = now.AddDate(0, 0, -30).Format("2006-01-02")
	}
	if request.Time.To == "" {
		request.Time.To = now.Format("2006-01-02")
	}
	if request.Time.Granularity == "" {
		request.Time.Granularity = "day"
	}
	if request.Limit == 0 {
		request.Limit = 200
	}
	catalog := buildAnalyticsCatalog(reg)
	if err := request.Validate(catalog); err != nil {
		return fmt.Sprintf("I could not run that report: %v. Use the analytics catalog to choose valid models, measures, and dimensions.", err)
	}
	result, err := (&analytics.QueryEngine{DB: tx.DB, SiteName: siteName}).ResolveSemanticQuery(catalog, request)
	if err != nil {
		return fmt.Sprintf("Analytics query failed: %v", err)
	}
	data, _ = json.Marshal(map[string]any{
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
	err := tx.DB.QueryRow(`SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1`, siteName).Scan(&configJSON)
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

func executeAnalyticsReports(reg *doctype.Registry, tx *orm.TxManager, siteName string) string {
	data, _ := json.Marshal(map[string]any{
		"meaning": "These are approved report definitions. Each report describes governed queries and intended visuals; it is not arbitrary SQL.",
		"reports": loadAnalyticsReports(tx, siteName),
	})
	return string(data)
}

func executeAnalyticsReport(reg *doctype.Registry, tx *orm.TxManager, siteName string, args map[string]any) string {
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
	return executeAnalyticsQuery(reg, tx, siteName, queryArgs)
}

// executeAnalyticsInsights runs an analytics query against rollup tables.
func executeAnalyticsInsights(tx *orm.TxManager, reg *doctype.Registry, doctypeName, siteName string) string {
	if tx == nil || tx.DB == nil {
		return "Analytics not available — no database connection."
	}

	// "all" → list doctypes with data.
	if doctypeName == "" || doctypeName == "all" {
		rows, err := tx.DB.Query(
			"SELECT DISTINCT doctype FROM _kora_analytics_daily WHERE site = ? LIMIT 20",
			siteName,
		)
		if err != nil {
			return "Analytics not available. Create documents first."
		}
		defer rows.Close()
		var dts []string
		for rows.Next() {
			var d string
			rows.Scan(&d)
			dts = append(dts, d)
		}
		if len(dts) == 0 {
			return "No analytics data yet. Create documents and metrics will appear within seconds."
		}
		return fmt.Sprintf("Doctypes with analytics data: %s. Use get_analytics_insights with a specific doctype name to see its metrics.", strings.Join(dts, ", "))
	}

	// Get pre-computed metrics for this doctype.
	rows, err := tx.DB.Query(
		`SELECT metric, dimension, SUM(value)
		 FROM _kora_analytics_daily
		 WHERE site = ? AND doctype = ?
		   AND date >= DATE_SUB(CURDATE(), INTERVAL 30 DAY)
		 GROUP BY metric, dimension
		 ORDER BY metric, SUM(value) DESC`,
		siteName, doctypeName,
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
		rows.Scan(&e.M, &e.D, &e.V)
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

func formatAnalyticsLabel(metric, doctype string) string {
	prefix := strings.ToLower(strings.ReplaceAll(doctype, " ", "_")) + "_"
	name := strings.TrimPrefix(metric, prefix)
	name = strings.ReplaceAll(name, "_", " ")
	return strings.Title(name)
}
