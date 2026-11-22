package ai

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/asenawritescode/kora/contract"
)

// keyOrderedItems are eight item groups in key order, as the engine returns them; the top seller is last.
func keyOrderedItems(values ...string) []contract.MetricRow {
	names := []string{"Maize", "Sugar", "Cooking Oil", "Bread", "Milk", "Tea", "Rice", "Bar Soap"}
	rows := make([]contract.MetricRow, 0, len(names))
	for i, name := range names {
		raw := json.RawMessage(`null`)
		if i < len(values) && values[i] != "" {
			raw = json.RawMessage(`"` + values[i] + `"`)
		}
		rows = append(rows, contract.MetricRow{Label: name, Group: []string{name}, Values: []json.RawMessage{raw}})
	}
	return rows
}

func TestTableCardKeepsTheBestGroupsOfATopN(t *testing.T) {
	tests := []struct {
		name   string
		top    int
		values []string
		want   []string
		total  int
	}{
		{name: "top ten ranks by value before the five-row cut", top: 10,
			values: []string{"14", "18", "18", "15", "19", "14", "16", "33"},
			want:   []string{"Bar Soap · 33", "Milk · 19", "Sugar · 18", "Cooking Oil · 18", "Rice · 16"}, total: 8},
		{name: "a group without a value ranks last", top: 10,
			values: []string{"", "18", "18", "15", "19", "14", "16", "33"},
			want:   []string{"Bar Soap · 33", "Milk · 19", "Sugar · 18", "Cooking Oil · 18", "Rice · 16"}, total: 8},
		{name: "without top the rows keep their order", top: 0,
			values: []string{"14", "18", "18", "15", "19", "14", "16", "33"},
			want:   []string{"Maize · 14", "Sugar · 18", "Cooking Oil · 18", "Bread · 15", "Milk · 19"}, total: 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := queryAnswer{
				request: contract.MetricQueryRequest{Metrics: []string{"items_sold"}, Dimensions: []string{"items.product"}, Top: tc.top},
				result:  contract.MetricQueryResult{Metrics: []string{"items_sold"}, Rows: keyOrderedItems(tc.values...)},
			}
			var card contract.AnswerCard
			fillTable(&card, q, &metricsCatalog{}, "KES")
			got := make([]string, 0, len(card.Rows))
			for _, r := range card.Rows {
				got = append(got, r.Label+" · "+r.Value)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rows = %v, want %v", got, tc.want)
			}
			if tc.total > len(card.Rows) && (card.Meta.Shown != 5 || card.Meta.Total != tc.total) {
				t.Fatalf("shown/total = %d/%d, want 5/%d", card.Meta.Shown, card.Meta.Total, tc.total)
			}
		})
	}
}
