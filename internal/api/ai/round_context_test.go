package ai

import "testing"

func TestDropOldToolResults(t *testing.T) {
	// round builds one assistant tool-call message and its result.
	round := func(result string) []map[string]any {
		return []map[string]any{
			{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c"}}},
			{"role": "tool", "content": result},
		}
	}
	build := func(results ...string) []map[string]any {
		messages := []map[string]any{{"role": "system", "content": "standing"}, {"role": "user", "content": "add product"}}
		for _, r := range results {
			messages = append(messages, round(r)...)
		}
		return messages
	}
	tests := []struct {
		name     string
		messages []map[string]any
		keep     int
		want     []string // content of each tool message, in order
	}{
		{name: "no rounds", messages: build(), keep: 2, want: nil},
		{name: "fewer rounds than kept", messages: build("a", "b"), keep: 2, want: []string{"a", "b"}},
		{name: "older rounds dropped", messages: build("a", "b", "c", "d"), keep: 2, want: []string{droppedResult, droppedResult, "c", "d"}},
		{name: "keep one", messages: build("a", "b", "c"), keep: 1, want: []string{droppedResult, droppedResult, "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dropOldToolResults(tc.messages, tc.keep)
			var contents []string
			for _, m := range got {
				if m["role"] == "tool" {
					contents = append(contents, m["content"].(string))
				}
			}
			if len(contents) != len(tc.want) {
				t.Fatalf("tool contents = %v, want %v", contents, tc.want)
			}
			for i := range contents {
				if contents[i] != tc.want[i] {
					t.Fatalf("tool contents = %v, want %v", contents, tc.want)
				}
			}
		})
	}
}
