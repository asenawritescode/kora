package auth

import "testing"

func TestMatchesEnvToken(t *testing.T) {
	const key = "KORA_TEST_ENV_TOKEN"
	tests := []struct {
		name      string
		env       string
		presented string
		want      bool
	}{
		{name: "match", env: "secret-token", presented: "secret-token", want: true},
		{name: "mismatch same length", env: "secret-token", presented: "secret-tokem", want: false},
		{name: "mismatch different length", env: "secret-token", presented: "secret", want: false},
		{name: "empty presented", env: "secret-token", presented: "", want: false},
		{name: "unset env never matches", env: "", presented: "", want: false},
		{name: "unset env rejects token", env: "", presented: "anything", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(key, tt.env)
			if got := matchesEnvToken(tt.presented, key); got != tt.want {
				t.Fatalf("matchesEnvToken(%q) = %v, want %v", tt.presented, got, tt.want)
			}
		})
	}
}
