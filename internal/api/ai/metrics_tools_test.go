package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

// fakeMetrics answers the metrics routes from fixed bodies, and records every read it was asked for.
type fakeMetrics struct {
	answers map[MetricsRoute]MetricsResponse
	reads   []MetricsRoute
}

func (f *fakeMetrics) run(route MetricsRoute, _ []byte) MetricsResponse {
	f.reads = append(f.reads, route)
	return f.answers[route]
}

func okBody(data string) MetricsResponse {
	return MetricsResponse{Status: 200, Body: []byte(`{"data":` + data + `}`)}
}

func sampleFake() *fakeMetrics {
	return &fakeMetrics{answers: map[MetricsRoute]MetricsResponse{
		MetricsRouteMetrics: okBody(`[{"name":"sales_total","label":"Sales","verified":true},{"name":"credit_given","label":"Credit","verified":false}]`),
		MetricsRouteExplore: okBody(`[{"record_type":"Sale","label":"Sales","measures":[],"dimensions":[]}]`),
		MetricsRouteReports: okBody(`[]`),
		MetricsRouteQuery:   MetricsResponse{Status: 200, Body: []byte(`{"data":{"metrics":["sales_total"],"dimensions":[],"rows":[]},"meta":{"period":{"preset":"last_week","from":"2026-11-09","to":"2026-11-15"},"time_zone":"Africa/Nairobi","day_ends":"04:00","as_of":"2026-11-21T16:14:00Z"}}`)},
		MetricsRouteDrill:   MetricsResponse{Status: 200, Body: []byte(`{"data":{"doctype":"Sale","filters":[],"list_openable":true,"total":"112480","rows":[]}}`)},
	}}
}

func TestMetricsToolsRunThroughRunnerAndKeepEvidence(t *testing.T) {
	cases := []struct {
		name         string
		tool         string
		args         map[string]any
		wantVerified bool
		wantQueries  int
		wantDrills   int
		wantOK       bool
	}{
		{"checked metric is verified", toolQueryMetrics, map[string]any{"metrics": []any{"sales_total"}, "grain": "range"}, true, 1, 0, true},
		{"unchecked metric is not verified", toolQueryMetrics, map[string]any{"metrics": []any{"credit_given"}, "grain": "range"}, false, 1, 0, true},
		{"unsaved measure is never verified", toolQueryMetrics, map[string]any{"measures": []any{map[string]any{"record_type": "Sale", "agg": "count"}}, "grain": "range"}, false, 1, 0, true},
		{"drill keeps the total as evidence", toolDrillMetrics, map[string]any{"metric": "sales_total", "from": "2026-11-09", "to": "2026-11-15"}, false, 0, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := sampleFake()
			turn := &answerTurn{run: fake.run}
			out := executeMetricsTool(turn, tc.tool, tc.args)
			if strings.HasPrefix(out, "Error") {
				t.Fatalf("tool refused: %s", out)
			}
			if len(turn.queries) != tc.wantQueries || len(turn.drills) != tc.wantDrills {
				t.Fatalf("evidence queries=%d drills=%d, want %d and %d", len(turn.queries), len(turn.drills), tc.wantQueries, tc.wantDrills)
			}
			if tc.wantQueries > 0 && turn.queries[0].verified != tc.wantVerified {
				t.Fatalf("verified = %v, want %v", turn.queries[0].verified, tc.wantVerified)
			}
		})
	}
}

func TestListMetricsReadsEveryCatalogRoute(t *testing.T) {
	fake := sampleFake()
	out := executeMetricsTool(&answerTurn{run: fake.run}, toolListMetrics, nil)
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("list_metrics answer is not JSON: %v", err)
	}
	for _, key := range []string{"metrics", "record_types", "saved_reports"} {
		if _, ok := parsed[key]; !ok {
			t.Fatalf("list_metrics answer lacks %q", key)
		}
	}
	if len(fake.reads) != 3 {
		t.Fatalf("list_metrics reads = %v, want the three catalog routes", fake.reads)
	}
}

func TestMetricsRefusalIsRelayedAsError(t *testing.T) {
	fake := &fakeMetrics{answers: map[MetricsRoute]MetricsResponse{
		MetricsRouteQuery: {Status: 403, Body: []byte(`{"error":{"code":"metrics.forbidden","message":"metrics are not available to field-assigned grants"}}`)},
	}}
	turn := &answerTurn{run: fake.run}
	out := executeMetricsTool(turn, toolQueryMetrics, map[string]any{"metrics": []any{"sales_total"}, "grain": "range"})
	if out != "Error: metrics are not available to field-assigned grants" {
		t.Fatalf("refusal = %q", out)
	}
	if len(turn.queries) != 0 {
		t.Fatal("a refused query became evidence")
	}
}

func TestMetricsToolsRefuseWithoutRunner(t *testing.T) {
	out := executeMetricsTool(&answerTurn{}, toolQueryMetrics, map[string]any{"grain": "range"})
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("query without runner = %q, want an error", out)
	}
}

func TestAskChoiceChecksTheChoices(t *testing.T) {
	cases := []struct {
		name    string
		choices []any
		ok      bool
	}{
		{"two choices", []any{choice("Sales", "Every sale"), choice("Sale count", "Number of sales")}, true},
		{"four choices", []any{choice("a", ""), choice("b", ""), choice("c", ""), choice("d", "")}, true},
		{"one choice", []any{choice("a", "")}, false},
		{"five choices", []any{choice("a", ""), choice("b", ""), choice("c", ""), choice("d", ""), choice("e", "")}, false},
		{"empty label", []any{choice("", ""), choice("b", "")}, false},
		{"repeated label", []any{choice("a", ""), choice("a", "")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			turn := &answerTurn{run: sampleFake().run}
			out := executeMetricsTool(turn, toolAskChoice, map[string]any{"question": "Which sales do you mean?", "choices": tc.choices})
			if got := turn.choice != nil; got != tc.ok {
				t.Fatalf("choice recorded = %v (answer %q), want %v", got, out, tc.ok)
			}
		})
	}
}

func choice(label, hint string) map[string]any {
	return map[string]any{"label": label, "hint": hint}
}
