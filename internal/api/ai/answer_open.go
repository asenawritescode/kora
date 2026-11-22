package ai

import (
	"encoding/json"
	"errors"

	"github.com/asenawritescode/kora/contract"
)

// metricKindSimple is the kind of a metric that is one aggregate of one record type's field: the only kind Insights
// can express as an unsaved measure.
const metricKindSimple = "simple"

// openQuestion is the Insights question a card's "Open in Insights" button repeats, in the shape Insights reads. It is
// nil when Insights cannot express the question: a ratio, derived, balance or cumulative metric, or a metric that
// cannot be read. Measure questions keep their own saved form.
func (t *answerTurn) openQuestion(q queryAnswer) json.RawMessage {
	if len(q.request.Metrics) == 0 {
		raw, ok := savedQuestionOf(q)
		if !ok {
			return nil
		}
		return raw
	}
	def, err := t.describe(q.request.Metrics[0])
	if err != nil {
		return nil
	}
	raw, ok := exploreQuestionOf(q, def)
	if !ok {
		return nil
	}
	return raw
}

// describe reads the definition of one metric, with its measure and filters, as the person.
func (t *answerTurn) describe(name string) (contract.MetricDefinition, error) {
	resp := t.run(MetricsRouteDescribe, []byte(name))
	if resp.Status != 200 {
		return contract.MetricDefinition{}, errors.New(refusalMessage(resp))
	}
	var env struct {
		Data contract.MetricDefinition `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return contract.MetricDefinition{}, errors.New("the metric definition could not be read")
	}
	return env.Data, nil
}

// exploreQuestionOf maps a one-metric question of a simple metric to the unsaved measure Insights asks: the metric's
// record type, aggregate and field, with the metric's filters and the question's own filters, the same grouping,
// period and comparison. It reports false for any other kind of metric or question.
func exploreQuestionOf(q queryAnswer, def contract.MetricDefinition) (json.RawMessage, bool) {
	r := q.request
	if def.Type != metricKindSimple || def.Measure == nil || def.Model == "" {
		return nil, false
	}
	if len(r.Metrics) != 1 || len(r.Measures) != 0 || len(r.Dimensions) > 1 {
		return nil, false
	}
	filters := append(append([]contract.MetricFilter{}, def.Filters...), r.Filters...)
	measure := contract.MetricAdHocMeasure{RecordType: def.Model, Agg: def.Measure.Agg, Field: def.Measure.Field, Filters: filters}
	saved := contract.SavedQuestion{AdHoc: &measure, By: r.Dimensions, Grain: r.Grain, Period: periodPreset(r),
		From: r.From, To: r.To, Compare: r.Compare, Top: r.Top}
	raw, err := json.Marshal(saved)
	return raw, err == nil
}
