package cli

import (
	"os"
	"testing"
)

func TestNATSEnabledIsExplicit(t *testing.T) {
	old := os.Getenv("KORA_EVENT_PROVIDER")
	t.Cleanup(func() { _ = os.Setenv("KORA_EVENT_PROVIDER", old) })

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"local", false},
		{"nats", true},
		{"NATS", false},
	} {
		if err := os.Setenv("KORA_EVENT_PROVIDER", tc.value); err != nil {
			t.Fatalf("set env: %v", err)
		}
		if got := natsEnabled(); got != tc.want {
			t.Fatalf("natsEnabled(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
