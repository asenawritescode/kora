package analytics

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

type SemanticQueryResponse struct {
	Queries     []SemanticQueryResult `json:"queries"`
	GeneratedAt string                `json:"generated_at"`
	DataSource  string                `json:"data_source"`
}

// ProjectionActorScope is derived from verified API authorization. Assignment
// and owner predicates are applied before report filters, aggregation, and row
// limits so analytics cannot widen actor visibility.
type ProjectionActorScope struct {
	SubjectID      string
	OwnerOnly      bool
	OwnerField     string
	AssignedFields []string
}

type SemanticQueryResult struct {
	Model          string                  `json:"model"`
	Columns        []string                `json:"columns"`
	Rows           []map[string]any        `json:"rows"`
	Total          int                     `json:"total"`
	SourceRevision string                  `json:"source_revision,omitempty"`
	AsOf           *time.Time              `json:"as_of,omitempty"`
	Freshness      contract.QueryFreshness `json:"freshness,omitempty"`
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
		if model.ProjectionOwner != "" {
			result, err := qe.resolvePublishedProjection(*model, query, request.Time, limit)
			if err != nil {
				return nil, err
			}
			response.DataSource = "published_projection"
			response.Queries = append(response.Queries, *result)
			continue
		}
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

type projectionAccumulator struct {
	group  any
	count  int
	sums   map[string]float64
	counts map[string]int
	mins   map[string]float64
	maxs   map[string]float64
}

func (qe *QueryEngine) resolvePublishedProjection(model SemanticModel, query ModelQuery, queryTime QueryTime, limit int) (*SemanticQueryResult, error) {
	scope := qe.BusinessScope
	if scope.TenantID == "" {
		scope = contract.BusinessScope{TenantID: contract.TenantID(qe.SiteName), Kind: contract.ScopeKindTenant}
	}
	if qe.DB == nil || qe.Dialect == nil || qe.SiteName == "" || scope.Validate() != nil || contract.ValidateResourceBusinessScope(scope, model.ProjectionScopePolicy) != nil {
		return nil, fmt.Errorf("published projection %q is unavailable for this business scope", model.Name)
	}
	var sourceRevision, freshness, rawData string
	var asOf time.Time
	stmt := `SELECT source_revision, as_of, freshness, data FROM _kora_analytics_module_projection WHERE site = ? AND projection_id = ? AND owner_module_id = ? AND scope_kind = ? AND legal_entity_id = ? AND source_id = 'published_sources' ORDER BY generation DESC LIMIT 1`
	err := qe.DB.QueryRow(kdb.Rebind(qe.Dialect, stmt), qe.SiteName, model.ProjectionID, model.ProjectionOwner, string(scope.Kind), scope.LegalEntityID).Scan(&sourceRevision, &asOf, &freshness, &rawData)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("published projection %q has no retained snapshot", model.Name)
		}
		return nil, fmt.Errorf("load published projection %q: %w", model.Name, err)
	}
	switch contract.QueryFreshness(freshness) {
	case contract.QueryFresh, contract.QueryStale, contract.QueryUnknown, contract.QueryIncomplete:
	default:
		return nil, fmt.Errorf("published projection %q has invalid freshness metadata", model.Name)
	}
	if sourceRevision == "" || asOf.IsZero() {
		return nil, fmt.Errorf("published projection %q is missing revision or as-of metadata", model.Name)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(rawData), &rows); err != nil {
		return nil, fmt.Errorf("published projection %q has invalid retained data", model.Name)
	}
	allowed := make(map[string]bool, len(model.AllowedFields))
	for _, field := range model.AllowedFields {
		allowed[field] = true
	}
	for _, row := range rows {
		for field := range row {
			if !allowed[field] {
				return nil, fmt.Errorf("published projection %q contains an undeclared field", model.Name)
			}
		}
	}
	if actorScope, scoped := qe.ProjectionActorScopes[model.Name]; scoped {
		if strings.TrimSpace(actorScope.SubjectID) == "" || actorScope.OwnerOnly && (actorScope.OwnerField == "" || actorScope.OwnerField != model.ProjectionOwnerField) || !actorScope.OwnerOnly && len(actorScope.AssignedFields) == 0 {
			return nil, fmt.Errorf("published projection %q actor scope is invalid", model.Name)
		}
		visible := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			matched := false
			if actorScope.OwnerOnly && strings.TrimSpace(fmt.Sprint(row[actorScope.OwnerField])) == actorScope.SubjectID {
				matched = true
			}
			for _, field := range actorScope.AssignedFields {
				if !allowed[field] {
					return nil, fmt.Errorf("published projection %q assignment field is not published", model.Name)
				}
				if strings.TrimSpace(fmt.Sprint(row[field])) == actorScope.SubjectID {
					matched = true
				}
			}
			if matched {
				visible = append(visible, row)
			}
		}
		rows = visible
	}
	filtered := make([]map[string]any, 0, len(rows))
	timeField := semanticDimensionField(model, model.TimeDimension)
	for _, row := range rows {
		if model.TimeDimension != "" {
			value, valid := projectionDateString(row[timeField])
			if !valid {
				return nil, fmt.Errorf("published projection %q has an invalid time dimension", model.Name)
			}
			if queryTime.From != "" && value < queryTime.From || queryTime.To != "" && value > queryTime.To {
				continue
			}
		}
		matches := true
		for _, filter := range query.Filters {
			if !semanticFilterMatches(fmt.Sprint(row[semanticDimensionField(model, filter.Field)]), filter) {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, row)
		}
	}
	groupField := ""
	groupName := ""
	if len(query.Dimensions) == 1 {
		groupName = query.Dimensions[0]
		groupField = semanticDimensionField(model, groupName)
	}
	groups := make(map[string]*projectionAccumulator)
	groupKeys := make([]string, 0)
	for _, row := range filtered {
		var key string
		var group any
		if groupField != "" {
			group = row[groupField]
			if groupName == model.TimeDimension && queryTime.Granularity != "" {
				date, ok := projectionDateString(group)
				if !ok {
					return nil, fmt.Errorf("published projection %q has an invalid time dimension", model.Name)
				}
				group = projectionTimeBucket(date, queryTime.Granularity)
			}
			encoded, _ := json.Marshal(group)
			key = string(encoded)
		}
		accumulator, exists := groups[key]
		if !exists {
			accumulator = &projectionAccumulator{group: group, sums: map[string]float64{}, counts: map[string]int{}, mins: map[string]float64{}, maxs: map[string]float64{}}
			groups[key] = accumulator
			groupKeys = append(groupKeys, key)
		}
		accumulator.count++
		for _, measureName := range query.Measures {
			measure := semanticMeasure(model, measureName)
			if measure.Aggregation == "count" {
				continue
			}
			number, ok := projectionNumber(row[measure.Field])
			if !ok {
				return nil, fmt.Errorf("published projection %q is missing numeric field %q", model.Name, measure.Field)
			}
			accumulator.sums[measureName] += number
			accumulator.counts[measureName]++
			if count := accumulator.counts[measureName]; count == 1 || number < accumulator.mins[measureName] {
				accumulator.mins[measureName] = number
			}
			if count := accumulator.counts[measureName]; count == 1 || number > accumulator.maxs[measureName] {
				accumulator.maxs[measureName] = number
			}
		}
	}
	if len(groupKeys) == 0 && groupField == "" {
		groups[""] = &projectionAccumulator{sums: map[string]float64{}, counts: map[string]int{}, mins: map[string]float64{}, maxs: map[string]float64{}}
		groupKeys = append(groupKeys, "")
	}
	sort.Strings(groupKeys)
	total := len(groupKeys)
	if len(groupKeys) > limit {
		groupKeys = groupKeys[:limit]
	}
	resultRows := make([]map[string]any, 0, len(groupKeys))
	for _, key := range groupKeys {
		accumulator := groups[key]
		resultRow := map[string]any{}
		if groupField != "" {
			resultRow[groupName] = accumulator.group
		}
		for _, measureName := range query.Measures {
			measure := semanticMeasure(model, measureName)
			switch measure.Aggregation {
			case "count":
				resultRow[measureName] = accumulator.count
			case "sum":
				resultRow[measureName] = accumulator.sums[measureName]
			case "avg":
				if accumulator.counts[measureName] > 0 {
					resultRow[measureName] = accumulator.sums[measureName] / float64(accumulator.counts[measureName])
				} else {
					resultRow[measureName] = 0
				}
			case "min":
				resultRow[measureName] = accumulator.mins[measureName]
			case "max":
				resultRow[measureName] = accumulator.maxs[measureName]
			default:
				return nil, fmt.Errorf("published projection %q has unsupported aggregation %q", model.Name, measure.Aggregation)
			}
		}
		resultRows = append(resultRows, resultRow)
	}
	columns := append([]string(nil), query.Dimensions...)
	columns = append(columns, query.Measures...)
	return &SemanticQueryResult{Model: query.Model, Columns: columns, Rows: resultRows, Total: total, SourceRevision: sourceRevision, AsOf: &asOf, Freshness: contract.QueryFreshness(freshness)}, nil
}

func semanticDimensionField(model SemanticModel, name string) string {
	for _, dimension := range model.Dimensions {
		if dimension.Name == name {
			return dimension.Field
		}
	}
	return name
}

func projectionDateString(value any) (string, bool) {
	text := strings.TrimSpace(fmt.Sprint(value))
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02"} {
		parsed, err := time.Parse(layout, text)
		if err == nil {
			return parsed.Format("2006-01-02"), true
		}
	}
	return "", false
}

func projectionTimeBucket(date, granularity string) string {
	parsed, _ := time.Parse("2006-01-02", date)
	switch granularity {
	case "day":
		return date
	case "week":
		weekday := (int(parsed.Weekday()) + 6) % 7
		return parsed.AddDate(0, 0, -weekday).Format("2006-01-02")
	case "month":
		return parsed.Format("2006-01") + "-01"
	case "quarter":
		quarter := (int(parsed.Month())-1)/3 + 1
		return fmt.Sprintf("%04d-Q%d", parsed.Year(), quarter)
	case "year":
		return fmt.Sprintf("%04d", parsed.Year())
	default:
		return date
	}
}

func semanticMeasure(model SemanticModel, name string) SemanticMeasure {
	for _, measure := range model.Measures {
		if measure.Name == name {
			return measure
		}
	}
	return SemanticMeasure{}
}

func projectionNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, !math.IsNaN(number) && !math.IsInf(number, 0)
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
	default:
		return 0, false
	}
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
