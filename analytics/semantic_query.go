package analytics

import (
	"fmt"
	"sort"
	"time"
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
func (qe *QueryEngine) ResolveSemanticQuery(catalog *SemanticCatalog, request AnalyticsQueryRequest) (*SemanticQueryResponse, error) {
	if err := request.Validate(catalog); err != nil {
		return nil, err
	}
	response := &SemanticQueryResponse{
		GeneratedAt: time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		DataSource:  "analytics_rollup",
	}
	for _, query := range request.Queries {
		model := catalog.model(query.Model)
		result, err := qe.resolveModelQuery(model, query, request.Time)
		if err != nil {
			return nil, err
		}
		response.Queries = append(response.Queries, *result)
	}
	return response, nil
}

func (qe *QueryEngine) resolveModelQuery(model *SemanticModel, query ModelQuery, queryTime QueryTime) (*SemanticQueryResult, error) {
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
		if len(query.Measures) != 1 || query.Measures[0] != "count" {
			return nil, fmt.Errorf("model %q supports category grouping only with the count measure", query.Model)
		}
		groupBy = ""
	}

	merged := map[string]map[string]any{}
	order := []string{}
	for _, measureName := range query.Measures {
		metric, err := metricForSemanticMeasure(model, measureName, query.Dimensions)
		if err != nil {
			return nil, err
		}
		metricResult, err := qe.Resolve(metric, QueryRequest{
			From:    queryTime.From,
			To:      queryTime.To,
			GroupBy: groupBy,
		})
		if err != nil {
			return nil, err
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

func metricForSemanticMeasure(model *SemanticModel, name string, dimensions []string) (*Metric, error) {
	for _, measure := range model.Measures {
		if measure.Name != name {
			continue
		}
		metricName := model.Name + "_count"
		metricType := MetricType(measure.Aggregation)
		field := measure.Field
		if len(dimensions) == 1 && dimensions[0] != model.TimeDimension {
			metricName = model.Name + "_count_by_" + dimensions[0]
			metricType = MetricCountByField
			field = dimensions[0]
		}
		if measure.Aggregation != "count" {
			metricName = model.Name + "_" + measure.Aggregation + "_" + measure.Field
		}
		return &Metric{Name: metricName, DocType: model.SourceDoctype, Type: metricType, Field: field, TimeField: model.TimeDimension}, nil
	}
	return nil, fmt.Errorf("unknown measure %q for model %q", name, model.Name)
}

func semanticRowKey(row map[string]any) string {
	if bucket, ok := row["bucket"]; ok {
		return "bucket:" + fmt.Sprint(bucket)
	}
	return "dimension:" + fmt.Sprint(row["dimension"])
}
