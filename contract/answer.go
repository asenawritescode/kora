package contract

import "encoding/json"

// ChoiceQuestion is a clarifying question Zana asks before it answers: one question with two to four choices,
// most common first.
//
//contract:wire
type ChoiceQuestion struct {
	Question string         `json:"question"`
	Choices  []ChoiceOption `json:"choices"`
}

// ChoiceOption is one answer a person can pick: a short label and a one-line hint.
//
//contract:wire
type ChoiceOption struct {
	Label string `json:"label"`
	Hint  string `json:"hint"`
}

// AnswerCard is one answer Zana gives: a number or a short table drawn by the server from a metrics result. The
// reply sentence never adds a figure the card lacks.
//
//contract:wire
type AnswerCard struct {
	Kind     string `json:"kind"`
	Label    string `json:"label"`
	Verified bool   `json:"verified"`
	// RecordType is the record type the question reads, and RecordTypeLabel its catalog label ("records" when unknown).
	RecordType      string `json:"record_type,omitempty"`
	RecordTypeLabel string `json:"record_type_label,omitempty"`
	// Unit is how Value, Previous and Rows print: "currency" (with Currency, the site code), "percent" or "number".
	Unit     string `json:"unit,omitempty"`
	Currency string `json:"currency,omitempty"`
	// Progress is set on a "building" card: the share of the history already built, from 0 to 1.
	Progress *float64 `json:"progress,omitempty"`
	// ScopeNote is set when the caller reads only their own records, for example "Only your own sales".
	ScopeNote string `json:"scope_note,omitempty"`
	// Value, Change and Previous are set on a number card; Change is "▲ 8.4% on the week before" or "—".
	Value    string `json:"value,omitempty"`
	Change   string `json:"change,omitempty"`
	Previous string `json:"previous,omitempty"`
	// Sparkline is one value per day of the period on a number card, from a day-grain read of the same question.
	// It is set only for periods of 31 days or fewer.
	Sparkline []string `json:"sparkline,omitempty"`
	// Rows are set on a table card: up to five rows.
	Rows    []AnswerRow    `json:"rows,omitempty"`
	Meta    AnswerMeta     `json:"meta"`
	Actions []AnswerAction `json:"actions,omitempty"`
}

// AnswerRow is one row of a table card: a group or bucket, and its value as printed.
//
//contract:wire
type AnswerRow struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// AnswerMeta is what the card says about its numbers: the exact dates, the comparison, the business clock and
// how fresh the data is. Each entry is printed as the card shows it.
//
//contract:wire
type AnswerMeta struct {
	Period   string `json:"period"`
	Compared string `json:"compared,omitempty"`
	Zone     string `json:"zone"`
	// TimeZone is the tenant's IANA zone name beside the display label in Zone.
	TimeZone string `json:"time_zone,omitempty"`
	DayEnds  string `json:"day_ends"`
	AsOf     string `json:"as_of"`
	// Shown and Total are set on a table that cuts its rows: "Top 5 of 12".
	Shown int `json:"shown,omitempty"`
	Total int `json:"total,omitempty"`
}

// AnswerAction is a button on a card. Question is the request the button repeats: a metrics query, a drill, or
// the question a saved report or pin keeps.
//
//contract:wire
type AnswerAction struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	//contract:json
	Question json.RawMessage `json:"question"`
	// Visibility lists the report visibilities the save action may choose: "shop" only for the administrator.
	Visibility []string `json:"visibility,omitempty"`
}
