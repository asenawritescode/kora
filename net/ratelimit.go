package net

import (
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// RateLimiter implements per-user token bucket rate limiting.
type RateLimiter struct {
	mu          sync.Mutex
	limiters    map[string]*rateLimiterEntry
	rate        rate.Limit
	burst       int
	nextCleanup time.Time
}

const rateLimiterCleanupInterval = 5 * time.Minute

type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// NewRateLimiter creates a rate limiter.
// rps: requests per second allowed per user. burst: max burst size.
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	if rps <= 0 {
		rps = 100
	}
	if burst <= 0 {
		burst = 20
	}

	rl := &RateLimiter{
		limiters:    make(map[string]*rateLimiterEntry),
		rate:        rate.Limit(rps),
		burst:       burst,
		nextCleanup: time.Now().Add(rateLimiterCleanupInterval),
	}

	return rl
}

// Middleware returns a Gin middleware that rate-limits requests per user.
func (rl *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Build key: "site:user" or "site:ip" if not authenticated.
		site, _ := c.Get("site_name")
		user, _ := c.Get("user")
		key := fmt.Sprintf("%v:%v", site, user)
		if user == "" || user == nil {
			key = fmt.Sprintf("%v:ip:%v", site, c.ClientIP())
		}

		limiter := rl.getLimiter(key)
		if !limiter.Allow() {
			c.AbortWithStatusJSON(429, gin.H{
				"error":   "rate_limit_exceeded",
				"message": "Too many requests. Please try again later.",
			})
			c.Header("Retry-After", "1")
			return
		}
		c.Next()
	}
}

func (rl *RateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if !now.Before(rl.nextCleanup) {
		cutoff := now.Add(-rateLimiterCleanupInterval)
		for existingKey, entry := range rl.limiters {
			if entry.lastUsed.Before(cutoff) {
				delete(rl.limiters, existingKey)
			}
		}
		rl.nextCleanup = now.Add(rateLimiterCleanupInterval)
	}

	entry, ok := rl.limiters[key]
	if !ok {
		entry = &rateLimiterEntry{
			limiter: rate.NewLimiter(rl.rate, rl.burst),
		}
		rl.limiters[key] = entry
	}
	entry.lastUsed = now
	return entry.limiter
}
