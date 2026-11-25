package ai

import (
	"reflect"
	"strings"
	"testing"
)

func TestModelHistory(t *testing.T) {
	u1 := ChatMessage{Role: "user", Content: "u1"}
	u2 := ChatMessage{Role: "user", Content: "u2"}
	u3 := ChatMessage{Role: "user", Content: "u3"}
	a1 := ChatMessage{Role: "assistant", Content: "a1"}
	a2 := ChatMessage{Role: "assistant", Content: "a2"}
	a3 := ChatMessage{Role: "assistant", Content: "a3"}

	tests := []struct {
		name       string
		channel    string
		transcript []ChatMessage
		want       []ChatMessage
	}{
		{
			name:       "whatsapp keeps users and only the last reply",
			channel:    "whatsapp",
			transcript: []ChatMessage{u1, a1, u2, a2, u3, a3},
			want:       []ChatMessage{u1, u2, u3, a3},
		},
		{
			name:       "whatsapp transcript ending with a user message keeps the last reply before it",
			channel:    "whatsapp",
			transcript: []ChatMessage{u1, a1, u2, a2, u3},
			want:       []ChatMessage{u1, u2, a2, u3},
		},
		{
			name:       "whatsapp without assistant messages keeps all user messages",
			channel:    "whatsapp",
			transcript: []ChatMessage{u1, u2},
			want:       []ChatMessage{u1, u2},
		},
		{
			name:       "whatsapp empty transcript is empty",
			channel:    "whatsapp",
			transcript: nil,
			want:       nil,
		},
		{
			name:       "workspace chat is unchanged",
			channel:    "",
			transcript: []ChatMessage{u1, a1, u2, a2, u3, a3},
			want:       []ChatMessage{u1, a1, u2, a2, u3, a3},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ChatTurn{Channel: tc.channel}.modelHistory(tc.transcript)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("modelHistory() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestModelHistoryDoesNotModifyInput(t *testing.T) {
	transcript := []ChatMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
	}
	before := append([]ChatMessage(nil), transcript...)

	_ = ChatTurn{Channel: "whatsapp"}.modelHistory(transcript)

	if !reflect.DeepEqual(transcript, before) {
		t.Fatalf("input changed: got %+v, want %+v", transcript, before)
	}
}

func TestWhatsAppTurnMessagesCarryOnlyLastReply(t *testing.T) {
	transcript := []ChatMessage{
		{Role: "user", Content: "bills?"},
		{Role: "assistant", Content: "OLD-REPLY-MADE-UP"},
		{Role: "user", Content: "bills again?"},
		{Role: "assistant", Content: "LAST-REPLY"},
	}
	turn := ChatTurn{Channel: "whatsapp"}

	messages := turnMessages(turn.modelHistory(transcript), 30, "", "", "new message")

	var contents []string
	for _, m := range messages {
		contents = append(contents, m["content"].(string))
	}
	joined := strings.Join(contents, "\n")
	if !strings.Contains(joined, "LAST-REPLY") {
		t.Fatalf("last assistant reply missing from model messages: %q", joined)
	}
	if strings.Contains(joined, "OLD-REPLY-MADE-UP") {
		t.Fatalf("older assistant reply reached the model: %q", joined)
	}
	if last := messages[len(messages)-1]; last["role"] != "user" || last["content"] != "new message" {
		t.Fatalf("new user message not last: %+v", last)
	}
}
