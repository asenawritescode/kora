package script

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPreviewRefusesOutboundHTTP runs a script that calls kora.http.fetch. Under a preview
// context the call is refused with the preview message, before any provider is reached.
func TestPreviewRefusesOutboundHTTP(t *testing.T) {
	tests := []struct {
		name    string
		ctx     context.Context
		wantSub string
	}{
		{name: "preview", ctx: WithPreview(context.Background()), wantSub: "not allowed while the change is being previewed"},
		{name: "confirmed write", ctx: context.Background(), wantSub: "provider not available"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runner := NewEmbeddedRunner(EmbeddedConfig{PoolSize: 1, Timeout: 5 * time.Second})
			defer func() { _ = runner.Close() }()
			_, err := runner.Execute(tc.ctx, ExecuteRequest{
				Script:     `kora.http.fetch("https://example.test/hook", {method: "POST", body: "x"});`,
				ScriptType: TypeDocEvent, ScriptName: "preview_http", DocType: "Item", Event: EventBeforeInsert,
				Document: map[string]any{"name": "A"}, User: "alice@example.test", UserRoles: []string{"Administrator"}, Site: "test.local",
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Execute error = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}
