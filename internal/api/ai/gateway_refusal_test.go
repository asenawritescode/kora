package ai

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

var gatewayRefusalCases = []struct {
	name    string
	status  int
	code    string
	message string
}{
	{name: "workspace disabled", status: http.StatusForbidden, code: "ai.workspace_disabled", message: "Zana's AI is turned off for this workspace."},
	{name: "unknown workspace", status: http.StatusForbidden, code: "ai.unknown_workspace", message: "This workspace is not connected to Kigae Cloud."},
	{name: "budget exhausted", status: http.StatusTooManyRequests, code: "ai.budget_exhausted", message: "This workspace has used its AI allowance for November."},
}

func refusalBody(code, message string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, message)
}

func TestCallAIWithRetryStopsOnGatewayRefusal(t *testing.T) {
	for _, tt := range gatewayRefusalCases {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(refusalBody(tt.code, tt.message)))
			}))
			defer server.Close()

			_, err := callAIWithRetry(providerCall{BaseURL: server.URL, APIKey: "k", Body: map[string]any{"model": "cloud-managed"}, Config: AIConfig{MaxRetries: 2, RetryBackoffMs: 1}})
			var refusal *gatewayRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("err = %v, want a gateway refusal", err)
			}
			if refusal.Status != tt.status || refusal.Code != tt.code || refusal.Message != tt.message {
				t.Fatalf("refusal = %+v, want status %d code %q message %q", *refusal, tt.status, tt.code, tt.message)
			}
			if n := attempts.Load(); n != 1 {
				t.Fatalf("attempts = %d, want 1 (no retry on a refusal)", n)
			}
		})
	}
}

func TestCallAIWithRetryRetriesPlainRateLimit(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	if _, err := callAIWithRetry(providerCall{BaseURL: server.URL, APIKey: "k", Body: map[string]any{"model": "cloud-managed"}, Config: AIConfig{MaxRetries: 1, RetryBackoffMs: 1}}); err != nil {
		t.Fatalf("callAIWithRetry: %v", err)
	}
	if n := attempts.Load(); n != 2 {
		t.Fatalf("attempts = %d, want 2 (plain 429 retries)", n)
	}
}

func TestRefusalFailureMapsGatewayCodes(t *testing.T) {
	for _, tt := range gatewayRefusalCases {
		t.Run(tt.name, func(t *testing.T) {
			outcome, ok := refusalFailure(&gatewayRefusal{Status: tt.status, Code: tt.code, Message: tt.message})
			if !ok {
				t.Fatal("refusalFailure ok = false, want true")
			}
			if outcome.Status != tt.status || outcome.Reply.Reply != "" {
				t.Fatalf("outcome status = %d reply = %q, want failure with status %d", outcome.Status, outcome.Reply.Reply, tt.status)
			}
			errBody, _ := outcome.Failure["error"].(gin.H)
			if errBody["code"] != tt.code || errBody["message"] != tt.message {
				t.Fatalf("failure body = %v, want code %q message %q", outcome.Failure, tt.code, tt.message)
			}
		})
	}
}

func TestRefusalFailureIgnoresOtherErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "provider 500", err: fmt.Errorf("AI provider returned 500: boom")},
		{name: "rate limit without refusal code", err: fmt.Errorf("AI provider returned 429: rate limit reached")},
		{name: "nil", err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := refusalFailure(tt.err); ok {
				t.Fatalf("refusalFailure ok = true for %v", tt.err)
			}
		})
	}
}

func TestParseGatewayRefusalOnlyKnownCodesOnRefusalStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "403 known code", status: http.StatusForbidden, body: refusalBody("ai.workspace_disabled", "off"), want: true},
		{name: "429 known code", status: http.StatusTooManyRequests, body: refusalBody("ai.budget_exhausted", "used"), want: true},
		{name: "500 known code", status: http.StatusInternalServerError, body: refusalBody("ai.budget_exhausted", "used"), want: false},
		{name: "403 unknown code", status: http.StatusForbidden, body: refusalBody("ai.other", "x"), want: false},
		{name: "403 not json", status: http.StatusForbidden, body: "forbidden", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGatewayRefusal(tt.status, []byte(tt.body)) != nil; got != tt.want {
				t.Fatalf("parseGatewayRefusal refusal = %v, want %v", got, tt.want)
			}
		})
	}
}
