package api

import (
	"strconv"

	"github.com/asenawritescode/kora/secret"
)

// AIConfig holds all configurable thresholds for the AI chat pipeline.
// Every value has a sensible default; site-level overrides are loaded
// from the secret store with an "ai." key prefix.
type AIConfig struct {
	MaxRounds          int     // Max tool-calling rounds (safety cap)
	TokenBudget        int     // Context window budget in tokens
	CompactionThreshold float64 // 0.0-1.0, when to start compacting history
	MaxToolResultChars int     // Cap on a single tool result string
	StallThreshold     int     // Consecutive identical tool calls before nudge
	MaxToolErrors      int     // Total tool errors before circuit breaker
	MaxTokensPerCall   int     // max_tokens per AI API call
	HTTPTimeoutSec     int     // Timeout in seconds for AI provider HTTP calls
	MaxRetries         int     // Retries on transient AI errors (429, 503)
	RetryBackoffMs     int     // Base backoff in milliseconds
	HistoryLimit       int     // Max incoming history messages from client
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

