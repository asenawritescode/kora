package ai

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
)

// maxCardRows is the most rows a table card shows.
const maxCardRows = 5

// maxCards is the most cards one reply carries: the last two queries of the turn.
const maxCards = 2

// cardPage is what a card needs from the request beyond the query results: the site currency, and whether the
// caller is the administrator, who alone may pin or share a question with the shop.
type cardPage struct {
	Currency string
	CanShare bool
}

// cards draws the cards of the turn from its successful queries, the last ones first to keep. Every number on a
// card is printed from a query result; the model never writes a card.
func (t *answerTurn) cards() []contract.AnswerCard {
	queries := t.queries
	if len(queries) > maxCards {
		queries = queries[len(queries)-maxCards:]
	}
	cards := make([]contract.AnswerCard, 0, len(queries)+1)
	for _, q := range queries {
		cards = append(cards, t.cardFor(q, t.page))
	}
	if n := len(t.building); n > 0 {
		cards = append(cards, buildingCard(t.building[n-1]))
	}
	return cards
}

// buildingCard says that a figure is not ready: its history is still building, with the progress. It carries no
// number of the figure.
func buildingCard(b buildingRead) contract.AnswerCard {
	return contract.AnswerCard{
		Kind:   "building",
		Label:  b.label,
		Change: fmt.Sprintf("Still building history: %d%% done", int(b.progress*100)),
		Meta:   metaOf(queryAnswer{meta: b.meta}),
	}
}

// cardFor draws one card: a number when the query returns one value, else a table.
func (t *answerTurn) cardFor(q queryAnswer, page cardPage) contract.AnswerCard {
	cat, err := t.metricsCatalog()
	if err != nil {
		cat = &metricsCatalog{metrics: map[string]contract.MetricSummary{}, types: map[string]contract.ExploreRecordType{}}
	}
	card := contract.AnswerCard{
		Label:    labelOf(q, cat),
		Verified: q.verified,
		Meta:     metaOf(q),
	}
	if q.meta.OwnerOnly {
		card.ScopeNote = "Only your own records"
	}
	if isNumberResult(q.result) {
		card.Kind = "number"
		fillNumber(&card, q, cat, page.Currency)
	} else {
		card.Kind = "table"
		fillTable(&card, q, cat, page.Currency)
	}
	card.Actions = actionsOf(q, cat, page, recordLabelOf(q, cat))
	return card
}

// isNumberResult reports whether a result is one value: one metric, one bucket, no group.
func isNumberResult(res contract.MetricQueryResult) bool {
	return len(res.Metrics) == 1 && len(res.Rows) == 1 && len(res.Rows[0].Group) == 0 && len(res.Rows[0].Values) == 1
}

// labelOf names the metrics a query reads, as the card shows them.
func labelOf(q queryAnswer, cat *metricsCatalog) string {
	if q.saved != nil {
		return q.saved.Name + " (saved by " + q.saved.Owner + ")"
	}
	names := make([]string, 0, len(q.result.Metrics))
	for _, name := range q.result.Metrics {
		if m, ok := cat.metrics[name]; ok && m.Label != "" {
			names = append(names, m.Label)
		} else if label := q.result.Labels[name]; label != "" {
			names = append(names, label)
		} else {
			names = append(names, name)
		}
	}
	return strings.Join(names, " and ")
}

// recordLabelOf names the record type a query reads: the first metric's, else the first measure's.
func recordLabelOf(q queryAnswer, cat *metricsCatalog) string {
	recordType := ""
	if len(q.request.Metrics) > 0 {
		recordType = cat.metrics[q.request.Metrics[0]].Model
	} else if len(q.request.Measures) > 0 {
		recordType = q.request.Measures[0].RecordType
	}
	if rt, ok := cat.types[recordType]; ok && rt.Label != "" {
		return rt.Label
	}
	return "records"
}

// formatOf is the number format of the first metric of a query. An unsaved measure takes the format of its field in
// the record type's catalog, and is a plain number when the catalog names none.
func formatOf(q queryAnswer, cat *metricsCatalog) string {
	if len(q.result.Metrics) > 0 {
		if m, ok := cat.metrics[q.result.Metrics[0]]; ok && m.Format != "" {
			return m.Format
		}
	}
	if len(q.request.Measures) > 0 {
		ms := q.request.Measures[0]
		for _, m := range cat.types[ms.RecordType].Measures {
			if m.Agg == ms.Agg && m.Field == ms.Field && m.Format != "" {
				return m.Format
			}
		}
	}
	return "number"
}

// fillNumber sets the value, the change against the compared period, and the previous value of a number card.
func fillNumber(card *contract.AnswerCard, q queryAnswer, cat *metricsCatalog, currency string) {
	format := formatOf(q, cat)
	cur, curOK := valueOf(q.result.Rows[0].Values[0])
	card.Value = printAmount(cur, curOK, format, currency)
	if len(q.result.Previous) == 0 {
		return
	}
	prev, prevOK := valueOf(q.result.Previous[0].Values[0])
	card.Previous = printAmount(prev, prevOK, format, currency)
	card.Change = changeOf(cur, curOK, prev, prevOK, q.request.Compare)
}

// valueOf reads one result value: a decimal string, or null for no value.
func valueOf(raw json.RawMessage) (decimal.Decimal, bool) {
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil || s == nil {
		return decimal.Zero, false
	}
	v, err := decimal.NewFromString(*s)
	return v, err == nil
}

// printAmount prints one value. Money carries the site currency and whole units; a percent has one decimal; a count
// is a whole number. A value that is not there prints as a dash, never zero.
func printAmount(v decimal.Decimal, ok bool, format, currency string) string {
	if !ok {
		return "—"
	}
	switch format {
	case "currency":
		return strings.TrimSpace(currency + " " + groupDigits(v.Round(0)))
	case "percent":
		return v.Round(1).StringFixed(1) + "%"
	default:
		return groupDigits(v.Round(0))
	}
}

// groupDigits prints a whole number with thousands separators.
func groupDigits(v decimal.Decimal) string {
	s := v.StringFixed(0)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// changeOf is the change against the compared period, one decimal, with an arrow and the name of the period.
func changeOf(cur decimal.Decimal, curOK bool, prev decimal.Decimal, prevOK bool, compare string) string {
	if compare == "" || compare == "none" || !curOK || !prevOK || prev.IsZero() {
		return "—"
	}
	pct := cur.Sub(prev).Div(prev.Abs()).Mul(decimal.NewFromInt(100)).Round(1)
	arrow := "▲"
	if pct.IsNegative() {
		arrow = "▼"
	}
	return fmt.Sprintf("%s %s%% %s", arrow, pct.Abs().StringFixed(1), comparedName(compare))
}

// comparedName names the comparison window in the words the card uses.
func comparedName(compare string) string {
	if compare == "week_before" {
		return "on the week before"
	}
	return "on the period before"
}

// fillTable sets up to five rows of a table card, with the total when it cuts the rows.
func fillTable(card *contract.AnswerCard, q queryAnswer, cat *metricsCatalog, currency string) {
	format := formatOf(q, cat)
	rows := q.result.Rows
	for _, row := range rows {
		for i, raw := range row.Values {
			if len(card.Rows) == maxCardRows {
				break
			}
			v, ok := valueOf(raw)
			label := rowLabel(row)
			if len(q.result.Metrics) > 1 && i < len(q.result.Metrics) {
				label = strings.TrimSpace(label + " · " + q.result.Metrics[i])
			}
			card.Rows = append(card.Rows, contract.AnswerRow{Label: label, Value: printAmount(v, ok, format, currency)})
		}
	}
	if len(rows) > maxCardRows {
		card.Meta.Shown, card.Meta.Total = maxCardRows, len(rows)
	}
}

// rowLabel names one row: its group's title when the record has one, else the group values, else the bucket.
func rowLabel(row contract.MetricRow) string {
	if row.Label != "" {
		return row.Label
	}
	if len(row.Labels) > 0 && row.Labels[0] != "" {
		return row.Labels[0]
	}
	if len(row.Group) > 0 {
		return strings.Join(row.Group, " · ")
	}
	return row.Bucket
}

// metaOf prints the exact dates, the comparison, the business clock and the freshness of a result.
func metaOf(q queryAnswer) contract.AnswerMeta {
	m := q.meta
	out := contract.AnswerMeta{
		Period:  spanText(m.Period.From, m.Period.To),
		Zone:    path.Base(m.TimeZone) + " time",
		DayEnds: "days end " + m.DayEnds,
		AsOf:    "Data as of " + asOfClock(m.AsOf, m.TimeZone),
	}
	if m.Previous != nil {
		out.Compared = spanText(m.Previous.From, m.Previous.To)
	}
	return out
}

// spanText prints an inclusive span of business dates as "9 Nov – 15 Nov 2026".
func spanText(from, to string) string {
	f, errF := time.Parse(time.DateOnly, from)
	t, errT := time.Parse(time.DateOnly, to)
	if errF != nil || errT != nil {
		return ""
	}
	return fmt.Sprintf("%d %s – %d %s %d", f.Day(), f.Format("Jan"), t.Day(), t.Format("Jan"), t.Year())
}

// asOfClock prints the time of a result in the tenant's zone, HH:MM.
func asOfClock(asOf, zone string) string {
	at, err := time.Parse(time.RFC3339, asOf)
	if err != nil {
		return ""
	}
	if loc, err := time.LoadLocation(zone); err == nil {
		at = at.In(loc)
	}
	return at.Format("15:04")
}

// actionsOf are the buttons of a card: the question in Explore, the records behind a number, and the save and pin
// actions for a question a saved report can keep. A caller who may not use a button does not get it.
func actionsOf(q queryAnswer, cat *metricsCatalog, page cardPage, recordLabel string) []contract.AnswerAction {
	actions := []contract.AnswerAction{{Kind: "open_in_insights", Label: "Open in Insights", Question: q.question}}
	if isNumberResult(q.result) {
		if drill, ok := drillRequestOf(q); ok {
			actions = append(actions, contract.AnswerAction{Kind: "see_records", Label: "See the " + strings.ToLower(recordLabel), Question: drill})
		}
	}
	if saved, ok := savedQuestionOf(q); ok {
		visibility := []string{"private"}
		if page.CanShare {
			visibility = []string{"shop", "private"}
		}
		actions = append(actions, contract.AnswerAction{Kind: "save_as_report", Label: "Save as report", Question: saved, Visibility: visibility})
		if page.CanShare {
			actions = append(actions, contract.AnswerAction{Kind: "pin_to_dashboard", Label: "Pin to dashboard", Question: saved})
		}
	}
	return actions
}

// drillRequestOf is the drill behind a number: the same metric and business days, without a group.
func drillRequestOf(q queryAnswer) (json.RawMessage, bool) {
	if len(q.request.Metrics) != 1 || len(q.request.Measures) != 0 || len(q.request.Dimensions) != 0 {
		return nil, false
	}
	raw, err := json.Marshal(contract.MetricDrillRequest{Metric: q.request.Metrics[0], From: q.meta.Period.From, To: q.meta.Period.To})
	return raw, err == nil
}

// savedQuestionOf is the question a saved report keeps for a query on one named metric. A query with unsaved
// measures has no saved form, so it offers no save or pin.
func savedQuestionOf(q queryAnswer) (json.RawMessage, bool) {
	r := q.request
	if len(r.Metrics) != 1 || len(r.Measures) != 0 || len(r.Dimensions) > 1 {
		return nil, false
	}
	saved := contract.SavedQuestion{
		Metric:  r.Metrics[0],
		By:      r.Dimensions,
		Grain:   r.Grain,
		Period:  periodPreset(r),
		From:    r.From,
		To:      r.To,
		Filters: r.Filters,
		Compare: r.Compare,
		Top:     r.Top,
	}
	raw, err := json.Marshal(saved)
	return raw, err == nil
}

// periodPreset is the preset a query named, or empty for a custom range.
func periodPreset(r contract.MetricQueryRequest) string {
	if r.Period == nil {
		return ""
	}
	return r.Period.Preset
}
