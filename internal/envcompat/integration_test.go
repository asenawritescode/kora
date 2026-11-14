package envcompat

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIntegrationTokenReadsOneVariable(t *testing.T) {
	t.Setenv("KORA_ENGINE_PROVISIONING_TOKEN", "old")
	t.Setenv(IntegrationTokenEnv, "  shared  ")
	if got := IntegrationToken(); got != "shared" {
		t.Fatalf("IntegrationToken() = %q, want the trimmed shared value", got)
	}
}

// Retired names are reported by name only, so an operator can remove them.
func TestRetiredTokenNamesAreReportedWithoutValues(t *testing.T) {
	env := map[string]string{"KORA_RELOAD_TOKEN": "reload-secret", "KORA_CLOUD_EVENTS_TOKEN": " ", "KORA_ENGINE_MANAGEMENT_TOKEN": "management-secret"}
	lookup := func(k string) string { return env[k] }
	var logs bytes.Buffer
	warnRetiredTokens(slog.New(slog.NewTextHandler(&logs, nil)), lookup)
	out := logs.String()
	for _, want := range []string{"KORA_ENGINE_MANAGEMENT_TOKEN", "KORA_RELOAD_TOKEN", IntegrationTokenEnv} {
		if !strings.Contains(out, want) {
			t.Fatalf("warning = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "KORA_CLOUD_EVENTS_TOKEN") || strings.Contains(out, "secret") {
		t.Fatalf("warning names a blank variable or leaks a value: %q", out)
	}
	logs.Reset()
	warnRetiredTokens(slog.New(slog.NewTextHandler(&logs, nil)), func(string) string { return "" })
	if logs.Len() != 0 {
		t.Fatalf("warned with nothing retired set: %q", logs.String())
	}
}
