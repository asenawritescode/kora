package ai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
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

	got, err := callAIWithRetry(server.URL, "test-key", map[string]any{"model": "gpt-4o"}, AIConfig{MaxRetries: 1, RetryBackoffMs: 1}, nil)
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
	_, err := callAIWithRetry(server.URL, "test-key", map[string]any{"model": "gpt-4o"}, AIConfig{HTTPTimeoutSec: 1, MaxRetries: 0}, nil)
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

	_, err := callAIWithRetry(server.URL, "test-key", map[string]any{"model": "gpt-4o"}, AIConfig{MaxRetries: 2, RetryBackoffMs: 1}, nil)
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
	t.Setenv("KORA_CLOUD_AI_GATEWAY_TOKEN", "engine-cloud-token")
	t.Setenv("KORA_CLOUD_AI_GATEWAY_MODEL", "cloud-model")

	providerKey, apiKey, baseURL, model := resolveProvider(db, "site-a", "")
	if providerKey != "kora_cloud" || apiKey != "engine-cloud-token" || baseURL != "http://127.0.0.1:8081/internal/engine/ai" || model != "cloud-model" {
		t.Fatalf("unexpected Cloud provider resolution: %q %q %q %q", providerKey, apiKey, baseURL, model)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
