package ai

import "log/slog"

// roundsKeptWhole is how many earlier tool rounds keep their full results. The model answers from the latest rounds;
// older results are dropped to a stub so each call stays small.
const roundsKeptWhole = 2

// droppedResult stands in for a tool result from an earlier round.
const droppedResult = "[result of an earlier step, dropped]"

// appendRound adds the model's tool calls and their results to the history for the next call. Earlier rounds'
// results beyond roundsKeptWhole are dropped, and the estimated size of the next call is logged.
func appendRound(messages []map[string]any, content any, toolCalls []any, results []map[string]any, functions []map[string]any) []map[string]any {
	messages = dropOldToolResults(messages, roundsKeptWhole)
	messages = append(messages, map[string]any{
		"role":       "assistant",
		"content":    content, // may be nil — that's fine
		"tool_calls": toolCalls,
	})
	messages = append(messages, results...)
	slog.Info("Chat round context", "messages", len(messages), "estimated_tokens", estimatePromptTokens(messages, functions))
	return messages
}

// dropOldToolResults replaces the content of every tool result that comes before the last keep rounds of tool calls.
// A round is an assistant message with tool_calls; its results follow it.
func dropOldToolResults(messages []map[string]any, keep int) []map[string]any {
	cut, rounds := 0, 0
	for i := len(messages) - 1; i >= 0 && rounds < keep; i-- {
		if messages[i]["role"] == "assistant" && messages[i]["tool_calls"] != nil {
			rounds++
			cut = i
		}
	}
	if rounds < keep {
		return messages
	}
	for i := 0; i < cut; i++ {
		if messages[i]["role"] == "tool" {
			messages[i]["content"] = droppedResult
		}
	}
	return messages
}
