package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
)

// MetricsRoute names one metrics read the chat may run. Each route is served by the API handler of the same
// name, so the chat applies the grants, scope and business clock of the person it answers.
type MetricsRoute string

// The metrics routes the chat reads through.
const (
	MetricsRouteMetrics MetricsRoute = "metrics" // GET /metrics: the metrics the caller may read
	MetricsRouteExplore MetricsRoute = "explore" // GET /explore/catalog: the record types the caller may question
	MetricsRouteReports MetricsRoute = "reports" // GET /explore/reports: the saved reports the caller may read
	MetricsRouteQuery   MetricsRoute = "query"   // POST /metrics/query
	MetricsRouteDrill   MetricsRoute = "drill"   // POST /metrics/drill
)

// MetricsResponse is the API's answer to one metrics read: its HTTP status and its JSON body.
type MetricsResponse struct {
	Status int
	Body   []byte
}

// MetricsRunner runs one metrics read as the person whose turn it is. The chat never sees SQL.
type MetricsRunner func(route MetricsRoute, body []byte) MetricsResponse

// ChatTurn is one chat request: the tenant transaction, the site's catalog, the site, the verified user and the
// runner for the metrics tools.
type ChatTurn struct {
	Tx       *orm.TxManager
	Registry *doctype.Registry
	Site     string
	User     string
	Metrics  MetricsRunner
}

// Tool names of the metrics tools.
const (
	toolListMetrics  = "list_metrics"
	toolQueryMetrics = "query_metrics"
	toolDrillMetrics = "drill_metrics"
	toolAskChoice    = "ask_choice"
)

// answerTurn is what one chat turn has read: every successful metrics read, the catalog those reads named, and
// the clarifying question when Zana asked one. The answer cards and the number check are built from it.
type answerTurn struct {
	run     MetricsRunner
	page    cardPage
	queries []queryAnswer
	drills  []drillAnswer
	choice  *contract.ChoiceQuestion
	// building is every read whose history was still building: it gives no number, only its progress.
	building []buildingRead
	catalog  *metricsCatalog
	// corrected is set once the reply was sent back for a wrong number; it is sent back at most once.
	corrected bool
}

// queryAnswer is one successful query: the question as the model sent it, the result, its meta and whether
// every number in it is a checked metric.
type queryAnswer struct {
	question json.RawMessage
	request  contract.MetricQueryRequest
	result   contract.MetricQueryResult
	meta     contract.MetricsMeta
	verified bool
	// saved is the saved report the question came from, nil for a question the model asked itself.
	saved *contract.SavedReport
}

// buildingRead is a read the server refused to answer as numbers because a fact model it reads is still rebuilding.
type buildingRead struct {
	label    string
	progress float64
	meta     contract.MetricsMeta
}

// drillAnswer is one successful drill: the request and the records behind the number.
type drillAnswer struct {
	request contract.MetricDrillRequest
	result  contract.MetricDrillResult
}

// metricsCatalog is the metric and record-type catalog of one turn, read once through the API.
type metricsCatalog struct {
	metrics map[string]contract.MetricSummary
	types   map[string]contract.ExploreRecordType
}

// executeMetricsTool runs one metrics tool for the turn. A missing runner refuses without reading anything.
func executeMetricsTool(turn *answerTurn, name string, args map[string]any) string {
	if turn == nil || turn.run == nil {
		return "Error: metrics are not available in this session."
	}
	switch name {
	case toolListMetrics:
		return turn.list()
	case toolQueryMetrics:
		return turn.query(args)
	case toolDrillMetrics:
		return turn.drill(args)
	default:
		return turn.askChoice(args)
	}
}

// list answers list_metrics with the metrics, the record types and the saved reports the caller may use.
func (t *answerTurn) list() string {
	parts := make([]json.RawMessage, 0, 3)
	for _, route := range []MetricsRoute{MetricsRouteMetrics, MetricsRouteExplore, MetricsRouteReports} {
		data, err := t.data(route)
		if err != nil {
			return toolError(err)
		}
		parts = append(parts, data)
	}
	out, err := json.Marshal(map[string]json.RawMessage{"metrics": parts[0], "record_types": parts[1], "saved_reports": parts[2]})
	if err != nil {
		return toolError(err)
	}
	return string(out)
}

// query answers query_metrics. It runs the query through the API and keeps the result as evidence for the reply.
// A query with saved_report runs the stored question of that report instead.
func (t *answerTurn) query(args map[string]any) string {
	if id, _ := args["saved_report"].(string); id != "" {
		return t.savedQuery(id)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return "Error: query_metrics arguments are not valid JSON."
	}
	var req contract.MetricQueryRequest
	if decodeErr := json.Unmarshal(body, &req); decodeErr != nil {
		return "Error: query_metrics arguments do not match the query shape."
	}
	return t.answerQuery(body, req, nil)
}

// savedQuery runs the question of a saved report the person may read, as the person. Its figures are not checked.
func (t *answerTurn) savedQuery(id string) string {
	data, err := t.data(MetricsRouteReports)
	if err != nil {
		return toolError(err)
	}
	var reports []contract.SavedReport
	if decodeErr := json.Unmarshal(data, &reports); decodeErr != nil {
		return "Error: the saved reports could not be read."
	}
	for i := range reports {
		if reports[i].ID != id {
			continue
		}
		req, ok := requestOfQuestion(reports[i].Question)
		if !ok {
			return "Error: this saved report does not hold a metric question that can be run."
		}
		body, err := json.Marshal(req)
		if err != nil {
			return toolError(err)
		}
		return t.answerQuery(body, req, &reports[i])
	}
	return "Error: no saved report with that id is visible to you."
}

// requestOfQuestion is the metrics query a saved question asks: its metric, grouping, period and filters.
func requestOfQuestion(q contract.SavedQuestion) (contract.MetricQueryRequest, bool) {
	if q.Metric == "" {
		return contract.MetricQueryRequest{}, false
	}
	req := contract.MetricQueryRequest{Metrics: []string{q.Metric}, Dimensions: q.By, Filters: q.Filters,
		From: q.From, To: q.To, Grain: q.Grain, Compare: q.Compare, Top: q.Top}
	if q.Period != "" {
		req.Period = &contract.MetricPeriod{Preset: q.Period}
	}
	if req.Grain == "" {
		req.Grain = "range"
	}
	return req, true
}

// answerQuery runs one query and keeps its result as evidence. A saved report's figures are never checked.
func (t *answerTurn) answerQuery(body []byte, req contract.MetricQueryRequest, saved *contract.SavedReport) string {
	resp := t.run(MetricsRouteQuery, body)
	if resp.Status != 200 {
		return toolError(errors.New(refusalMessage(resp)))
	}
	var env struct {
		Data contract.MetricQueryResult `json:"data"`
		Meta contract.MetricsMeta       `json:"meta"`
	}
	if decodeErr := json.Unmarshal(resp.Body, &env); decodeErr != nil {
		return "Error: the query answer could not be read."
	}
	if h := env.Meta.History; h != nil && h.State == "building" {
		t.building = append(t.building, buildingRead{label: requestLabel(req), progress: h.Progress, meta: env.Meta})
		return fmt.Sprintf("Error: the history behind this figure is still building (%d%% done). Give no number for it; say that the history is still building.", int(h.Progress*100))
	}
	verified := false
	if saved == nil {
		var err error
		if verified, err = t.verified(req); err != nil {
			return toolError(err)
		}
	}
	t.queries = append(t.queries, queryAnswer{question: body, request: req, result: env.Data, meta: env.Meta, verified: verified, saved: saved})
	return string(resp.Body)
}

// requestLabel names what a query asks for, before its result is read: the metric names, or the measure's record type.
func requestLabel(req contract.MetricQueryRequest) string {
	if len(req.Metrics) > 0 {
		return strings.Join(req.Metrics, " and ")
	}
	if len(req.Measures) > 0 {
		return req.Measures[0].RecordType
	}
	return "the figure"
}

// drill answers drill_metrics with the records behind one number, and keeps the total as evidence.
func (t *answerTurn) drill(args map[string]any) string {
	body, err := json.Marshal(args)
	if err != nil {
		return "Error: drill_metrics arguments are not valid JSON."
	}
	var req contract.MetricDrillRequest
	if decodeErr := json.Unmarshal(body, &req); decodeErr != nil {
		return "Error: drill_metrics arguments do not match the drill shape."
	}
	resp := t.run(MetricsRouteDrill, body)
	if resp.Status != 200 {
		return toolError(errors.New(refusalMessage(resp)))
	}
	var env struct {
		Data contract.MetricDrillResult `json:"data"`
	}
	if decodeErr := json.Unmarshal(resp.Body, &env); decodeErr != nil {
		return "Error: the drill answer could not be read."
	}
	t.drills = append(t.drills, drillAnswer{request: req, result: env.Data})
	return string(resp.Body)
}

// askChoice records a clarifying question with two to four choices. The turn ends with the question, so the model
// does not answer before the person picks one.
func (t *answerTurn) askChoice(args map[string]any) string {
	question, _ := args["question"].(string)
	question = strings.TrimSpace(question)
	choices, err := choicesOf(args["choices"])
	if err != nil || question == "" {
		return "Error: ask_choice needs a question and two to four choices, each with a label and a hint."
	}
	t.choice = &contract.ChoiceQuestion{Question: question, Choices: choices}
	return "Shown to the person. Do not answer yet; wait for their choice."
}

// choicesOf decodes and checks the choices of ask_choice: two to four, each with a distinct label.
func choicesOf(raw any) ([]contract.ChoiceOption, error) {
	items, ok := raw.([]any)
	if !ok || len(items) < 2 || len(items) > 4 {
		return nil, fmt.Errorf("choices need two to four entries")
	}
	seen := map[string]bool{}
	out := make([]contract.ChoiceOption, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("choice is not an object")
		}
		label, _ := m["label"].(string)
		hint, _ := m["hint"].(string)
		label, hint = strings.TrimSpace(label), strings.TrimSpace(hint)
		if label == "" || seen[label] {
			return nil, fmt.Errorf("choice label is empty or repeated")
		}
		seen[label] = true
		out = append(out, contract.ChoiceOption{Label: label, Hint: hint})
	}
	return out, nil
}

// verified reports whether a query reads only checked metrics: no unsaved measure, and every named metric verified.
func (t *answerTurn) verified(req contract.MetricQueryRequest) (bool, error) {
	if len(req.Measures) > 0 {
		return false, nil
	}
	cat, err := t.metricsCatalog()
	if err != nil {
		return false, err
	}
	for _, name := range req.Metrics {
		if m, ok := cat.metrics[name]; !ok || !m.Verified {
			return false, nil
		}
	}
	return len(req.Metrics) > 0, nil
}

// metricsCatalog reads the metric and record-type catalog once per turn.
func (t *answerTurn) metricsCatalog() (*metricsCatalog, error) {
	if t.catalog != nil {
		return t.catalog, nil
	}
	var metricList []contract.MetricSummary
	data, err := t.data(MetricsRouteMetrics)
	if err != nil {
		return nil, err
	}
	if decodeErr := json.Unmarshal(data, &metricList); decodeErr != nil {
		return nil, errors.New("the metric list could not be read")
	}
	var types []contract.ExploreRecordType
	data, err = t.data(MetricsRouteExplore)
	if err != nil {
		return nil, err
	}
	if decodeErr := json.Unmarshal(data, &types); decodeErr != nil {
		return nil, errors.New("the record type list could not be read")
	}
	cat := &metricsCatalog{metrics: map[string]contract.MetricSummary{}, types: map[string]contract.ExploreRecordType{}}
	for _, m := range metricList {
		cat.metrics[m.Name] = m
	}
	for _, rt := range types {
		cat.types[rt.RecordType] = rt
	}
	t.catalog = cat
	return cat, nil
}

// data runs one read and returns the data of its answer, or the refusal as an error.
func (t *answerTurn) data(route MetricsRoute) (json.RawMessage, error) {
	resp := t.run(route, nil)
	if resp.Status != 200 {
		return nil, errors.New(refusalMessage(resp))
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if decodeErr := json.Unmarshal(resp.Body, &env); decodeErr != nil || len(env.Data) == 0 {
		return nil, errors.New("the answer could not be read")
	}
	return env.Data, nil
}

// refusalMessage is the message of a refused API answer: its message, never its code.
func refusalMessage(resp MetricsResponse) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &env); err == nil && env.Error.Message != "" {
		return env.Error.Message
	}
	return "the metrics request was refused"
}

// toolError is the text the model reads for a failed tool: it always starts with "Error".
func toolError(err error) string {
	return "Error: " + err.Error()
}

// metricsToolDefs are the function definitions of the metrics tools.
func metricsToolDefs() []map[string]any {
	return []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        toolListMetrics,
				"description": "List the metrics this person may read: checked metrics (verified true) and unchecked ones, the record types their questions can use with its measures and dimensions, and the saved reports they can see. Call this before answering a business number unless the list is already in the conversation.",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        toolQueryMetrics,
				"description": "Get business numbers from checked metrics (or measures of record types), or from a saved report by saved_report id. The only source of numbers in a reply. Periods: set period.preset to today, yesterday, this_week, last_week (previous Monday to Sunday), this_month, last_month, last_7_days, last_30_days, last_3_months, or custom with from and to (YYYY-MM-DD). Grain is day, week, month or range. compare is none, previous_period or week_before. by dimensions and filters use the list_metrics names.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"metrics":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Metric names from list_metrics"},
						"measures":     map[string]any{"type": "array", "description": "Unsaved measures of a record type (never checked)", "items": map[string]any{"type": "object", "properties": map[string]any{"record_type": map[string]any{"type": "string"}, "agg": map[string]any{"type": "string", "enum": []string{"count", "sum", "count_distinct"}}, "field": map[string]any{"type": "string"}}, "required": []string{"record_type", "agg"}}},
						"dimensions":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"period":       map[string]any{"type": "object", "properties": map[string]any{"preset": map[string]any{"type": "string"}}},
						"from":         map[string]any{"type": "string"},
						"to":           map[string]any{"type": "string"},
						"grain":        map[string]any{"type": "string", "enum": []string{"day", "week", "month", "range"}},
						"compare":      map[string]any{"type": "string", "enum": []string{"none", "previous_period", "week_before"}},
						"top":          map[string]any{"type": "integer"},
						"saved_report": map[string]any{"type": "string", "description": "Id of a saved report from list_metrics: runs its stored question instead; its figures are not checked"},
					},
					"required": []string{"grain"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        toolDrillMetrics,
				"description": "List the records behind one number from query_metrics: the same metric (or measure) and the same business days, with its group if any. Returns the total the records add up to.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"metric": map[string]any{"type": "string"},
						"from":   map[string]any{"type": "string"},
						"to":     map[string]any{"type": "string"},
						"group":  map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
						"cursor": map[string]any{"type": "string"},
					},
					"required": []string{"from", "to"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        toolAskChoice,
				"description": "Ask the person one clarifying question when more than one metric or period fits. Give two to four choices, most common first, each with a short hint. Use it instead of guessing; the turn ends and the person picks one.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{"type": "string"},
						"choices": map[string]any{"type": "array", "minItems": 2, "maxItems": 4, "items": map[string]any{
							"type":       "object",
							"properties": map[string]any{"label": map[string]any{"type": "string"}, "hint": map[string]any{"type": "string"}},
							"required":   []string{"label", "hint"},
						}},
					},
					"required": []string{"question", "choices"},
				},
			},
		},
	}
}
