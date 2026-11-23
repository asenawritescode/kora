package ai

import (
	"context"

	"github.com/asenawritescode/kora/internal/orm"
)

// windowTools are the only tools an open batch window offers: write previews. A window is open once a write in the
// turn has been previewed; the person then sees one card for every preview the turn makes.
var windowTools = map[string]bool{toolCreateRecord: true, toolUpdateRecord: true, toolRunAction: true}

// windowFunctions keeps the function definitions an open window offers.
func windowFunctions(functions []map[string]any) []map[string]any {
	var out []map[string]any
	for _, fn := range functions {
		if windowTools[functionName(fn)] {
			out = append(out, fn)
		}
	}
	return out
}

// functionName is the tool name of a function definition or of a tool call: both carry it under "function".
func functionName(m map[string]any) string {
	return safeGetString(safeGetMap(m, "function"), "name")
}

// windowAllows reports whether every call of a round is a write preview, the only kind an open window accepts.
func windowAllows(toolCalls []any) bool {
	if len(toolCalls) == 0 {
		return false
	}
	for _, tc := range toolCalls {
		call, _ := tc.(map[string]any)
		if !windowTools[functionName(call)] {
			return false
		}
	}
	return true
}

// windowOpen reports whether a write in this turn has been previewed.
func (t *answerTurn) windowOpen() bool {
	return len(t.previews) > 0
}

// windowClosedBy reports whether an open window refuses a round: a call that is not a write preview. The calls are
// then not run and the turn ends with the card.
func (t *answerTurn) windowClosedBy(toolCalls []any) bool {
	return t.windowOpen() && !windowAllows(toolCalls)
}

// windowEndedAt reports whether a round ended an open window: it added no preview to the ones there were before it.
func (t *answerTurn) windowEndedAt(before int) bool {
	return t.windowOpen() && len(t.previews) == before
}

// closesRound reports whether a round must end before its calls run: a stall, or a call an open window refuses. The
// stall check always runs, so its count stays right.
func (t *answerTurn) closesRound(stops *turnStops, toolCalls []any) bool {
	stalled := stops.stallOn(toolCallSignatures(toolCalls))
	return stalled || t.windowClosedBy(toolCalls)
}

// narrowTools offers the window's tools to the next call while a window is open.
func (t *answerTurn) narrowTools(body map[string]any, functions []map[string]any) {
	if t.windowOpen() {
		body["tools"] = windowFunctions(functions)
	}
}

// chatEnd is what a turn needs to end with its open previews.
type chatEnd struct {
	ctx    context.Context
	tx     *orm.TxManager
	answer finalAnswer // the turn's identity; StepID is set per round
}

// at is the turn's identity on one step.
func (e chatEnd) at(stepID string) finalAnswer {
	a := e.answer
	a.StepID = stepID
	return a
}

// cardOutcome ends the turn with the card of its open previews, on the given step.
func (e chatEnd) cardOutcome(answers *answerTurn, stepID string) ChatOutcome {
	return replyOutcome(endWithPreviews(e.ctx, e.tx, answers.previews, e.at(stepID)))
}

// afterTools ends the turn when a round's tools end it: a clarifying question, or an open window that the round added
// no preview to. ok is false when the turn goes on.
func (e chatEnd) afterTools(answers *answerTurn, stepID string, previewsBefore int) (out ChatOutcome, ok bool) {
	if answers.choice != nil {
		return replyOutcome(endWithChoice(e.ctx, e.tx, answers.choice, e.at(stepID))), true
	}
	if answers.windowEndedAt(previewsBefore) {
		return e.cardOutcome(answers, stepID), true
	}
	return ChatOutcome{}, false
}

// turnMessages is the first request's messages: the standing prompt, the durable history within its limit, the turn
// context and the person's message.
func turnMessages(transcript []ChatMessage, historyLimit int, standingExtra, turnContext, message string) []map[string]any {
	// Durable server history is the source of truth for the conversation.
	return buildChatMessages(chatSystemPrompt+standingExtra, sanitizeHistory(transcript, historyLimit), turnContext, message)
}

// providerBody is the request body of a model call. Tools are offered only when there are any.
func providerBody(model string, messages []map[string]any, functions []map[string]any, maxTokens int) map[string]any {
	body := map[string]any{
		"model":               model,
		"messages":            messages,
		"max_tokens":          maxTokens,
		"parallel_tool_calls": false, // Force sequential — avoids ordering issues.
	}
	if len(functions) > 0 {
		body["tools"] = functions
		body["tool_choice"] = "auto"
	}
	return body
}
