package ai

import "testing"

func TestNamedTypes(t *testing.T) {
	types := []string{"Supplier", "Supplier Bill", "Category"}
	tests := []struct {
		text string
		want int
	}{
		{"List all supplier bills under oumas son", 2},
		{"List all bills", 1},
		{"show suppliers", 1},
		{"which categories sell most?", 1},
		{"Hello I want to create a site", 0},
		{"Frut", 0},
		{"billing is fine, thanks", 0},
	}
	for _, tt := range tests {
		if got := namedTypes(tt.text, types); len(got) != tt.want {
			t.Errorf("namedTypes(%q) = %v, want %d types", tt.text, got, tt.want)
		}
	}
}

func TestRecordTypesOfReadsFindRecordsEnum(t *testing.T) {
	functions := []map[string]any{
		{"type": "function", "function": map[string]any{"name": "ask_choice", "parameters": map[string]any{}}},
		{"type": "function", "function": map[string]any{"name": toolFindRecords, "parameters": map[string]any{
			"properties": map[string]any{"record_type": map[string]any{"enum": []string{"Supplier", "Supplier Bill"}}},
		}}},
	}
	if got := recordTypesOf(functions); len(got) != 2 || got[1] != "Supplier Bill" {
		t.Fatalf("recordTypesOf = %v", got)
	}
	if got := recordTypesOf(nil); got != nil {
		t.Fatalf("recordTypesOf(nil) = %v, want nil", got)
	}
}

func TestLookupFixSendsBackAnUncheckedAnswerOnce(t *testing.T) {
	tests := []struct {
		name    string
		turn    answerTurn
		message string
		reply   string
		wantFix bool
	}{
		{name: "named type and no tool call", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "List all supplier bills", wantFix: true},
		{name: "a tool already ran", turn: answerTurn{recordTypes: []string{"Supplier Bill"}, lookedUp: true}, message: "List all supplier bills"},
		{name: "already sent back once", turn: answerTurn{recordTypes: []string{"Supplier Bill"}, lookupSent: true}, message: "List all supplier bills"},
		{name: "no record type named", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "Hello"},
		{name: "reply about something else", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "List all supplier bills", reply: "Insights is not available for your role."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn := tt.turn
			reply := tt.reply
			if reply == "" {
				reply = "No supplier bills found."
			}
			fix := turn.correction(reply, tt.message)
			if (fix != nil) != tt.wantFix {
				t.Fatalf("fix = %v, want fix %v", fix, tt.wantFix)
			}
			body := map[string]any{"tool_choice": "auto"}
			turn.resend(body, nil)
			if want := map[bool]string{true: "required", false: "auto"}[tt.wantFix]; body["tool_choice"] != want {
				t.Fatalf("tool_choice = %v, want %s", body["tool_choice"], want)
			}
			if tt.wantFix && turn.correction(reply, tt.message) != nil {
				t.Fatal("the reply was sent back a second time")
			}
		})
	}
}
