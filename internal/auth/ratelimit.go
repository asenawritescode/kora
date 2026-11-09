package auth

import (
	"sync"
	"time"
)

type rateLimitEntry struct {
	count   int
	resetAt time.Time
}

var (
	authRateLimitMu sync.Mutex
	authRateLimits  = map[string]*rateLimitEntry{}
)

func allowAuthRequest(key string, limit int, window time.Duration) (bool, time.Duration) {
	now := time.Now()

	authRateLimitMu.Lock()
	defer authRateLimitMu.Unlock()

	entry, ok := authRateLimits[key]
	if !ok || now.After(entry.resetAt) {
		authRateLimits[key] = &rateLimitEntry{
			count:   1,
			resetAt: now.Add(window),
		}
		return true, 0
	}

	entry.count++
	if entry.count > limit {
		return false, time.Until(entry.resetAt)
	}
	return true, 0
}

func cleanupAuthRateLimits() {
	now := time.Now()
	authRateLimitMu.Lock()
	defer authRateLimitMu.Unlock()
	for key, entry := range authRateLimits {
		if now.After(entry.resetAt.Add(5 * time.Minute)) {
			delete(authRateLimits, key)
		}
	}
}
