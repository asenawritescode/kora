package ai

import "testing"

// TestChatTurnChannel checks the channel a turn records: its own channel, or the workspace's "chat".
func TestChatTurnChannel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		channel string
		want    string
	}{
		{"workspace turn", "", "chat"},
		{"whatsapp turn", "whatsapp", "whatsapp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (ChatTurn{Channel: tc.channel}).channel(); got != tc.want {
				t.Fatalf("channel() = %q, want %q", got, tc.want)
			}
		})
	}
}
