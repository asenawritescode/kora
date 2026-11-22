//go:build integration

package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// TestGatewayCallUsageStoredAcrossDialects runs one scripted Cloud gateway call and checks, on MySQL and Postgres, that
// the gateway received the four attribution headers and that the usage row keeps the requested model, the served model
// and the token classes, cached input included.
func TestGatewayCallUsageStoredAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ name, dsn string }{
		{name: "mysql", dsn: "KORA_AI_GATEWAY_USAGE_MYSQL_DSN"},
		{name: "postgres", dsn: "KORA_AI_GATEWAY_USAGE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			database := isolatedAIDatabase(t, tc.name, dsn)
			dialect := kdb.Resolve(tc.name)
			ctx := WithDialect(context.Background(), dialect)
			if err := ksite.BootstrapSystemTables(ctx, database, dialect); err != nil {
				t.Fatalf("bootstrap %s system tables: %v", tc.name, err)
			}
			if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
				t.Fatalf("create AI run tables: %v", err)
			}

			const runID = "run-gateway-usage"
			const requested = "deepseek-chat"
			var gotHeaders http.Header
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				// DeepSeek-shaped reply: the served model differs from the requested one. 1000 input tokens, 800 of
				// them cache hits, so 200 are cache misses; 100 output tokens.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"deepseek-flash","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200}}`))
			}))
			defer gateway.Close()

			_, err := callAIWithRetry(providerCall{
				BaseURL: gateway.URL,
				APIKey:  "gateway-token",
				Body:    map[string]any{"model": requested, "messages": []any{}},
				Config:  AIConfig{HTTPTimeoutSec: 5},
				Headers: gatewayHeadersFor("kora_cloud", "site-gateway", "whatsapp", runID),
				Record: func(attempt int, status string, latency time.Duration, reply providerReply, attemptErr error) {
					_ = RecordUsage(ctx, database, contract.UsageEvent{
						RunID: runID, Site: "site-gateway", Model: requested,
						Provider: "kora_cloud", Attempt: attempt, Status: status, Tokens: reply.Usage.tokenClasses(),
						Attribution: usageAttribution(reply.Model),
					})
				},
			})
			if err != nil {
				t.Fatalf("gateway call: %v", err)
			}

			wantHeaders := map[string]string{
				"X-Kora-Site": "site-gateway", "X-Kora-Channel": "whatsapp", "X-Kora-Run": runID, "X-Kora-Purpose": "chat",
			}
			for name, want := range wantHeaders {
				if got := gotHeaders.Get(name); got != want {
					t.Fatalf("gateway header %s = %q, want %q", name, got, want)
				}
			}

			var model, tokens, attribution string
			query := kdb.Rebind(dialect, `SELECT model, tokens, attribution FROM _kora_ai_usage WHERE run_id = ?`)
			if err := database.QueryRowContext(ctx, query, runID).Scan(&model, &tokens, &attribution); err != nil {
				t.Fatalf("read usage row: %v", err)
			}
			if model != requested {
				t.Fatalf("usage row model = %q, want requested %q", model, requested)
			}
			var attr map[string]string
			if err := json.Unmarshal([]byte(attribution), &attr); err != nil || attr["served_model"] != "deepseek-flash" {
				t.Fatalf("usage attribution = %s (%v), want served_model deepseek-flash", attribution, err)
			}
			var classes map[string]int64
			if err := json.Unmarshal([]byte(tokens), &classes); err != nil {
				t.Fatalf("usage tokens = %s: %v", tokens, err)
			}
			want := map[string]int64{"input": 1000, "cached_input": 800, "output": 100, "total": 1100}
			for class, n := range want {
				if classes[class] != n {
					t.Fatalf("usage tokens[%s] = %d, want %d (all: %s)", class, classes[class], n, tokens)
				}
			}
		})
	}
}
