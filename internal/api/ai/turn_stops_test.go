package ai

import (
	"strings"
	"testing"
	"time"
)

func TestTurnStopsBeforeRound(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		at     time.Time
		want   stopReason
		isStop bool
	}{
		{name: "first round", at: start, isStop: false},
		{name: "inside the budget", at: start.Add(turnTimeBudget - time.Second), isStop: false},
		{name: "budget reached", at: start.Add(turnTimeBudget), want: stopTimeBudget, isStop: true},
		{name: "budget passed", at: start.Add(2 * turnTimeBudget), want: stopTimeBudget, isStop: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := newTurnStops(start).beforeRound(tc.at)
			if ok != tc.isStop || got != tc.want {
				t.Fatalf("beforeRound() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.isStop)
			}
		})
	}
}

func TestTurnStopsStall(t *testing.T) {
	tests := []struct {
		name   string
		rounds [][]string
		want   []bool // stall verdict per round
	}{
		{name: "first call is not a stall", rounds: [][]string{{"find_records::{}"}}, want: []bool{false}},
		{name: "same call twice stalls on the second", rounds: [][]string{{"find_records::{}"}, {"find_records::{}"}}, want: []bool{false, true}},
		{name: "different arguments do not stall", rounds: [][]string{{"find_records::{\"a\":1}"}, {"find_records::{\"a\":2}"}}, want: []bool{false, false}},
		{name: "a call between repeats resets", rounds: [][]string{{"find_records::{}"}, {"get_record::{}"}, {"find_records::{}"}}, want: []bool{false, false, false}},
		{name: "same batch twice stalls", rounds: [][]string{{"a::1", "b::2"}, {"a::1", "b::2"}}, want: []bool{false, true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stops := newTurnStops(time.Now())
			for i, sigs := range tc.rounds {
				if got := stops.stallOn(sigs); got != tc.want[i] {
					t.Fatalf("round %d: stallOn() = %v, want %v", i+1, got, tc.want[i])
				}
			}
		})
	}
}

func TestTurnStopsAfterResults(t *testing.T) {
	malformed := `{"error":{"code":"invalid_arguments","message":"bad"}}`
	missing := `{"error":{"code":"missing_fields","message":"name"}}`
	tests := []struct {
		name    string
		results [][]string // one slice per round, the contents of its tool results
		want    []bool     // stop verdict per round
	}{
		{name: "one malformed call is one repair", results: [][]string{{malformed}}, want: []bool{false}},
		{name: "second malformed call in a row stops", results: [][]string{{malformed}, {malformed}}, want: []bool{false, true}},
		{name: "a success resets the error", results: [][]string{{malformed}, {`{"data":[]}`}, {malformed}}, want: []bool{false, false, false}},
		{name: "different codes do not stop", results: [][]string{{malformed}, {missing}}, want: []bool{false, false}},
		{name: "plain text error is not a code", results: [][]string{{"Error: boom"}, {"Error: boom"}}, want: []bool{false, false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stops := newTurnStops(time.Now())
			for i, contents := range tc.results {
				var results []map[string]any
				for _, c := range contents {
					results = append(results, map[string]any{"role": "tool", "content": c})
				}
				if _, got := stops.afterResults(results); got != tc.want[i] {
					t.Fatalf("round %d: afterResults() stop = %v, want %v", i+1, got, tc.want[i])
				}
			}
		})
	}
}

func TestStopAction(t *testing.T) {
	tests := []struct {
		stop stopReason
		want string
	}{
		{stop: stopMaxRounds, want: "max_rounds_reached"},
		{stop: stopStall, want: "turn_stopped"},
		{stop: stopRepeatError, want: "turn_stopped"},
		{stop: stopTimeBudget, want: "turn_stopped"},
		{stop: stopTokenBudget, want: "turn_stopped"},
	}
	for _, tc := range tests {
		if got := stopAction(tc.stop); got != tc.want {
			t.Errorf("stopAction(%q) = %q, want %q", tc.stop, got, tc.want)
		}
	}
}

func TestTallyToolResults(t *testing.T) {
	long := strings.Repeat("x", 50)
	tests := []struct {
		name       string
		contents   []string
		maxChars   int
		wantErrs   int
		wantCapped bool // the first result was trimmed to maxChars plus its marker
	}{
		{name: "successes are not errors", contents: []string{`{"data":[]}`, "done"}, maxChars: 100},
		{name: "structured error counts as error", contents: []string{`{"error":{"code":"not_found","message":"x"}}`}, maxChars: 100, wantErrs: 1},
		{name: "plain error counts as error", contents: []string{"Error: boom"}, maxChars: 100, wantErrs: 1},
		{name: "long result is capped", contents: []string{long}, maxChars: 20, wantCapped: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var results []map[string]any
			for _, c := range tc.contents {
				results = append(results, map[string]any{"role": "tool", "content": c})
			}
			if errs := tallyToolResults(results, tc.maxChars); errs != tc.wantErrs {
				t.Fatalf("tallyToolResults() = %d errors, want %d", errs, tc.wantErrs)
			}
			got, _ := results[0]["content"].(string)
			if capped := strings.Contains(got, "characters trimmed"); capped != tc.wantCapped {
				t.Fatalf("first result capped = %v, want %v: %q", capped, tc.wantCapped, got)
			}
		})
	}
}

func TestStopReplyBuildsFromTrace(t *testing.T) {
	saved := &turnStops{done: []string{`Created Item "ITM-3".`}}
	tests := []struct {
		name      string
		stop      stopReason
		trace     *turnStops
		wantParts []string
	}{
		{name: "nothing saved, budget ran out", stop: stopMaxRounds, trace: &turnStops{}, wantParts: []string{"Nothing is saved yet.", "needed more steps", "continue"}},
		{name: "saved then time ran out", stop: stopTimeBudget, trace: saved, wantParts: []string{`Done: Created Item "ITM-3".`, "30 seconds", "continue"}},
		{name: "token budget", stop: stopTokenBudget, trace: &turnStops{}, wantParts: []string{"token budget", "continue"}},
		{name: "stall names the repeat", stop: stopStall, trace: &turnStops{}, wantParts: []string{"repeated", "name the record"}},
		{name: "repeated error quotes the blocker", stop: stopRepeatError, trace: &turnStops{lastErr: "The arguments are not valid JSON."}, wantParts: []string{"Blocked: The arguments are not valid JSON.", "check the value"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stopReply(tc.stop, tc.trace)
			for _, part := range tc.wantParts {
				if !strings.Contains(got, part) {
					t.Errorf("stopReply() = %q, missing %q", got, part)
				}
			}
			if strings.Contains(got, "ran out of rounds") || strings.Contains(got, "finished") {
				t.Errorf("stopReply() = %q, uses the old round-cap wording", got)
			}
		})
	}
}
