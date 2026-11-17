package analytics

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// CompareModePreviousPeriod compares with the equal-length window before the range.
const CompareModePreviousPeriod = "previous_period"

// SemanticComparison is the previous-period companion of a query result.
type SemanticComparison struct {
	PreviousRange SemanticDateRange `json:"previous_range"`
	Totals        SemanticTotals    `json:"totals"`
	// PreviousRows pairs with result rows by position (bucketed queries only).
	PreviousRows []map[string]any `json:"previous_rows,omitempty"`
}

// SemanticDateRange is an inclusive ISO date range.
type SemanticDateRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// SemanticTotals holds whole-window totals; null when not exact from the rows.
type SemanticTotals struct {
	Current  map[string]*float64      `json:"current"`
	Previous map[string]*float64      `json:"previous"`
	Delta    map[string]SemanticDelta `json:"delta"`
}

// SemanticDelta is current minus previous; percent is null when previous is 0.
type SemanticDelta struct {
	Absolute *float64 `json:"absolute"`
	Percent  *float64 `json:"percent"`
}

// compareSemanticQuery builds the previous-window comparison. current.Rows must
// still be raw, before zero-filling.
func (qe *QueryEngine) compareSemanticQuery(call semanticCall, current *SemanticQueryResult) (*SemanticComparison, error) {
	from, to, err := parseSemanticWindow(call.time.From, call.time.To)
	if err != nil {
		return nil, err
	}
	previousFrom, previousTo := previousWindow(from, to)
	previousTime := QueryTime{From: previousFrom.Format(time.DateOnly), To: previousTo.Format(time.DateOnly), Granularity: call.time.Granularity}
	previous, err := qe.resolveSemanticModel(call.model, call.query, previousTime, call.limit, call.registry)
	if err != nil {
		return nil, err
	}
	model := call.model
	comparison := &SemanticComparison{PreviousRange: SemanticDateRange{From: previousTime.From, To: previousTime.To}, Totals: semanticTotals(*model, call.query.Measures, current.Rows, previous.Rows)}
	if bucketedQuery(*model, call.query, call.time.Granularity) {
		comparison.PreviousRows = fillSemanticBuckets(model.TimeDimension, call.query.Measures, bucketLabels(previousFrom, previousTo, call.time.Granularity), previous.Rows)
	}
	return comparison, nil
}

// bucketedQuery reports whether rows are one per time bucket.
func bucketedQuery(model SemanticModel, query ModelQuery, granularity string) bool {
	if granularity == "" || model.TimeDimension == "" || model.Relation != nil || len(query.Dimensions) > 1 {
		return false
	}
	if len(query.Dimensions) == 0 {
		return model.ProjectionOwner == ""
	}
	return query.Dimensions[0] == model.TimeDimension
}

// previousWindow returns the equal-length window ending the day before from.
func previousWindow(from, to time.Time) (time.Time, time.Time) {
	days := int(to.Sub(from).Hours()/24) + 1
	end := from.AddDate(0, 0, -1)
	return end.AddDate(0, 0, 1-days), end
}

// bucketLabels lists every bucket label touched by [from, to], in order.
func bucketLabels(from, to time.Time, granularity string) []string {
	var labels []string
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		label := projectionTimeBucket(day.Format(time.DateOnly), granularity)
		if n := len(labels); n == 0 || labels[n-1] != label {
			labels = append(labels, label)
		}
	}
	return labels
}

// fillSemanticBuckets returns one row per label, zero where there is no data.
func fillSemanticBuckets(key string, measures, labels []string, rows []map[string]any) []map[string]any {
	byLabel := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		byLabel[fmt.Sprint(row[key])] = row
	}
	filled := make([]map[string]any, 0, len(labels))
	for _, label := range labels {
		out := map[string]any{key: label}
		for _, name := range measures {
			out[name] = semanticMeasureValue(byLabel[label][name])
		}
		filled = append(filled, out)
	}
	return filled
}

func semanticMeasureValue(raw any) any {
	if text, isText := raw.(string); isText {
		if number, ok := semanticNumber(text); ok {
			return number
		}
	}
	if raw == nil {
		return 0.0
	}
	return raw
}

func semanticTotals(model SemanticModel, measures []string, current, previous []map[string]any) SemanticTotals {
	totals := SemanticTotals{Current: map[string]*float64{}, Previous: map[string]*float64{}, Delta: map[string]SemanticDelta{}}
	for _, name := range measures {
		aggregation := semanticMeasure(model, name).Aggregation
		totals.Current[name] = foldMeasure(aggregation, name, current)
		totals.Previous[name] = foldMeasure(aggregation, name, previous)
		totals.Delta[name] = semanticDelta(totals.Current[name], totals.Previous[name])
	}
	return totals
}

// foldMeasure folds per-bucket values into one total; nil counts as zero.
func foldMeasure(aggregation, name string, rows []map[string]any) *float64 {
	values := make([]float64, 0, len(rows))
	for _, row := range rows {
		value, ok := semanticNumber(row[name])
		if !ok && row[name] != nil {
			return nil
		}
		values = append(values, value)
	}
	total, ok := aggregateValues(aggregation, values)
	if !ok {
		return nil
	}
	return &total
}

// aggregateValues folds count/sum/min/max exactly; avg only for a single row.
func aggregateValues(aggregation string, values []float64) (float64, bool) {
	var total float64
	switch aggregation {
	case "count", "sum":
		for _, value := range values {
			total += value
		}
	case "min", "max":
		for i, value := range values {
			if i == 0 || (aggregation == "min" && value < total) || (aggregation == "max" && value > total) {
				total = value
			}
		}
	case "avg":
		if len(values) > 1 {
			return 0, false
		}
		if len(values) == 1 {
			total = values[0]
		}
	default:
		return 0, false
	}
	return total, true
}

func semanticDelta(current, previous *float64) SemanticDelta {
	if current == nil || previous == nil {
		return SemanticDelta{}
	}
	absolute := *current - *previous
	delta := SemanticDelta{Absolute: &absolute}
	if *previous != 0 {
		percent := absolute / math.Abs(*previous) * 100
		delta.Percent = &percent
	}
	return delta
}

// semanticNumber reads a measure value; MySQL returns SUM as a string.
func semanticNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case int:
		return float64(number), true
	case string:
		parsed, err := strconv.ParseFloat(number, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func parseSemanticWindow(from, to string) (start, end time.Time, err error) {
	if start, err = time.Parse(time.DateOnly, from); err != nil {
		return start, end, fmt.Errorf("time.from must be an ISO date: %w", err)
	}
	if end, err = time.Parse(time.DateOnly, to); err != nil {
		return start, end, fmt.Errorf("time.to must be an ISO date: %w", err)
	}
	return start, end, nil
}
