package ai

import (
	"database/sql"
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

func TestResolveProviderFallsBackToSharedEnv(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT encrypted_value FROM _kora_secret WHERE site = \\? AND key_name = \\?").
		WithArgs("site-a", "openai_api_key").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT encrypted_value FROM _kora_secret WHERE site = \\? AND key_name = \\?").
		WithArgs("site-a", "deepseek_api_key").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT encrypted_value FROM _kora_secret WHERE site = \\? AND key_name = \\?").
		WithArgs("site-a", "anthropic_api_key").
		WillReturnError(sql.ErrNoRows)

	t.Setenv("KORA_SHARED_AI_ENABLED", "true")
	t.Setenv("KORA_SHARED_OPENAI_API_KEY", "shared-openai")
	t.Setenv("KORA_SHARED_DEEPSEEK_API_KEY", "")
	t.Setenv("KORA_SHARED_ANTHROPIC_API_KEY", "")

	providerKey, apiKey, baseURL, model := resolveProvider(db, "site-a", "")
	if providerKey != "openai_api_key" || apiKey != "shared-openai" || baseURL != "https://api.openai.com/v1" || model != "gpt-4o" {
		t.Fatalf("unexpected shared provider fallback: %q %q %q %q", providerKey, apiKey, baseURL, model)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
