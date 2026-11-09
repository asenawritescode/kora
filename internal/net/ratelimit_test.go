package net

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestRateLimiterCleansStaleEntriesOnRequestWithoutBackgroundWorker(t *testing.T) {
	limiter := NewRateLimiter(10, 2)
	now := time.Now()
	limiter.mu.Lock()
	limiter.limiters["stale"] = &rateLimiterEntry{limiter: rate.NewLimiter(10, 2), lastUsed: now.Add(-rateLimiterCleanupInterval - time.Second)}
	limiter.limiters["active"] = &rateLimiterEntry{limiter: rate.NewLimiter(10, 2), lastUsed: now.Add(-time.Minute)}
	limiter.nextCleanup = now.Add(-time.Second)
	limiter.mu.Unlock()

	limiter.getLimiter("new")

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, exists := limiter.limiters["stale"]; exists {
		t.Fatal("stale limiter survived opportunistic cleanup")
	}
	if _, exists := limiter.limiters["active"]; !exists {
		t.Fatal("active limiter was removed by opportunistic cleanup")
	}
	if _, exists := limiter.limiters["new"]; !exists {
		t.Fatal("request limiter was not created")
	}
	if len(limiter.limiters) != 2 {
		t.Fatalf("retained limiter count = %d, want active and new entries", len(limiter.limiters))
	}
}
