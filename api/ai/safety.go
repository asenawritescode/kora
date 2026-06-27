package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/doctype"
)

// ---------------------------------------------------------------------------
// Stall detection helpers
// ---------------------------------------------------------------------------

// toolCallSignatures returns a canonical signature for each tool call
// (name + sorted JSON args) for comparison across rounds.
func toolCallSignatures(toolCalls []any) []string {
	sigs := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn := safeGetMap(call, "function")
		if fn == nil {
			continue
		}
		name := safeGetString(fn, "name")
		args := safeGetString(fn, "arguments")
		sigs = append(sigs, name+"::"+args)
	}
	return sigs
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Tool result helpers
// ---------------------------------------------------------------------------

// capResultSize truncates a tool result to maxChars, preserving head and tail.
func capResultSize(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] +
		fmt.Sprintf("\n\n... [%d characters trimmed] ...\n\n", len(content)-maxChars) +
		content[len(content)-half:]
}

func isToolError(content string) bool {
	return strings.HasPrefix(content, "Error") || strings.HasPrefix(content, "Unknown")
}

// ---------------------------------------------------------------------------
// Textified tool call detection
// ---------------------------------------------------------------------------

var textifiedPatterns = []string{
	"<｜｜DSML｜｜tool_calls>",
	"<function_call>",
	"<invoke name=",
	`"tool_calls"`,
	`"function_call"`,
}

func hasTextifiedToolCall(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pat := range textifiedPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Narrate-then-act detection (GPT-4o false finish)
// ---------------------------------------------------------------------------

var narrativePatterns = []string{
	"i'll look",
	"let me check",
	"i will search",
	"let me search",
	"i'll check",
	"i'll find",
	"let me find",
	"i'll look that up",
	"let me look that up",
	"i will look",
}

func isNarrativePromise(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pat := range narrativePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// containsActualData is a cheap heuristic: does the content contain data (markdown table,
// bullet list, key-value pairs) rather than just a conversational promise.
func containsActualData(content string) bool {
	markers := []string{"|", "**", "- ", "1. ", "✅", "❌", "$", "€", "£"}
	for _, m := range markers {
		if strings.Contains(content, m) {
			return true
		}
	}
	// If the content is longer than 200 chars, it likely contains substantive data.
	return len(content) > 200
}

// ---------------------------------------------------------------------------
// Partial tool call detection (for finish_reason == "length")
// ---------------------------------------------------------------------------

func hasMalformedArgs(toolCalls []any) bool {
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn := safeGetMap(call, "function")
		if fn == nil {
			continue
		}
		argsJSON := safeGetString(fn, "arguments")
		if argsJSON == "" {
			continue
		}
		var dummy map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &dummy); err != nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// History sanitization
// ---------------------------------------------------------------------------

func sanitizeHistory(history []ChatMessage, limit int) []ChatMessage {
	if len(history) == 0 {
		return nil
	}
	// Cap at limit.
	if len(history) > limit {
		history = history[len(history)-limit:]
	}
	// Filter out system messages and messages with tool call content.
	var cleaned []ChatMessage
	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		// Skip messages that look like they contain raw tool calls.
		if hasTextifiedToolCall(m.Content) {
			continue
		}
		cleaned = append(cleaned, m)
	}
	return cleaned
}

// ---------------------------------------------------------------------------
// Context compaction
// ---------------------------------------------------------------------------

// compactHistory keeps the first message (system prompt), the user's first message,
// and the most recent keepRecent messages. Tool results in between are summarized.
func compactHistory(messages []map[string]any, keepRecent int) []map[string]any {
	if len(messages) <= keepRecent+2 {
		return messages
	}

	compacted := make([]map[string]any, 0, keepRecent+3)

	// Always preserve the system prompt (first message).
	compacted = append(compacted, messages[0])

	// Preserve the user's first message (second message after system prompt, or the
	// first user-role message we find).
	for i := 1; i < len(messages)-keepRecent; i++ {
		if safeGetString(messages[i], "role") == "user" {
			compacted = append(compacted, messages[i])
			break
		}
	}

	// Build a summary of what happened in the compacted region.
	summary := summarizeCompacted(messages[1 : len(messages)-keepRecent])
	if summary != "" {
		compacted = append(compacted, map[string]any{
			"role":    "system",
			"content": "[Earlier conversation steps: " + summary + "]",
		})
	}

	// Keep the most recent messages.
	compacted = append(compacted, messages[len(messages)-keepRecent:]...)

	return compacted
}

func summarizeCompacted(messages []map[string]any) string {
	var actions []string
	for _, m := range messages {
		role := safeGetString(m, "role")
		switch role {
		case "tool":
			content := safeGetString(m, "content")
			if content != "" {
				// Extract first line or first 120 chars.
				firstLine := strings.SplitN(content, "\n", 2)[0]
				if len(firstLine) > 120 {
					firstLine = firstLine[:120] + "..."
				}
				actions = append(actions, firstLine)
			}
		case "user":
			content := safeGetString(m, "content")
			if content != "" && !strings.HasPrefix(content, "You've called") && !strings.HasPrefix(content, "You appear to") && !strings.HasPrefix(content, "You mentioned") {
				if len(content) > 100 {
					content = content[:100] + "..."
				}
				actions = append(actions, "User asked: "+content)
			}
		}
	}
	// Take the most recent 3 actions for the summary.
	if len(actions) > 3 {
		actions = actions[len(actions)-3:]
	}
	return strings.Join(actions, "; ")
}

// ---------------------------------------------------------------------------
// Field validation helpers
// ---------------------------------------------------------------------------

// unknownFields returns args keys that don't exist on the doctype.
func unknownFields(args map[string]any, dt *doctype.DocType) []string {
	valid := make(map[string]bool)
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			valid[f.Fieldname] = true
		}
	}
	var unknown []string
	for k := range args {
		if !valid[k] {
			unknown = append(unknown, k)
		}
	}
	return unknown
}

// availableFieldNames returns the doctype's data field names as a comma-separated string.
func availableFieldNames(dt *doctype.DocType) string {
	var names []string
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			names = append(names, f.Fieldname)
		}
	}
	return strings.Join(names, ", ")
}
