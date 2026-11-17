package analytics

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
)

// DrillRequest names one number. Value is a raw key, or a bucket label for the time dimension.
type DrillRequest struct {
	Model     string           `json:"model"`
	Measure   string           `json:"measure,omitempty"`
	Filters   []SemanticFilter `json:"filters,omitempty"`
	Dimension string           `json:"dimension,omitempty"`
	Value     string           `json:"value,omitempty"`
	Time      QueryTime        `json:"time"`
}

// DrillResponse is the record type and its list API `filters` triples.
type DrillResponse struct {
	Doctype string  `json:"doctype"`
	Filters [][]any `json:"filters"`
}

// BuildDrill maps a number to its list filter; no records are read.
func BuildDrill(catalog *SemanticCatalog, registry *doctype.Registry, request DrillRequest, scope contract.BusinessScope) (*DrillResponse, error) {
	model := catalog.model(request.Model)
	if model == nil || model.SourceDoctype == "" || model.ProjectionOwner != "" || model.Relation != nil || model.TimeDimension == "" {
		return nil, fmt.Errorf("model %q has no DocType source with a time dimension to drill into", request.Model)
	}
	source := registry.Get(model.SourceDoctype)
	if source == nil || request.Dimension == "" && request.Value != "" {
		return nil, fmt.Errorf("drill on %q is invalid: source unavailable or value without dimension", request.Model)
	}
	measure := cmp.Or(request.Measure, "count")
	query := ModelQuery{Model: request.Model, Measures: []string{measure}, Filters: request.Filters}
	if request.Dimension != "" {
		query.Dimensions = []string{request.Dimension}
	}
	if err := (&SemanticRequest{Queries: []ModelQuery{query}, Time: request.Time}).Validate(catalog); err != nil {
		return nil, err
	}
	// Live rows label a category "field=value"; the drill needs the bare value.
	request.Value = strings.TrimPrefix(request.Value, request.Dimension+"=")
	filters, err := drillFilters(model, request, measure)
	if err != nil {
		return nil, err
	}
	scopeFilters, err := drillScopeFilters(scope, source)
	if err != nil {
		return nil, err
	}
	return &DrillResponse{Doctype: source.Name, Filters: append(filters, scopeFilters...)}, nil
}

// drillFilters lists the measure's filters, the query filters, the grouping value
// or bucket, and the time window.
func drillFilters(model *SemanticModel, request DrillRequest, measure string) ([][]any, error) {
	var out [][]any
	for _, filter := range semanticMeasure(*model, measure).Filters {
		clause, err := listFilter(filter.Field, filter)
		if err != nil {
			return nil, err
		}
		out = append(out, clause)
	}
	for _, filter := range request.Filters {
		clause, err := listFilter(semanticDimensionField(*model, filter.Field), filter)
		if err != nil {
			return nil, err
		}
		out = append(out, clause)
	}
	from, to, err := parseSemanticWindow(request.Time.From, request.Time.To)
	if err != nil {
		return nil, err
	}
	lo, hi := from, to.AddDate(0, 0, 1)
	grouped := request.Dimension != "" && request.Dimension != model.TimeDimension
	switch {
	case grouped && request.Value == "":
		out = append(out, []any{semanticDimensionField(*model, request.Dimension), "is", nil})
	case grouped:
		out = append(out, []any{semanticDimensionField(*model, request.Dimension), "=", request.Value})
	case request.Value != "":
		if lo, hi, err = bucketWindow(request.Value, request.Time.Granularity, lo, hi); err != nil {
			return nil, err
		}
	}
	timeField := semanticDimensionField(*model, model.TimeDimension)
	return append(out, []any{timeField, ">=", lo.Format(time.DateOnly)}, []any{timeField, "<", hi.Format(time.DateOnly)}), nil
}

// bucketWindow narrows [lo, hi) to the bucket, as the bucketed query counted it.
func bucketWindow(label, granularity string, lo, hi time.Time) (time.Time, time.Time, error) {
	start, end, err := bucketBounds(label, granularity)
	if err != nil {
		return lo, hi, err
	}
	if start.After(lo) {
		lo = start
	}
	if end.Before(hi) {
		hi = end
	}
	if !lo.Before(hi) {
		return lo, hi, fmt.Errorf("bucket %q is outside the time range", label)
	}
	return lo, hi, nil
}

// listFilter converts a semantic filter to a list API triple. Operators the list
// API cannot match exactly (like, not like) are refused, not approximated.
func listFilter(field string, filter SemanticFilter) ([]any, error) {
	op := strings.NewReplacer("==", "=", "<>", "!=").Replace(strings.ToLower(strings.TrimSpace(filter.Operator)))
	values := make([]any, len(filter.Values))
	for i, value := range filter.Values {
		values[i] = value
	}
	switch {
	case len(values) == 0:
		return nil, fmt.Errorf("filter on %q has no values", field)
	case (op == "=" || op == "!=") && len(values) == 1:
		return []any{field, op, values[0]}, nil
	case op == "=" || op == "in":
		return []any{field, "in", values}, nil
	case op == "not in":
		return []any{field, "not in", values}, nil
	}
	return nil, fmt.Errorf("filter on %q with operator %q cannot be listed exactly", field, filter.Operator)
}

// bucketBounds returns the [start, end) dates of a bucket label, checking that the
// label is exactly the bucket the rollups and live queries produce.
func bucketBounds(label, granularity string) (start, end time.Time, err error) {
	if !validGranularity(granularity) {
		return start, end, fmt.Errorf("drill on time bucket %q needs time.granularity", label)
	}
	switch granularity {
	case "quarter":
		var year, quarter int
		_, err = fmt.Sscanf(label, "%d-Q%d", &year, &quarter)
		start = time.Date(year, time.Month(3*quarter-2), 1, 0, 0, 0, 0, time.UTC)
	case "year":
		var year int
		year, err = strconv.Atoi(label)
		start = time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	default:
		start, err = time.Parse(time.DateOnly, label)
	}
	if err != nil || projectionTimeBucket(start.Format(time.DateOnly), granularity) != label {
		return start, end, fmt.Errorf("%q is not a %s bucket", label, granularity)
	}
	months := map[string]int{"month": 1, "quarter": 3, "year": 12}
	return start, start.AddDate(0, months[granularity], 0).AddDate(0, 0, map[string]int{"week": 7, "day": 1}[granularity]), nil
}

// drillScopeFilters mirrors liveScopeClause: a mismatched caller has no records.
func drillScopeFilters(scope contract.BusinessScope, source *doctype.DocType) ([][]any, error) {
	entityCaller := scope.Kind == contract.ScopeKindLegalEntity
	if source.BusinessScopePolicy != contract.ScopeEntityOwned {
		if entityCaller {
			return nil, fmt.Errorf("the number has no records in this business scope")
		}
		return nil, nil
	}
	if !entityCaller || scope.LegalEntityID == "" {
		return nil, fmt.Errorf("the number has no records in this business scope")
	}
	return [][]any{{source.BusinessScopeField, "=", scope.LegalEntityID}}, nil
}
