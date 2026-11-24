package ai

import (
	"strings"

	"github.com/asenawritescode/kora/internal/textmatch"
)

// lookupCorrection is the message that sends back a reply about records the turn never looked up. Earlier answers
// in the conversation may be wrong or out of date, and a model tends to repeat them: a person who names a record
// type they may read gets an answer from this turn's tool results, not from the transcript.
const lookupCorrection = "You answered without looking anything up in this turn. Earlier answers in this conversation may be wrong or out of date. Call the tool that reads them now, such as find_records, and answer only from its result."

// recordTypesOf returns the record types the person may read: the record_type enum of the find_records tool.
func recordTypesOf(functions []map[string]any) []string {
	for _, raw := range functions {
		fn, _ := raw["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != toolFindRecords {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		recordType, _ := props["record_type"].(map[string]any)
		switch types := recordType["enum"].(type) {
		case []string:
			return types
		case []any:
			names := make([]string, 0, len(types))
			for _, v := range types {
				if name, ok := v.(string); ok {
					names = append(names, name)
				}
			}
			return names
		}
	}
	return nil
}

// namedTypes returns the record types a text names by their last word, singular or plural: "Supplier Bill" is
// named by "supplier bills" and by "bills".
func namedTypes(text string, types []string) []string {
	words := textmatch.Words(text)
	var named []string
	for _, recordType := range types {
		name := textmatch.Words(recordType)
		if len(name) == 0 {
			continue
		}
		for _, word := range words {
			if sameNoun(word, name[len(name)-1]) {
				named = append(named, recordType)
				break
			}
		}
	}
	return named
}

// sameNoun reports whether word is noun or its English plural.
func sameNoun(word, noun string) bool {
	if word == noun || word == noun+"s" || word == noun+"es" {
		return true
	}
	return strings.HasSuffix(noun, "y") && word == strings.TrimSuffix(noun, "y")+"ies"
}

// lookupFix returns the messages that send back a reply given without any tool call that talks about a record type
// the person named, once per turn. The next request then requires a tool call. A reply about something else, such
// as a refusal, may stand.
func (t *answerTurn) lookupFix(reply, message string) []map[string]any {
	if t.lookedUp || t.lookupSent || len(namedTypes(reply, namedTypes(message, t.recordTypes))) == 0 {
		return nil
	}
	t.lookupSent, t.forceTool = true, true
	return []map[string]any{
		{"role": "assistant", "content": reply},
		{"role": "user", "content": lookupCorrection},
	}
}

// resend sets the messages of the next request after a correction. A lookup correction requires a tool call on it.
func (t *answerTurn) resend(body map[string]any, messages []map[string]any) {
	body["messages"] = messages
	if t.forceTool {
		body["tool_choice"] = "required"
		t.forceTool = false
	}
}
