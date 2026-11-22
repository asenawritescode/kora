package ai

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/asenawritescode/kora/internal/secret"
)

func TestValidateProviderProfile(t *testing.T) {
	tests := []struct {
		name    string
		prof    ProviderProfile
		wantErr bool
	}{
		{
			name: "openai valid",
			prof: ProviderProfile{
				ProviderKey:    "openai_api_key",
				BaseURL:        "https://api.openai.com/v1",
				Model:          "gpt-4o",
				HTTPTimeoutSec: 60,
			},
		},
		{
			name: "missing model",
			prof: ProviderProfile{
				ProviderKey:    "openai_api_key",
				BaseURL:        "https://api.openai.com/v1",
				HTTPTimeoutSec: 60,
			},
			wantErr: true,
		},
		{
			name: "base url mismatch",
			prof: ProviderProfile{
				ProviderKey:    "anthropic_api_key",
				BaseURL:        "https://api.openai.com/v1",
				Model:          "claude-sonnet-4-6",
				HTTPTimeoutSec: 60,
			},
			wantErr: true,
		},
		{
			name: "negative timeout",
			prof: ProviderProfile{
				ProviderKey:    "deepseek_api_key",
				BaseURL:        "https://api.deepseek.com",
				Model:          "deepseek-v4-pro",
				HTTPTimeoutSec: 0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateProviderProfile(tt.prof)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCallAIWithRetryRetriesTransientAndSucceeds(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`temporary outage`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	got, err := callAIWithRetry(providerCall{BaseURL: server.URL, APIKey: "test-key", Body: map[string]any{"model": "gpt-4o"}, Config: AIConfig{MaxRetries: 1, RetryBackoffMs: 1}})
	if err != nil {
		t.Fatalf("callAIWithRetry: %v", err)
	}
	if got == nil || attempts != 2 {
		t.Fatalf("expected retry success after 2 attempts, attempts=%d result=%#v", attempts, got)
	}
}

func TestCallAIWithRetryHonorsTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"late"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	start := time.Now()
	_, err := callAIWithRetry(providerCall{BaseURL: server.URL, APIKey: "test-key", Body: map[string]any{"model": "gpt-4o"}, Config: AIConfig{HTTPTimeoutSec: 1, MaxRetries: 0}})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("timeout took too long: %v", time.Since(start))
	}
}

func TestCallAIWithRetryFailsOnNonTransientMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()

	_, err := callAIWithRetry(providerCall{BaseURL: server.URL, APIKey: "test-key", Body: map[string]any{"model": "gpt-4o"}, Config: AIConfig{MaxRetries: 2, RetryBackoffMs: 1}})
	if err == nil || !strings.Contains(err.Error(), "parsing AI response") {
		t.Fatalf("expected parsing failure, got %v", err)
	}
}

func TestResolveProviderPrefersCloudGateway(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	t.Setenv("KORA_CLOUD_AI_GATEWAY_URL", "http://127.0.0.1:8081/internal/engine/ai")
	t.Setenv("KORA_INTEGRATION_TOKEN", "engine-cloud-token")
	t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "cloud-model")

	providerKey, apiKey, baseURL, model := resolveProvider(db, "site-a", "")
	if providerKey != "kora_cloud" || apiKey != "engine-cloud-token" || baseURL != "http://127.0.0.1:8081/internal/engine/ai" || model != "cloud-model" {
		t.Fatalf("unexpected Cloud provider resolution: %q %q %q %q", providerKey, apiKey, baseURL, model)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAIConfigModelTable(t *testing.T) {
	tests := []struct {
		name                                               string
		model                                              string
		wantRounds, wantTokens, wantHistory, wantResultCap int
		wantBudget                                         int
	}{
		{name: "deepseek chat is tuned for short tool loops", model: "deepseek-chat", wantRounds: 6, wantTokens: 1024, wantHistory: 12, wantResultCap: 3000, wantBudget: 60000},
		{name: "unknown model keeps the defaults", model: "some-unlisted-model", wantRounds: 10, wantTokens: 4096, wantHistory: 20, wantResultCap: 4000, wantBudget: 80000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer func() { _ = database.Close() }()
			// No site override is stored: every lookup finds no row, so only the model table applies.
			for range 11 {
				mock.ExpectQuery("").WillReturnError(sql.ErrNoRows)
			}
			cfg := LoadAIConfig(secret.NewStore(database), "demo", tc.model)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("site overrides were not all read: %v", err)
			}
			if cfg.MaxRounds != tc.wantRounds || cfg.MaxTokensPerCall != tc.wantTokens || cfg.HistoryLimit != tc.wantHistory ||
				cfg.MaxToolResultChars != tc.wantResultCap || cfg.TokenBudget != tc.wantBudget {
				t.Errorf("LoadAIConfig(%q) = rounds %d tokens %d history %d result %d budget %d",
					tc.model, cfg.MaxRounds, cfg.MaxTokensPerCall, cfg.HistoryLimit, cfg.MaxToolResultChars, cfg.TokenBudget)
			}
		})
	}
}

func TestGatewayHeadersFor(t *testing.T) {
	tests := []struct {
		name        string
		providerKey string
		channel     string
		want        map[string]string
	}{
		{
			name:        "gateway call on the workspace is web",
			providerKey: "kora_cloud",
			channel:     "",
			want: map[string]string{
				"X-Kora-Site": "site-a", "X-Kora-Channel": "web", "X-Kora-Run": "run-1", "X-Kora-Purpose": "chat",
			},
		},
		{
			name:        "gateway call on a channel names the channel",
			providerKey: "kora_cloud",
			channel:     "whatsapp",
			want: map[string]string{
				"X-Kora-Site": "site-a", "X-Kora-Channel": "whatsapp", "X-Kora-Run": "run-1", "X-Kora-Purpose": "chat",
			},
		},
		{
			name:        "direct provider sends no gateway headers",
			providerKey: "deepseek_api_key",
			channel:     "whatsapp",
			want:        nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gatewayHeadersFor(tt.providerKey, "site-a", tt.channel, "run-1")
			if len(got) != len(tt.want) {
				t.Fatalf("gatewayHeadersFor() = %v, want %v", got, tt.want)
			}
			for name, value := range tt.want {
				if got[name] != value {
					t.Fatalf("header %s = %q, want %q", name, got[name], value)
				}
			}
		})
	}
}

func TestDeepSeekFlashModelConfigAndAlias(t *testing.T) {
	flash, ok := modelConfigs["deepseek-flash"]
	if !ok {
		t.Fatal("deepseek-flash has no model config")
	}
	chat, ok := modelConfigs["deepseek-chat"]
	if !ok {
		t.Fatal("deepseek-chat alias has no model config")
	}
	if flash != chat {
		t.Fatalf("deepseek-chat alias = %+v, want deepseek-flash %+v", chat, flash)
	}
	if flash.MaxRounds != 6 || flash.TokenBudget != 60000 || flash.MaxTokensPerCall != 1024 || flash.HistoryLimit != 12 || flash.MaxToolResultChars != 3000 {
		t.Fatalf("deepseek-flash config = %+v, want the tuned values", flash)
	}
}

func TestDirectDeepSeekDefaultModelIsFlash(t *testing.T) {
	for _, p := range directProviders {
		if p.key == "deepseek_api_key" {
			if p.defaultModel != "deepseek-flash" || p.base != "https://api.deepseek.com" {
				t.Fatalf("DeepSeek provider = %+v, want base api.deepseek.com and model deepseek-flash", p)
			}
			return
		}
	}
	t.Fatal("no DeepSeek direct provider")
}

func TestRequestBodyDisablesDeepSeekThinking(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		body    map[string]any
		// want is the thinking field the request must carry; nil means the field must be absent.
		want map[string]any
	}{
		{name: "DeepSeek API", baseURL: "https://api.deepseek.com", body: map[string]any{"model": "deepseek-flash"}, want: map[string]any{"type": "disabled"}},
		{name: "deepseek model through the gateway", baseURL: "https://cloud.example/internal/engine/ai", body: map[string]any{"model": "deepseek-chat"}, want: map[string]any{"type": "disabled"}},
		{name: "caller already sets thinking", baseURL: "https://api.deepseek.com", body: map[string]any{"model": "deepseek-flash", "thinking": map[string]any{"type": "enabled"}}, want: map[string]any{"type": "enabled"}},
		{name: "OpenAI gets no thinking field", baseURL: "https://api.openai.com/v1", body: map[string]any{"model": "gpt-4o"}},
		{name: "non-DeepSeek gateway model gets no thinking field", baseURL: "https://cloud.example/internal/engine/ai", body: map[string]any{"model": "cloud-managed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.body)
			got := requestBody(tt.body, tt.baseURL)
			thinking, present := got["thinking"]
			if tt.want == nil {
				if present {
					t.Fatalf("thinking = %v, want absent", thinking)
				}
			} else {
				gotType, _ := thinking.(map[string]any)
				if !present || gotType["type"] != tt.want["type"] {
					t.Fatalf("thinking = %v, want %v", thinking, tt.want)
				}
			}
			if len(tt.body) != before {
				t.Fatal("requestBody changed the caller's map")
			}
		})
	}
}
