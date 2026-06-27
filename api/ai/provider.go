package ai

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/asenawritescode/kora/secret"
)

// AIConfig holds all configurable thresholds for the AI chat pipeline.
// Every value has a sensible default; site-level overrides are loaded
// from the secret store with an "ai." key prefix.
type AIConfig struct {
	MaxRounds           int     // Max tool-calling rounds (safety cap)
	TokenBudget         int     // Context window budget in tokens
	CompactionThreshold float64 // 0.0-1.0, when to start compacting history
	MaxToolResultChars  int     // Cap on a single tool result string
	StallThreshold      int     // Consecutive identical tool calls before nudge
	MaxToolErrors       int     // Total tool errors before circuit breaker
	MaxTokensPerCall    int     // max_tokens per AI API call
	HTTPTimeoutSec      int     // Timeout in seconds for AI provider HTTP calls
	MaxRetries          int     // Retries on transient AI errors (429, 503)
	RetryBackoffMs      int     // Base backoff in milliseconds
	HistoryLimit        int     // Max incoming history messages from client
}

// DefaultAIConfig returns sane defaults that work for most models.
func DefaultAIConfig() AIConfig {
	return AIConfig{
		MaxRounds:          10,
		TokenBudget:        80000,
		CompactionThreshold: 0.80,
		MaxToolResultChars: 4000,
		StallThreshold:     3,
		MaxToolErrors:      5,
		MaxTokensPerCall:   4096,
		HTTPTimeoutSec:     60,
		MaxRetries:         2,
		RetryBackoffMs:     500,
		HistoryLimit:       20,
	}
}

// modelDefaults maps model names (as resolved by resolveProvider) to
// model-specific overrides. The map is keyed by the exact model string
// (e.g., "gpt-4o", "claude-sonnet-4-6").
var modelConfigs = map[string]AIConfig{
	"gpt-4o": {
		MaxRounds: 15, TokenBudget: 120000, CompactionThreshold: 0.80,
		MaxToolResultChars: 4000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 4096, HTTPTimeoutSec: 60, MaxRetries: 2,
		RetryBackoffMs: 500, HistoryLimit: 20,
	},
	"gpt-4o-mini": {
		MaxRounds: 10, TokenBudget: 120000, CompactionThreshold: 0.80,
		MaxToolResultChars: 4000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 4096, HTTPTimeoutSec: 60, MaxRetries: 2,
		RetryBackoffMs: 500, HistoryLimit: 20,
	},
	"gpt-4.1": {
		MaxRounds: 15, TokenBudget: 950000, CompactionThreshold: 0.85,
		MaxToolResultChars: 8000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 8192, HTTPTimeoutSec: 60, MaxRetries: 2,
		RetryBackoffMs: 500, HistoryLimit: 30,
	},
	"claude-sonnet-4-6": {
		MaxRounds: 20, TokenBudget: 190000, CompactionThreshold: 0.85,
		MaxToolResultChars: 8000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 8192, HTTPTimeoutSec: 90, MaxRetries: 3,
		RetryBackoffMs: 1000, HistoryLimit: 30,
	},
	"claude-opus-4-8": {
		MaxRounds: 25, TokenBudget: 190000, CompactionThreshold: 0.85,
		MaxToolResultChars: 8000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 8192, HTTPTimeoutSec: 120, MaxRetries: 3,
		RetryBackoffMs: 1000, HistoryLimit: 30,
	},
	"claude-haiku-4-5": {
		MaxRounds: 10, TokenBudget: 190000, CompactionThreshold: 0.85,
		MaxToolResultChars: 4000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 4096, HTTPTimeoutSec: 60, MaxRetries: 2,
		RetryBackoffMs: 500, HistoryLimit: 20,
	},
	"deepseek-v4-pro": {
		MaxRounds: 15, TokenBudget: 120000, CompactionThreshold: 0.80,
		MaxToolResultChars: 4000, StallThreshold: 3, MaxToolErrors: 5,
		MaxTokensPerCall: 4096, HTTPTimeoutSec: 90, MaxRetries: 2,
		RetryBackoffMs: 1000, HistoryLimit: 20,
	},
}

// LoadAIConfig returns the effective AIConfig for the given model and site.
// Resolution order: DefaultAIConfig → model-specific defaults → site-level
// overrides from the secret store (keys prefixed with "ai.").
func LoadAIConfig(store *secret.Store, siteName, model string) AIConfig {
	cfg := DefaultAIConfig()

	// Apply model-specific defaults.
	if mc, ok := modelConfigs[model]; ok {
		cfg = mc
	}

	// Apply site-level overrides from the secret store.
	// Keys: ai.max_rounds, ai.token_budget, ai.max_tool_result_chars, etc.
	applyInt(store, siteName, "ai.max_rounds", &cfg.MaxRounds)
	applyInt(store, siteName, "ai.token_budget", &cfg.TokenBudget)
	applyFloat(store, siteName, "ai.compaction_threshold", &cfg.CompactionThreshold)
	applyInt(store, siteName, "ai.max_tool_result_chars", &cfg.MaxToolResultChars)
	applyInt(store, siteName, "ai.stall_threshold", &cfg.StallThreshold)
	applyInt(store, siteName, "ai.max_tool_errors", &cfg.MaxToolErrors)
	applyInt(store, siteName, "ai.max_tokens_per_call", &cfg.MaxTokensPerCall)
	applyInt(store, siteName, "ai.http_timeout_sec", &cfg.HTTPTimeoutSec)
	applyInt(store, siteName, "ai.max_retries", &cfg.MaxRetries)
	applyInt(store, siteName, "ai.retry_backoff_ms", &cfg.RetryBackoffMs)
	applyInt(store, siteName, "ai.history_limit", &cfg.HistoryLimit)

	return cfg
}

func applyInt(store *secret.Store, site, key string, dst *int) {
	v, err := store.Get(site, key)
	if err != nil || v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		*dst = n
	}
}

func applyFloat(store *secret.Store, site, key string, dst *float64) {
	v, err := store.Get(site, key)
	if err != nil || v == "" {
		return
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1.0 {
		*dst = f
	}
}

// ---------------------------------------------------------------------------
// Provider resolution
// ---------------------------------------------------------------------------

func resolveProvider(db *sql.DB, siteName, modelOverride string) (providerKey, apiKey, baseURL, model string) {
	store := secret.NewStore(db)

	providers := []struct{ key, base, defaultModel string }{
		{"openai_api_key", "https://api.openai.com/v1", "gpt-4o"},
		{"deepseek_api_key", "https://api.deepseek.com", "deepseek-v4-pro"},
		{"anthropic_api_key", "https://api.anthropic.com/v1", "claude-sonnet-4-6"},
	}
	for _, p := range providers {
		if k, err := store.Get(siteName, p.key); err == nil && k != "" {
			m := p.defaultModel
			if modelOverride != "" {
				m = modelOverride
			}
			return p.key, k, p.base, m
		}
	}

	// Fallback: shared AI keys from environment (superadmin-configured).
	if os.Getenv("KORA_SHARED_AI_ENABLED") != "true" {
		return "", "", "", ""
	}
	sharedProviders := []struct{ envKey, base, defaultModel string }{
		{"KORA_SHARED_OPENAI_API_KEY", "https://api.openai.com/v1", "gpt-4o"},
		{"KORA_SHARED_DEEPSEEK_API_KEY", "https://api.deepseek.com", "deepseek-v4-pro"},
		{"KORA_SHARED_ANTHROPIC_API_KEY", "https://api.anthropic.com/v1", "claude-sonnet-4-6"},
	}
	for _, p := range sharedProviders {
		if k := os.Getenv(p.envKey); k != "" {
			m := p.defaultModel
			if modelOverride != "" {
				m = modelOverride
			}
			return p.envKey, k, p.base, m
		}
	}
	return "", "", "", ""
}

// ---------------------------------------------------------------------------
// AI provider HTTP call with retry
// ---------------------------------------------------------------------------

func callAI(baseURL, apiKey string, body map[string]any) (map[string]any, error) {
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("AI provider returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing AI response: %w", err)
	}
	return result, nil
}

// callAIWithRetry calls the AI provider with exponential backoff on transient errors.
func callAIWithRetry(baseURL, apiKey string, body map[string]any, cfg AIConfig) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(cfg.RetryBackoffMs) * time.Millisecond * time.Duration(math.Pow(2, float64(attempt-1)))
			slog.Info("Retrying AI call", "attempt", attempt, "backoff_ms", backoff.Milliseconds())
			time.Sleep(backoff)
		}

		result, err := callAI(baseURL, apiKey, body)
		if err == nil {
			return result, nil
		}

		lastErr = err

		// Only retry on transient errors (429, 503, 502, 504).
		if !isTransientError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("AI provider call failed after %d retries: %w", cfg.MaxRetries, lastErr)
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "502") ||
		strings.Contains(msg, "504")
}
