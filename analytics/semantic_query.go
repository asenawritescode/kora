package analytics

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/doctype"
)

type SemanticQueryResponse struct {
	Queries     []SemanticQueryResult `json:"queries"`
	GeneratedAt string                `json:"generated_at"`
	DataSource  string                `json:"data_source"`
}

type SemanticQueryResult struct {
	Model   string           `json:"model"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Total   int              `json:"total"`
}

// ResolveSemanticQuery resolves the safe subset of semantic queries that can
// be served by the existing metric rollups. It intentionally refuses queries
// that would require an unsafe or ambiguous raw-table join.
func (qe *QueryEngine) ResolveSemanticQuery(catalog *SemanticCatalog, request AnalyticsQueryRequest, registries ...*doctype.Registry) (*SemanticQueryResponse, error) {
	if err := request.Validate(catalog); err != nil {
		return nil, err
	}
	limit := request.Limit
	if limit == 0 {
		limit = MaxSemanticResultRows
	}
	response := &SemanticQueryResponse{
		GeneratedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		DataSource:  "analytics_rollup",
	}
	var registry *doctype.Registry
	if len(registries) > 0 {
		registry = registries[0]
	}
	for _, query := range request.Queries {
		model := catalog.model(query.Model)
		result, err := qe.resolveModelQuery(model, query, request.Time, limit, registry)
		if err != nil {
			return nil, err
		}
		if model.Relation != nil {
			response.DataSource = "live_records"
		}
		response.Queries = append(response.Queries, *result)
	}
	return response, nil
}

func (qe *QueryEngine) resolveModelQuery(model *SemanticModel, query ModelQuery, queryTime QueryTime, limit int, registry *doctype.Registry) (*SemanticQueryResult, error) {
	if len(query.Measures) == 0 {
		return nil, fmt.Errorf("model %q requires at least one measure", query.Model)
	}
	if len(query.Dimensions) > 1 {
		return nil, fmt.Errorf("model %q currently supports one grouping dimension per query", query.Model)
	}

	groupBy := queryTime.Granularity
	if groupBy != "" && !validGranularity(groupBy) {
		return nil, fmt.Errorf("unsupported query granularity %q", groupBy)
	}
	if len(query.Dimensions) == 1 && query.Dimensions[0] != model.TimeDimension {
		groupBy = ""
	}
	if len(query.Filters) > 0 && len(query.Dimensions) != 1 {
		return nil, fmt.Errorf("model %q filters require one grouping dimension so the rollup can apply them safely", query.Model)
	}

	merged := map[string]map[string]any{}
	order := []string{}
	dimensionName := model.TimeDimension
	if len(query.Dimensions) == 1 {
		dimensionName = query.Dimensions[0]
	}
	for _, measureName := range query.Measures {
		var metricResult *QueryResult
		var err error
		if model.Relation != nil {
			metricResult, err = qe.resolveRelatedMeasure(registry, model, query, queryTime, measureName, limit)
		} else {
			var metric *Metric
			metric, err = metricForSemanticMeasure(model, measureName, query.Dimensions)
			if err == nil {
				metricResult, err = qe.Resolve(metric, QueryRequest{
					From:      queryTime.From,
					To:        queryTime.To,
					GroupBy:   groupBy,
					Dimension: dimensionName,
					Filters:   query.Filters,
					Limit:     limit,
				})
			}
		}
		if err != nil {
			return nil, err
		}
		if len(query.Filters) > 0 {
			// Category filters are applied in SQL before aggregation and LIMIT.
			// Keep a defensive post-filter for alternate metric sources, but do
			// not re-filter rollup results: their dimension column contains the
			// encoded "field=value" form used by the database predicate.
			if model.Relation != nil {
				metricResult.Rows = filterSemanticRows(metricResult.Rows, query.Dimensions[0], query.Filters)
				metricResult.Total = len(metricResult.Rows)
			}
		}
		for _, row := range metricResult.Rows {
			key := semanticRowKey(row)
			if _, ok := merged[key]; !ok {
				merged[key] = map[string]any{}
				if bucket, ok := row["bucket"]; ok {
					merged[key][model.TimeDimension] = bucket
				} else if dimension, ok := row["dimension"]; ok {
					merged[key][query.Dimensions[0]] = dimension
				}
				order = append(order, key)
			}
			merged[key][measureName] = row["value"]
		}
	}

	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		rows = append(rows, merged[key])
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(query.Dimensions) == 1 && query.Dimensions[0] != model.TimeDimension {
		sort.Slice(rows, func(i, j int) bool {
			return fmt.Sprint(rows[i][query.Dimensions[0]]) < fmt.Sprint(rows[j][query.Dimensions[0]])
		})
	}
	columns := append([]string{}, query.Dimensions...)
	if groupBy != "" && len(columns) == 0 {
		columns = append(columns, model.TimeDimension)
	}
	columns = append(columns, query.Measures...)
	return &SemanticQueryResult{Model: query.Model, Columns: columns, Rows: rows, Total: len(rows)}, nil
}

func filterSemanticRows(rows []map[string]any, dimension string, filters []SemanticFilter) []map[string]any {
	filtered := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		value := fmt.Sprint(row[dimension])
		if bucket, ok := row["bucket"]; ok {
			value = fmt.Sprint(bucket)
		}
		if dimensionValue, ok := row["dimension"]; ok {
			value = fmt.Sprint(dimensionValue)
			if index := strings.Index(value, "="); index >= 0 {
				value = value[index+1:]
			}
		}
		matches := true
		for _, filter := range filters {
			if !semanticFilterMatches(value, filter) {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func semanticFilterMatches(value string, filter SemanticFilter) bool {
	op := strings.ToLower(strings.TrimSpace(filter.Operator))
	if op == "not in" || op == "not like" {
		for _, candidate := range filter.Values {
			candidate = strings.TrimSpace(candidate)
			if (op == "not in" && value == candidate) || (op == "not like" && strings.Contains(strings.ToLower(value), strings.ToLower(candidate))) {
				return false
			}
		}
		return true
	}
	for _, candidate := range filter.Values {
		candidate = strings.TrimSpace(candidate)
		matched := false
		switch op {
		case "=", "==", "in":
			matched = value == candidate
		case "!=", "<>":
			matched = value != candidate
		case "like":
			matched = strings.Contains(strings.ToLower(value), strings.ToLower(candidate))
		default:
			return false
		}
		if matched {
			return true
		}
	}
	return false
}

func metricForSemanticMeasure(model *SemanticModel, name string, dimensions []string) (*Metric, error) {
	for _, measure := range model.Measures {
		if measure.Name != name {
			continue
		}
		metricName := model.Name + "_count"
		metricType := MetricType(measure.Aggregation)
		field := measure.Field
		groupByField := ""
		if len(dimensions) == 1 && dimensions[0] != model.TimeDimension {
			metricName = model.Name + "_count_by_" + dimensions[0]
			metricType = MetricCountByField
			field = dimensions[0]
		}
		if measure.Aggregation != "count" {
			metricName = model.Name + "_" + measure.Aggregation + "_" + measure.Field
		}
		if len(dimensions) == 1 && dimensions[0] != model.TimeDimension && measure.Aggregation != "count" {
			metricName += "_by_" + dimensions[0]
			metricType = MetricSumByField
			groupByField = dimensions[0]
		}
		return &Metric{Name: metricName, DocType: model.SourceDoctype, Type: metricType, Field: field, GroupByField: groupByField, TimeField: model.TimeDimension}, nil
	}
	return nil, fmt.Errorf("unknown measure %q for model %q", name, model.Name)
}

func semanticRowKey(row map[string]any) string {
	if bucket, ok := row["bucket"]; ok {
		return "bucket:" + fmt.Sprint(bucket)
	}
	return "dimension:" + fmt.Sprint(row["dimension"])
}
