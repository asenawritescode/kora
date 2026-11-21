package contract

import "encoding/json"

// MetricSummary is one metric definition a caller may read, without its computation.
//
//contract:wire
type MetricSummary struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Model       string `json:"model,omitempty"`
	Format      string `json:"format,omitempty"`
	Good        string `json:"good,omitempty"`
	Description string `json:"description,omitempty"`
	Verified    bool   `json:"verified"`
}

// MetricDefinition is one metric as declared: its computation and its filters.
//
//contract:wire
type MetricDefinition struct {
	Name        string         `json:"name"`
	Label       string         `json:"label"`
	Type        string         `json:"type"`
	Model       string         `json:"model,omitempty"`
	Format      string         `json:"format,omitempty"`
	Good        string         `json:"good,omitempty"`
	Description string         `json:"description,omitempty"`
	Verified    bool           `json:"verified"`
	Time        string         `json:"time,omitempty"`
	Measure     *MetricMeasure `json:"measure,omitempty"`
	Numerator   string         `json:"numerator,omitempty"`
	Denominator string         `json:"denominator,omitempty"`
	Formula     string         `json:"formula,omitempty"`
	Filters     []MetricFilter `json:"filters,omitempty"`
}

// MetricMeasure aggregates one record field: sum, count or count_distinct.
//
//contract:wire
type MetricMeasure struct {
	Agg   string `json:"agg"`
	Field string `json:"field,omitempty"`
}

// MetricFilter keeps the rows whose field satisfies Op against Value. Value is a JSON scalar, or a
// JSON array for the "in" operator.
//
//contract:wire
type MetricFilter struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	//contract:json
	Value json.RawMessage `json:"value"`
}

// MetricQueryRequest asks for metrics over an inclusive range of business dates.
//
//contract:wire
type MetricQueryRequest struct {
	Metrics    []string       `json:"metrics"`
	Dimensions []string       `json:"dimensions,omitempty"`
	Filters    []MetricFilter `json:"filters,omitempty"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	Grain      string         `json:"grain"`
	Compare    string         `json:"compare,omitempty"`
	Top        int            `json:"top,omitempty"`
}

// MetricQueryResult holds every bucket of every group. A null value is a ratio or derived metric
// with no value for that bucket, never zero.
//
//contract:wire
type MetricQueryResult struct {
	Metrics    []string    `json:"metrics"`
	Dimensions []string    `json:"dimensions"`
	Rows       []MetricRow `json:"rows"`
	Previous   []MetricRow `json:"previous,omitempty"`
}

// MetricRow is one bucket of one group. Values follow the result's metrics: each entry is a decimal
// string, or null when the metric has no value for the bucket. Label is set only on the Other group,
// whose Group is "__other__".
//
//contract:wire
type MetricRow struct {
	Bucket string   `json:"bucket"`
	Group  []string `json:"group"`
	Label  string   `json:"label,omitempty"`
	//contract:json
	Values []json.RawMessage `json:"values"`
}

// MetricsMeta states how fresh a metric result is.
//
//contract:wire
type MetricsMeta struct {
	AsOf           string `json:"as_of"`
	Tier           string `json:"tier"`
	SourceSequence int64  `json:"source_sequence"`
}

// MetricDrillRequest names one simple metric and the group and business days to list records for.
// Group maps a dimension to its value.
//
//contract:wire
type MetricDrillRequest struct {
	Metric string            `json:"metric"`
	From   string            `json:"from"`
	To     string            `json:"to"`
	Group  map[string]string `json:"group,omitempty"`
}

// MetricDrillResult is the record list behind a number: the record type and the filters that list
// its records. The record list applies the caller's own record scope.
//
//contract:wire
type MetricDrillResult struct {
	Doctype string         `json:"doctype"`
	Filters []MetricFilter `json:"filters"`
}
