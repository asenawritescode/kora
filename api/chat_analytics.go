package api

import (
	"fmt"
	"strings"

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
			return "Analytics not available. Set KORA_ANALYTICS=true and create documents first."
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

	type entry struct{ M, D string; V float64 }
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
