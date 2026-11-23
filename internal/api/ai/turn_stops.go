package ai

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
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
	// done is what the turn saved, in order: the outcome line of each confirmed write.
	done []string
	// lastErr is the message of the last structured tool error, the blocker in the ending.
	lastErr string
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

// afterResults records what the executed tool results did and reports the stop they call for: the same error code
// twice in a row. A saved write is recorded as done; a structured error is recorded as the blocker.
func (s *turnStops) afterResults(results []map[string]any) (stopReason, bool) {
	for _, tr := range results {
		content, _ := tr["content"].(string)
		code, message := toolErrorOf(content)
		if code == "" {
			s.lastErrCode = ""
			if isSavedOutcome(content) {
				s.done = append(s.done, content)
			}
			continue
		}
		s.lastErr = message
		if code == s.lastErrCode {
			return stopRepeatError, true
		}
		s.lastErrCode = code
	}
	return "", false
}

// isSavedOutcome reports whether a successful tool result is the outcome line of a confirmed write.
func isSavedOutcome(content string) bool {
	return strings.HasPrefix(content, "Created ") || strings.HasPrefix(content, "Updated ")
}

// toolErrorOf is the structured error code and message of a tool result, or "" for a result that is not a structured
// error. Malformed tool-call JSON is a structured error too, so a second malformed call in a row ends the turn: one repair.
func toolErrorOf(content string) (code, message string) {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return "", ""
	}
	return env.Error.Code, env.Error.Message
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

// tallyToolResults marks the failed tool results of a round and caps each result to maxChars so the next model call
// stays small. It returns how many results failed.
func tallyToolResults(results []map[string]any, maxChars int) (errs int) {
	for i, tr := range results {
		raw, _ := tr["content"].(string)
		slog.Info("Tool result", "content", raw[:min(len(raw), 200)])
		if isToolError(raw) {
			errs++
			tr["is_error"] = true
		}
		results[i]["content"] = capResultSize(raw, maxChars)
	}
	return errs
}

// stopReply is the ending of a stopped turn, built from its trace: what was saved, what blocked it, and one next
// step. It never says the turn ran out of rounds.
func stopReply(stop stopReason, trace *turnStops) string {
	done := "Nothing is saved yet."
	if len(trace.done) > 0 {
		done = "Done: " + strings.Join(trace.done, " ")
	}
	return done + " Blocked: " + stopBlocker(stop, trace) + " Next: " + stopNextStep(stop)
}

// stopBlocker says in plain words why the turn stopped.
func stopBlocker(stop stopReason, trace *turnStops) string {
	switch stop {
	case stopStall:
		return "the same lookup repeated without a new result."
	case stopRepeatError:
		if trace.lastErr != "" {
			return trace.lastErr
		}
		return "the same error came back twice."
	case stopTimeBudget:
		return "the request took longer than " + strconv.Itoa(int(turnTimeBudget.Seconds())) + " seconds."
	case stopTokenBudget:
		return "the request used more of its token budget than it allows."
	default:
		return "the request needed more steps than it allows."
	}
}

// stopNextStep is the one thing the person does next.
func stopNextStep(stop stopReason) string {
	switch stop {
	case stopStall:
		return "name the record exactly, or give more detail."
	case stopRepeatError:
		return "check the value named above and ask again."
	default:
		return "reply continue to pick up the rest."
	}
}
