package ai

import (
	"encoding/json"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

func TestExploreQuestionOf(t *testing.T) {
	paid := contract.MetricFilter{Field: "status", Op: "=", Value: json.RawMessage(`"Paid"`)}
	region := contract.MetricFilter{Field: "town", Op: "=", Value: json.RawMessage(`"Nairobi"`)}
	simple := contract.MetricDefinition{Type: "simple", Model: "Sale", Measure: &contract.MetricMeasure{Agg: "sum", Field: "base_total"}, Filters: []contract.MetricFilter{paid}}
	tests := []struct {
		name    string
		def     contract.MetricDefinition
		request contract.MetricQueryRequest
		want    bool
		check   func(t *testing.T, got contract.SavedQuestion)
	}{
		{
			name:    "simple metric maps to an unsaved measure with merged filters",
			def:     simple,
			request: contract.MetricQueryRequest{Metrics: []string{"sales"}, Dimensions: []string{"status"}, Filters: []contract.MetricFilter{region}, Period: &contract.MetricPeriod{Preset: "last_week"}, Compare: "week_before", Top: 5},
			want:    true,
			check: func(t *testing.T, got contract.SavedQuestion) {
				if got.AdHoc == nil || got.AdHoc.RecordType != "Sale" || got.AdHoc.Agg != "sum" || got.AdHoc.Field != "base_total" {
					t.Fatalf("ad-hoc measure = %#v", got.AdHoc)
				}
				if len(got.AdHoc.Filters) != 2 || got.AdHoc.Filters[0].Field != "status" || got.AdHoc.Filters[1].Field != "town" {
					t.Fatalf("filters = %#v, want metric then request filters", got.AdHoc.Filters)
				}
				if got.Period != "last_week" || got.Compare != "week_before" || got.Top != 5 || len(got.By) != 1 || got.By[0] != "status" {
					t.Fatalf("question = %#v", got)
				}
				if got.Metric != "" || got.Measure != "" {
					t.Fatalf("question still names a metric: %#v", got)
				}
			},
		},
		{name: "ratio metric is left out", def: contract.MetricDefinition{Type: "ratio", Model: "Sale", Numerator: "a", Denominator: "b"}, request: contract.MetricQueryRequest{Metrics: []string{"rate"}}},
		{name: "balance metric is left out", def: contract.MetricDefinition{Type: "balance", Model: "Sale", Measure: &contract.MetricMeasure{Agg: "count"}}, request: contract.MetricQueryRequest{Metrics: []string{"open"}}},
		{name: "two dimensions are left out", def: simple, request: contract.MetricQueryRequest{Metrics: []string{"sales"}, Dimensions: []string{"status", "town"}}},
		{name: "two metrics are left out", def: simple, request: contract.MetricQueryRequest{Metrics: []string{"sales", "credit"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, ok := exploreQuestionOf(queryAnswer{request: tc.request}, tc.def)
			if ok != tc.want {
				t.Fatalf("exploreQuestionOf ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				if raw != nil {
					t.Fatalf("question = %s, want none", raw)
				}
				return
			}
			var got contract.SavedQuestion
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			tc.check(t, got)
		})
	}
}

func TestActionsOmitOpenWhenInsightsCannotExpressIt(t *testing.T) {
	q := queryAnswer{request: contract.MetricQueryRequest{Metrics: []string{"rate"}}}
	for _, tc := range []struct {
		name     string
		open     json.RawMessage
		wantOpen bool
	}{
		{name: "expressible question keeps the button", open: json.RawMessage(`{"ad_hoc":{}}`), wantOpen: true},
		{name: "inexpressible question has no button", open: nil, wantOpen: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := false
			for _, a := range actionsOf(q, cardPage{}, "Sales", tc.open) {
				found = found || a.Kind == "open_in_insights"
			}
			if found != tc.wantOpen {
				t.Fatalf("open_in_insights present = %v, want %v", found, tc.wantOpen)
			}
		})
	}
}

func TestUnitOf(t *testing.T) {
	for format, want := range map[string]string{"currency": "currency", "percent": "percent", "number": "number", "": "number"} {
		if got := unitOf(format); got != want {
			t.Fatalf("unitOf(%q) = %q, want %q", format, got, want)
		}
	}
}
