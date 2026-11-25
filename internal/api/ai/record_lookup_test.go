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
		{"supp", 1},
		{"show supp-001", 1},
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
		{name: "a prefix of a type names it", turn: answerTurn{recordTypes: []string{"Supplier", "Supplier Bill"}}, message: "Show supp-001", reply: "My supplier list came back empty.", wantFix: true},
		{name: "a code names no type, reply names one", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "SB-0001?", reply: "Supplier Bill SB-0001 is paid.", wantFix: true},
		{name: "a code and a reply with no record", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "SB-0001?", reply: "Hi"},
		{name: "a code and a reply that quotes a code", turn: answerTurn{recordTypes: []string{"Supplier Bill"}}, message: "bill-001", reply: "It is SB-0003.", wantFix: true},
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

func TestPickedOption(t *testing.T) {
	previous := "No supplier bills found for Mr. Ouma Son.\n\n1. List all supplier bills\n2) Show all suppliers"
	tests := []struct{ message, want string }{
		{"1", "List all supplier bills"},
		{"option 2", "Show all suppliers"},
		{" 2 ", "Show all suppliers"},
		{"3", "3"},
		{"List stock counts", "List stock counts"},
		{"12 bags of sugar", "12 bags of sugar"},
	}
	for _, tt := range tests {
		if got := pickedOption(tt.message, previous); got != tt.want {
			t.Errorf("pickedOption(%q) = %q, want %q", tt.message, got, tt.want)
		}
	}
}

func TestLookupFixReadsAPickedOption(t *testing.T) {
	turn := answerTurn{recordTypes: []string{"Supplier Bill"}, lastReply: "No supplier bills found.\n\n1. List all supplier bills\n2. Show all suppliers"}
	if turn.correction("None found: no supplier bills have been created yet.", "1") == nil {
		t.Fatal("an answer to a picked option was not sent back for a lookup")
	}
}

func TestRecordCodeShape(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"SB-0001", true},
		{"supp-001", true},
		{"BILL-001", true},
		{"hello", false},
		{"12", false},
		{"1-2", false},
		{"a-b", false},
		{"SB0001", false},
		{"SB-0001-2", false},
	}
	for _, tt := range tests {
		if got := recordCode(tt.text).shaped(); got != tt.want {
			t.Errorf("recordCode(%q).shaped() = %v, want %v", tt.text, got, tt.want)
		}
	}
}

func TestCodesInSplitsOnWhitespaceAndTrimsPunctuation(t *testing.T) {
	tests := []struct {
		message string
		want    []recordCode
	}{
		{"Show supp-001 please", []recordCode{"supp-001"}},
		{"SB-0001?", []recordCode{"SB-0001"}},
		{"(BILL-001), and SB-0002.", []recordCode{"BILL-001", "SB-0002"}},
		{"List all bills", nil},
	}
	for _, tt := range tests {
		got := codesIn(tt.message)
		if len(got) != len(tt.want) {
			t.Errorf("codesIn(%q) = %v, want %v", tt.message, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("codesIn(%q) = %v, want %v", tt.message, got, tt.want)
			}
		}
	}
}
