package ai

import "testing"

func TestGivenIn(t *testing.T) {
	words := "Add product Sugar 1kg, price 140. Also 12.5 kg of salt"
	tests := []struct {
		name  string
		value any
		want  bool
	}{
		{name: "text the person typed", value: "Sugar 1kg", want: true},
		{name: "case does not matter", value: "sugar 1KG", want: true},
		{name: "number the person typed", value: 140, want: true},
		{name: "decimal the person typed", value: 12.5, want: true},
		{name: "invented text", value: "SODA2", want: false},
		{name: "invented number", value: 70, want: false},
		{name: "partial word is not a match", value: "Sugar 1", want: false},
		{name: "digit inside a longer number", value: 14, want: false},
		{name: "empty value is not a suggestion", value: "", want: true},
		{name: "nil is not a suggestion", value: nil, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := givenIn(tc.value, words); got != tc.want {
				t.Fatalf("givenIn(%v) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestMarkSuggested(t *testing.T) {
	words := userWords([]ChatMessage{{Role: "user", Content: "Add product Sugar 1kg"}, {Role: "assistant", Content: "Fanta"}}, "price 140")
	tests := []struct {
		name          string
		preview       map[string]any
		wantSuggested []string
	}{
		{
			name:          "create marks the fields the person did not give",
			preview:       map[string]any{"action": "create", "fields": map[string]any{"item_name": "Sugar 1kg", "price": 140, "cost": 70}},
			wantSuggested: []string{"cost"},
		},
		{
			name:          "update marks new values only",
			preview:       map[string]any{"action": "update", "changes": map[string]any{"price": map[string]any{"old": 100, "new": 140}, "reorder": map[string]any{"old": 1, "new": 10}}},
			wantSuggested: []string{"reorder"},
		},
		{
			name:          "a reply from an earlier turn is not the person's words",
			preview:       map[string]any{"action": "create", "fields": map[string]any{"item_name": "Fanta"}},
			wantSuggested: []string{"item_name"},
		},
		{
			name:          "all given means no suggested map",
			preview:       map[string]any{"action": "create", "fields": map[string]any{"item_name": "Sugar 1kg", "price": 140}},
			wantSuggested: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			markSuggested(tc.preview, words)
			got, _ := tc.preview["suggested"].(map[string]any)
			if len(got) != len(tc.wantSuggested) {
				t.Fatalf("suggested = %v, want fields %v", got, tc.wantSuggested)
			}
			for _, field := range tc.wantSuggested {
				if got[field] != true {
					t.Fatalf("suggested = %v, want %q set to true", got, field)
				}
			}
		})
	}
}
