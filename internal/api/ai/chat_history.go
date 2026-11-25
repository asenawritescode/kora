package ai

// modelHistory returns the transcript the model is given for this turn. A workspace turn gets the transcript
// unchanged. A channel turn (WhatsApp) keeps every user message but only the most recent assistant message: older
// assistant replies are claims, not facts, and on a channel they were copied forward as if they were true. Facts
// come from this turn's tool calls. The input slice is not modified and the stored transcript is untouched.
func (t ChatTurn) modelHistory(transcript []ChatMessage) []ChatMessage {
	if t.Channel == "" {
		return transcript
	}
	lastAssistant := -1
	for i, m := range transcript {
		if m.Role == "assistant" {
			lastAssistant = i
		}
	}
	var kept []ChatMessage
	for i, m := range transcript {
		if m.Role == "assistant" && i != lastAssistant {
			continue
		}
		kept = append(kept, m)
	}
	return kept
}
