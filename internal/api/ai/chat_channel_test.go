package ai

import (
	"strings"
	"testing"
)

func TestChannelReplyStyle(t *testing.T) {
	for _, tc := range []struct {
		channel string
		styled  bool
	}{
		{channel: "", styled: false},
		{channel: "web", styled: false},
		{channel: "whatsapp", styled: true},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			got := channelReplyStyle(tc.channel)
			if (got != "") != tc.styled {
				t.Fatalf("channelReplyStyle(%q) = %q, styled want %v", tc.channel, got, tc.styled)
			}
			if tc.styled && (!strings.Contains(got, "300 characters") || !strings.Contains(got, "numbered list")) {
				t.Fatalf("WhatsApp style omits the limits: %q", got)
			}
		})
	}
}

func TestToolsOnChannelKeepsOnlyAllowedTools(t *testing.T) {
	tools := []ToolDescriptor{
		{Name: "find_records", ChannelAllowlist: []string{"web", "whatsapp"}},
		{Name: "list_metrics", ChannelAllowlist: []string{"web"}},
		{Name: "no_channels"},
	}
	for _, tc := range []struct {
		channel string
		want    []string
	}{
		{channel: "whatsapp", want: []string{"find_records"}},
		{channel: "web", want: []string{"find_records", "list_metrics"}},
		{channel: "sms", want: nil},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			got := toolsOnChannel(tools, tc.channel)
			if len(got) != len(tc.want) {
				t.Fatalf("tools on %q = %d, want %d", tc.channel, len(got), len(tc.want))
			}
			for i, tool := range got {
				if tool.Name != tc.want[i] {
					t.Fatalf("tool %d on %q = %q, want %q", i, tc.channel, tool.Name, tc.want[i])
				}
			}
		})
	}
}
