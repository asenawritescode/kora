package ai

import (
	"encoding/json"
	"log/slog"
	"time"
)

// turnTimeBudget is how long one chat turn may run, from its first model call. It is a constant, not a setting.
const turnTimeBudget = 30 * time.Second

// stopReason is why a chat turn ended without an answer or a preview.
type stopReason string

const (
	stopMaxRounds   stopReason = "max_rounds"   // the hard cap on model rounds
	stopStall       stopReason = "stall"        // the same tool calls came back in the next round
	stopRepeatError stopReason = "repeat_error" // the same tool error code came back twice in a row
	stopTimeBudget  stopReason = "time_budget"  // the turn ran past turnTimeBudget
	stopTokenBudget stopReason = "token_budget" // the run used more tokens than the budget
)

// turnStops is the repeat and budget state of one turn. A tool call that repeats the previous round's calls is
// a stall: the turn ends on the second identical round rather than calling the same tool again.
type turnStops struct {
	deadline    time.Time
	lastSigs    []string
	lastErrCode string
}

func newTurnStops(start time.Time) *turnStops {
	return &turnStops{deadline: start.Add(turnTimeBudget)}
}

// beforeRound reports the stop that ends the turn before another model round is called.
func (s *turnStops) beforeRound(now time.Time) (stopReason, bool) {
	if !now.Before(s.deadline) {
		return stopTimeBudget, true
	}
	return "", false
}

// stallOn reports whether this round's tool calls repeat the previous round's calls exactly. The repeated calls are not run.
func (s *turnStops) stallOn(sigs []string) bool {
	if len(sigs) > 0 && stringSlicesEqual(sigs, s.lastSigs) {
		return true
	}
	s.lastSigs = sigs
	return false
}

// resultStop reports the stop that the executed tool results call for: the same error code twice in a row.
func (s *turnStops) resultStop(results []map[string]any) (stopReason, bool) {
	for _, tr := range results {
		content, _ := tr["content"].(string)
		code := toolErrorCode(content)
		if code == "" {
			s.lastErrCode = ""
			continue
		}
		if code == s.lastErrCode {
			return stopRepeatError, true
		}
		s.lastErrCode = code
	}
	return "", false
}

// toolErrorCode is the structured error code of a tool result, or "" for a result that is not a structured error.
// Malformed tool-call JSON is a structured error too, so a second malformed call in a row ends the turn: one repair.
func toolErrorCode(content string) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return ""
	}
	return env.Error.Code
}

// logToolCalls writes each tool call of a round to the log, by name and arguments.
func logToolCalls(toolCalls []any) {
	for _, tc := range toolCalls {
		if call, ok := tc.(map[string]any); ok {
			fn := safeGetMap(call, "function")
			slog.Info("AI tool call", "name", safeGetString(fn, "name"), "args", safeGetString(fn, "arguments"))
		}
	}
}

// tallyToolResults counts the successful and failed tool results of a round, marks the failed ones, and caps each
// result to maxChars so the next model call stays small.
func tallyToolResults(results []map[string]any, maxChars int) (errs, steps int) {
	for i, tr := range results {
		raw, _ := tr["content"].(string)
		slog.Info("Tool result", "content", raw[:min(len(raw), 200)])
		if isToolError(raw) {
			errs++
			tr["is_error"] = true
		} else {
			steps++
		}
		results[i]["content"] = capResultSize(raw, maxChars)
	}
	return errs, steps
}
