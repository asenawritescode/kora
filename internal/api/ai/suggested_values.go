package ai

import (
	"fmt"
	"strings"
	"unicode"
)

// userWords is the person's own text in this conversation: every earlier message of theirs and the current one.
// Values in a preview are checked against it.
func userWords(transcript []ChatMessage, current string) string {
	var parts []string
	for _, m := range transcript {
		if m.Role == "user" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(append(parts, current), "\n")
}

// suggestedFields names the fields of a preview whose value the person did not give. It is a deterministic check:
// a value is given when its words appear, in order and as whole words, in the person's text. Anything else is a
// suggestion, including a value the text only partly matches.
func suggestedFields(values map[string]any, words string) map[string]any {
	suggested := map[string]any{}
	for field, value := range values {
		if !givenIn(value, words) {
			suggested[field] = true
		}
	}
	return suggested
}

// givenIn reports whether a value is in the person's text. An empty value is not a suggestion, so it counts as given.
func givenIn(value any, words string) bool {
	if value == nil {
		return true
	}
	want := normalizeWords(fmt.Sprint(value))
	if want == "" {
		return true
	}
	return strings.Contains(" "+normalizeWords(words)+" ", " "+want+" ")
}

// normalizeWords lowercases text and splits it on every character that is not a letter or a digit.
func normalizeWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), " ")
}

// markSuggested adds the "suggested" map to a create or update preview: one entry per field the person did not give,
// set to true. A create marks its fields; an update marks the new value of each change.
func markSuggested(preview map[string]any, words string) {
	values := map[string]any{}
	switch preview["action"] {
	case "create":
		values, _ = preview["fields"].(map[string]any)
	case "update":
		changes, _ := preview["changes"].(map[string]any)
		for field, raw := range changes {
			if change, ok := raw.(map[string]any); ok {
				values[field] = change["new"]
			}
		}
	}
	if suggested := suggestedFields(values, words); len(suggested) > 0 {
		preview["suggested"] = suggested
	}
}
