package api

import (
	"strings"
	"testing"
)

func TestChannelThreadID(t *testing.T) {
	long := strings.Repeat("k", 101)
	exact := strings.Repeat("k", 100)
	for _, tc := range []struct {
		name, key string
		hashed    bool
	}{
		{name: "short key is the thread", key: "wa-thread-1"},
		{name: "100 characters is still the thread", key: exact},
		{name: "101 characters is hashed", key: long, hashed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := channelThreadID(tc.key)
			if !tc.hashed {
				if got != tc.key {
					t.Fatalf("thread = %q, want the key itself", got)
				}
				return
			}
			if len(got) != 64 || got == tc.key || got != channelThreadID(tc.key) {
				t.Fatalf("thread = %q, want a stable 64-character digest", got)
			}
		})
	}
}
