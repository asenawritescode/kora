package netguard

import "testing"

func TestIsPrivateHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"api.localhost", true},
		{"db.internal", true},
		{"printer.local", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"192.168.1.10", true},
		{"169.254.169.254", true},
		{"api.safaricom.co.ke", false},
		{"8.8.8.8", false},
	}
	for _, tt := range tests {
		if got := IsPrivateHost(tt.host); got != tt.want {
			t.Errorf("IsPrivateHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
