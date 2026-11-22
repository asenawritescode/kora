package ai

import "testing"

func TestUsageAttribution(t *testing.T) {
	tests := []struct {
		name     string
		reported string
		want     map[string]string
	}{
		{name: "provider names the served model", reported: "deepseek-flash", want: map[string]string{"operation": "chat", "served_model": "deepseek-flash"}},
		{name: "provider names no model", reported: "", want: map[string]string{"operation": "chat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usageAttribution(tt.reported)
			if len(got) != len(tt.want) {
				t.Fatalf("usageAttribution(%q) = %v, want %v", tt.reported, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("usageAttribution(%q)[%s] = %q, want %q", tt.reported, k, got[k], v)
				}
			}
		})
	}
}
