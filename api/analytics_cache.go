package api

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/asenawritescode/kora/analytics"
)

type analyticsQueryCacheEntry struct {
	value     *analytics.SemanticQueryResponse
	expiresAt time.Time
}

type analyticsQueryCache struct {
	mu      sync.Mutex
	items   map[string]analyticsQueryCacheEntry
	ttl     time.Duration
	maxSize int
}

func newAnalyticsQueryCache(ttl time.Duration, maxSize int) *analyticsQueryCache {
	return &analyticsQueryCache{items: make(map[string]analyticsQueryCacheEntry), ttl: ttl, maxSize: maxSize}
}

func (cache *analyticsQueryCache) key(site string, request analytics.AnalyticsQueryRequest) string {
	data, _ := json.Marshal(request)
	return site + "\x00" + string(data)
}

func (cache *analyticsQueryCache) get(key string) (*analytics.SemanticQueryResponse, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.items[key]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(cache.items, key)
		return nil, false
	}
	return entry.value, true
}

func (cache *analyticsQueryCache) put(key string, value *analytics.SemanticQueryResponse) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.items) >= cache.maxSize {
		for oldestKey := range cache.items {
			delete(cache.items, oldestKey)
			break
		}
	}
	cache.items[key] = analyticsQueryCacheEntry{value: value, expiresAt: time.Now().Add(cache.ttl)}
}
