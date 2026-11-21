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
}

// SavedReport is one saved Explore report a caller may read. Visibility is "shop" (every caller who can
// read its record types sees it) or "private" (only its owner sees it). A broken report names the
// missing piece in BrokenReason; BrokenAt is when it broke.
//
//contract:wire
type SavedReport struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Visibility   string        `json:"visibility"`
	Owner        string        `json:"owner"`
	Question     SavedQuestion `json:"question"`
	RecordTypes  []string      `json:"record_types"`
	BrokenReason string        `json:"broken_reason,omitempty"`
	BrokenAt     string        `json:"broken_at,omitempty"`
	Modified     string        `json:"modified"`
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
