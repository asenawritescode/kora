package contract

// SavedQuestion is the Explore question a saved report stores: the record type or metric, the measure,
// the "by" dimensions, the grain, the period, the filters, the comparison, the top count and the view.
// A period preset (today, last_week, ...) reruns relative to today; a custom period keeps From and To.
// It never holds numbers; a report reruns its question when it is opened.
//
//contract:wire
type SavedQuestion struct {
	RecordType string         `json:"record_type,omitempty"`
	Metric     string         `json:"metric,omitempty"`
	Measure    string         `json:"measure,omitempty"`
	By         []string       `json:"by,omitempty"`
	Grain      string         `json:"grain,omitempty"`
	Period     string         `json:"period,omitempty"`
	From       string         `json:"from,omitempty"`
	To         string         `json:"to,omitempty"`
	Filters    []MetricFilter `json:"filters,omitempty"`
	Compare    string         `json:"compare,omitempty"`
	Top        int            `json:"top,omitempty"`
	View       string         `json:"view,omitempty"`
	// AdHoc is an unsaved measure of a record type asked directly, with its own filters: a question that names
	// neither a record type nor a metric. Set alone; it never carries RecordType, Metric or Measure.
	AdHoc *MetricAdHocMeasure `json:"ad_hoc,omitempty"`
}

// SavedReportBroken says which piece of a report's question a configuration change removed. Kind is
// missing_record_type, missing_field, missing_metric or invalid_question. RecordType and Field name the
// piece (Field is a field or measure key, or a metric key for a metric question). Label is what the UI
// shows; it falls back to the key because the removed piece has no label left.
//
//contract:wire
type SavedReportBroken struct {
	Kind       string `json:"kind"`
	RecordType string `json:"record_type,omitempty"`
	Field      string `json:"field,omitempty"`
	Label      string `json:"label"`
}

// SavedReport is one saved Explore report a caller may read. Visibility is "shop" (every caller who can
// read its record types sees it) or "private" (only its owner sees it). OwnerName is the owner's full name,
// or the owner's user id when the user has none. Broken is set when a configuration change removed a piece
// of the question; BrokenAt is when it broke.
//
//contract:wire
type SavedReport struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Visibility  string             `json:"visibility"`
	Owner       string             `json:"owner"`
	OwnerName   string             `json:"owner_name"`
	Question    SavedQuestion      `json:"question"`
	RecordTypes []string           `json:"record_types"`
	Broken      *SavedReportBroken `json:"broken,omitempty"`
	BrokenAt    string             `json:"broken_at,omitempty"`
	Modified    string             `json:"modified"`
}

// SavedReportRequest saves a report: its name, who can see it and its question.
//
//contract:wire
type SavedReportRequest struct {
	Name       string        `json:"name"`
	Visibility string        `json:"visibility"`
	Question   SavedQuestion `json:"question"`
}

// SavedReportCopyRequest names the copy of a saved report and who can see it.
//
//contract:wire
type SavedReportCopyRequest struct {
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}
