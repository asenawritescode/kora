package ai

import (
	"strings"
	"testing"
)

func call(name string) map[string]any {
	return map[string]any{"id": name, "function": map[string]any{"name": name, "arguments": "{}"}}
}

func fn(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}

func TestWindowAllows(t *testing.T) {
	tests := []struct {
		name  string
		calls []any
		want  bool
	}{
		{name: "one write preview", calls: []any{call(toolCreateRecord)}, want: true},
		{name: "several write previews", calls: []any{call(toolCreateRecord), call(toolUpdateRecord), call(toolRunAction)}, want: true},
		{name: "a read tool is refused", calls: []any{call("find_records")}, want: false},
		{name: "a write preview with a read is refused", calls: []any{call(toolCreateRecord), call("find_records")}, want: false},
		{name: "confirm is not a window tool", calls: []any{call(toolConfirmWrite)}, want: false},
		{name: "no calls is not a window round", calls: nil, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := windowAllows(tc.calls); got != tc.want {
				t.Fatalf("windowAllows() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWindowFunctionsOffersWritePreviewsOnly(t *testing.T) {
	got := windowFunctions([]map[string]any{fn("find_records"), fn(toolCreateRecord), fn(toolUpdateRecord), fn(toolRunAction), fn(toolConfirmWrite)})
	var names []string
	for _, f := range got {
		names = append(names, functionName(f))
	}
	if joined := strings.Join(names, ","); joined != "create_record,update_record,run_action" {
		t.Fatalf("window offers %q", joined)
	}
}

func TestWindowEndsOnAReadOrAStalledRound(t *testing.T) {
	tests := []struct {
		name     string
		previews int      // previews already open before the round
		added    int      // previews the round added
		calls    []any    // the round's calls
		wantEnd  []string // which end rule fires
	}{
		{name: "no window yet: nothing ends", previews: 0, added: 0, calls: []any{call("find_records")}},
		{name: "first preview opens the window", previews: 0, added: 1, calls: []any{call(toolCreateRecord)}},
		{name: "another write preview keeps it open", previews: 1, added: 1, calls: []any{call(toolCreateRecord)}},
		{name: "a read tool attempt ends it before it runs", previews: 2, added: 0, calls: []any{call("find_records")}, wantEnd: []string{"closed"}},
		{name: "a write that does not preview ends it", previews: 2, added: 0, calls: []any{call(toolCreateRecord)}, wantEnd: []string{"ended"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := &answerTurn{}
			for i := 0; i < tc.previews; i++ {
				turn.previews = append(turn.previews, previewCard{WriteID: "w", Summary: "s"})
			}
			// A refused round never runs, so only a round that runs can end the window by adding no preview.
			closed := turn.windowClosedBy(tc.calls)
			turn.previews = append(turn.previews, make([]previewCard, tc.added)...)
			ended := !closed && turn.windowEndedAt(tc.previews)
			var got []string
			if closed {
				got = append(got, "closed")
			}
			if ended {
				got = append(got, "ended")
			}
			if strings.Join(got, ",") != strings.Join(tc.wantEnd, ",") {
				t.Fatalf("end rules = %v, want %v", got, tc.wantEnd)
			}
		})
	}
}

func TestProviderBodyOffersToolsOnlyWhenThereAreTools(t *testing.T) {
	tests := []struct {
		name      string
		functions []map[string]any
		wantTools bool
	}{
		{name: "with tools", functions: []map[string]any{fn("create_record")}, wantTools: true},
		{name: "without tools", functions: nil, wantTools: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := providerBody("m", nil, tc.functions, 100)
			_, has := body["tools"]
			if has != tc.wantTools || body["parallel_tool_calls"] != false || body["max_tokens"] != 100 {
				t.Fatalf("body = %v", body)
			}
		})
	}
}

// TestWindowTextEndsTheTurnWithItsCard: a model text reply while previews are open ends the turn with their card; the
// text is not shown instead. Three previews give one card listing all three.
func TestWindowTextEndsTheTurnWithItsCard(t *testing.T) {
	turn := &answerTurn{}
	for _, name := range []string{"Tea", "Milk", "Bread"} {
		turn.previews = append(turn.previews, previewCard{WriteID: "w-" + name, Summary: "create Item (item_name " + name + ")"})
	}
	if !turn.windowOpen() {
		t.Fatal("open previews must keep the window open, so a text reply ends the turn with the card")
	}
	got := previewsText(turn.previews)
	if !strings.HasPrefix(got, "Ready to save 3 writes:") || !strings.HasSuffix(got, "Save all?") {
		t.Fatalf("card = %q, want one card for three writes", got)
	}
}

// TestWindowCapEndsTheTurnWithItsCard: at the round cap an open window ends with its card; with no preview the cap
// keeps the stop reply.
func TestWindowCapEndsTheTurnWithItsCard(t *testing.T) {
	tests := []struct {
		name     string
		previews int
		wantCard bool
	}{
		{name: "open window at the cap ends with its card", previews: 2, wantCard: true},
		{name: "no window at the cap keeps the stop reply", previews: 0, wantCard: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := &answerTurn{}
			for i := 0; i < tc.previews; i++ {
				turn.previews = append(turn.previews, previewCard{WriteID: "w", Summary: "s"})
			}
			if got := turn.windowOpen(); got != tc.wantCard {
				t.Fatalf("windowOpen() at the cap = %v, want %v", got, tc.wantCard)
			}
		})
	}
}
