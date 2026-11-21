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

// MetricQueryRequest asks for metrics over an inclusive range of business dates. Grain is day, week,
// month or range; range returns one bucket for the whole range, keyed by From. Measures ask for
// unsaved, unverified measures of a record type, beside the named metrics.
//
//contract:wire
type MetricQueryRequest struct {
	Metrics    []string             `json:"metrics"`
	Measures   []MetricAdHocMeasure `json:"measures,omitempty"`
	Dimensions []string             `json:"dimensions,omitempty"`
	Filters    []MetricFilter       `json:"filters,omitempty"`
	From       string               `json:"from"`
	To         string               `json:"to"`
	Grain      string               `json:"grain"`
	Compare    string               `json:"compare,omitempty"`
	Top        int                  `json:"top,omitempty"`
}

// MetricAdHocMeasure is one measure of a record type asked for without a saved metric: a count, a sum
// of a measure field, or a count_distinct of a dimension field, optionally filtered.
//
//contract:wire
type MetricAdHocMeasure struct {
	RecordType string         `json:"record_type"`
	Agg        string         `json:"agg"`
	Field      string         `json:"field,omitempty"`
	Filters    []MetricFilter `json:"filters,omitempty"`
}

// MetricQueryResult holds every bucket of every group. A null value is a ratio or derived metric
// with no value for that bucket, never zero. Labels names each unsaved measure of the request.
//
//contract:wire
type MetricQueryResult struct {
	Metrics    []string          `json:"metrics"`
	Dimensions []string          `json:"dimensions"`
	Rows       []MetricRow       `json:"rows"`
	Previous   []MetricRow       `json:"previous,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// ExploreRecordType is one record type a question can read, with the measures and dimensions it offers.
// Every record type offers a count.
//
//contract:wire
type ExploreRecordType struct {
	RecordType  string             `json:"record_type"`
	Label       string             `json:"label"`
	Description string             `json:"description,omitempty"`
	TimeField   string             `json:"time_field"`
	Measures    []ExploreMeasure   `json:"measures"`
	Dimensions  []ExploreDimension `json:"dimensions"`
}

// ExploreMeasure is one measure a question can ask for: its aggregate, and the field when it has one.
//
//contract:wire
type ExploreMeasure struct {
	Key    string `json:"key"`
	Agg    string `json:"agg"`
	Field  string `json:"field,omitempty"`
	Label  string `json:"label"`
	Format string `json:"format,omitempty"`
}

// ExploreDimension is one category a question can group by. A path through Link fields has Hops set to
// the number of links it follows and Group naming them ("Supplier ›"); a field of the record type itself
// has zero hops and no group.
//
//contract:wire
type ExploreDimension struct {
	Path  string `json:"path"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Hops  int    `json:"hops"`
	Group string `json:"group,omitempty"`
}

// MetricRow is one bucket of one group. Values follow the result's metrics: each entry is a decimal
// string, or null when the metric has no value for the bucket. Label is set only on the Other group,
// whose Group is "__other__". Labels follows Group when a dimension names a record type: each entry is that
// record's title for the group value, empty when there is none.
//
//contract:wire
type MetricRow struct {
	Bucket string   `json:"bucket"`
	Group  []string `json:"group"`
	Label  string   `json:"label,omitempty"`
	Labels []string `json:"labels,omitempty"`
	//contract:json
	Values []json.RawMessage `json:"values"`
}

// MetricsMeta states how fresh a metric result is.
//
//contract:wire
type MetricsMeta struct {
	AsOf           string          `json:"as_of"`
	Tier           string          `json:"tier"`
	SourceSequence int64           `json:"source_sequence"`
	History        *MetricsHistory `json:"history,omitempty"`
}

// MetricsHistory says whether the history a query reads is still being rebuilt from source records.
//
//contract:wire
type MetricsHistory struct {
	State    string  `json:"state"`    // "building" while a rebuild of any model read is queued or running, else "ready"
	Progress float64 `json:"progress"` // 0 to 1: the least progress across the models being rebuilt; 1 when ready
}

// MetricDrillRequest names one simple metric, or one unsaved measure, and the group and business days to
// list records for. Group maps a dimension to its value. Cursor is the next_cursor of the page before.
//
//contract:wire
type MetricDrillRequest struct {
	Metric  string              `json:"metric,omitempty"`
	Measure *MetricAdHocMeasure `json:"measure,omitempty"`
	From    string              `json:"from"`
	To      string              `json:"to"`
	Group   map[string]string   `json:"group,omitempty"`
	Cursor  string              `json:"cursor,omitempty"`
}

// MetricDrillResult is the record list behind a number: the record type, the filters that list its records
// in the record list, and one page of the records. Total is the number the records add up to. The
// records apply the caller's own record scope. ListOpenable is false when the record list cannot show
// the same records (a group through a path or a line, or a private module record type). NextCursor is
// empty on the last page.
//
//contract:wire
type MetricDrillResult struct {
	Doctype      string              `json:"doctype"`
	Filters      []MetricFilter      `json:"filters"`
	ListOpenable bool                `json:"list_openable"`
	Total        string              `json:"total"`
	Rows         []MetricDrillRecord `json:"rows"`
	NextCursor   string              `json:"next_cursor,omitempty"`
}

// MetricDrillRecord is one record behind a drill number: its name, its title, its business time and its
// value as an exact decimal string.
//
//contract:wire
type MetricDrillRecord struct {
	Name       string `json:"name"`
	Title      string `json:"title"`
	OccurredAt string `json:"occurred_at"`
	Value      string `json:"value"`
}
