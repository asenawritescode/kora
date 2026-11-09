package api

import (
	"container/list"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/asenawritescode/kora/internal/analytics"
)

type analyticsQueryCacheEntry struct {
	key       string
	value     []byte
	bytes     uint64
	expiresAt time.Time
}

type analyticsQueryCache struct {
	mu        sync.Mutex
	items     map[string]*list.Element
	lru       list.List
	ttl       time.Duration
	maxSize   int
	maxBytes  uint64
	maxEntry  uint64
	usedBytes uint64
	evictions uint64
	rejected  uint64
}

type AnalyticsQueryCacheSnapshot struct {
	Entries    int
	MaxEntries int
	Bytes      uint64
	MaxBytes   uint64
	Evictions  uint64
	Rejected   uint64
}

// AnalyticsQueryCache is a process-owned semantic result cache. Tenant service
// snapshots select keys and permissions, but do not own this shared budget.
type AnalyticsQueryCache struct {
	cache *analyticsQueryCache
}

var (
	defaultAnalyticsCacheOnce sync.Once
	defaultAnalyticsCache     *AnalyticsQueryCache
)

func NewAnalyticsQueryCache() *AnalyticsQueryCache {
	return &AnalyticsQueryCache{cache: newAnalyticsQueryCache(30*time.Second, 256)}
}

func DefaultAnalyticsQueryCache() *AnalyticsQueryCache {
	defaultAnalyticsCacheOnce.Do(func() { defaultAnalyticsCache = NewAnalyticsQueryCache() })
	return defaultAnalyticsCache
}

func (c *AnalyticsQueryCache) AnalyticsQueryCacheSnapshot() AnalyticsQueryCacheSnapshot {
	if c == nil || c.cache == nil {
		return AnalyticsQueryCacheSnapshot{}
	}
	return c.cache.snapshot()
}

const (
	defaultAnalyticsQueryCacheMaxBytes uint64 = 32 << 20
	defaultAnalyticsQueryCacheMaxEntry uint64 = 1 << 20
	analyticsQueryCacheEntryOverhead   uint64 = 128
)

func newAnalyticsQueryCache(ttl time.Duration, maxSize int) *analyticsQueryCache {
	return newAnalyticsQueryCacheWithBudget(ttl, maxSize, defaultAnalyticsQueryCacheMaxBytes, defaultAnalyticsQueryCacheMaxEntry)
}

func newAnalyticsQueryCacheWithBudget(ttl time.Duration, maxSize int, maxBytes, maxEntry uint64) *analyticsQueryCache {
	if maxSize < 1 {
		maxSize = 1
	}
	if maxBytes == 0 {
		maxBytes = defaultAnalyticsQueryCacheMaxBytes
	}
	if maxEntry == 0 || maxEntry > maxBytes {
		maxEntry = maxBytes
	}
	return &analyticsQueryCache{items: make(map[string]*list.Element), ttl: ttl, maxSize: maxSize, maxBytes: maxBytes, maxEntry: maxEntry}
}

func (cache *analyticsQueryCache) key(tenant string, configRevision uint64, permissionScope string, request analytics.SemanticRequest) string {
	data, _ := json.Marshal(request)
	return tenant + "\x00" + strconv.FormatUint(configRevision, 10) + "\x00" + permissionScope + "\x00" + string(data)
}

func (cache *analyticsQueryCache) get(key string) (json.RawMessage, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	element, ok := cache.items[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(*analyticsQueryCacheEntry)
	if time.Now().After(entry.expiresAt) {
		cache.remove(element)
		return nil, false
	}
	cache.lru.MoveToFront(element)
	return json.RawMessage(entry.value), true
}

func (cache *analyticsQueryCache) snapshot() AnalyticsQueryCacheSnapshot {
	if cache == nil {
		return AnalyticsQueryCacheSnapshot{}
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return AnalyticsQueryCacheSnapshot{Entries: len(cache.items), MaxEntries: cache.maxSize, Bytes: cache.usedBytes, MaxBytes: cache.maxBytes, Evictions: cache.evictions, Rejected: cache.rejected}
}

func (cache *analyticsQueryCache) put(key string, value *analytics.SemanticQueryResponse) bool {
	if cache == nil || value == nil || key == "" {
		return false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		cache.mu.Lock()
		cache.rejected++
		cache.mu.Unlock()
		return false
	}
	entryBytes := uint64(len(key)) + uint64(len(encoded)) + analyticsQueryCacheEntryOverhead
	if entryBytes > cache.maxEntry || entryBytes > cache.maxBytes {
		cache.mu.Lock()
		cache.rejected++
		cache.mu.Unlock()
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.removeExpired(time.Now())
	if previous := cache.items[key]; previous != nil {
		cache.remove(previous)
	}
	for len(cache.items) >= cache.maxSize || cache.usedBytes+entryBytes > cache.maxBytes {
		oldest := cache.lru.Back()
		if oldest == nil {
			cache.rejected++
			return false
		}
		cache.evictions++
		cache.remove(oldest)
	}
	entry := &analyticsQueryCacheEntry{key: key, value: encoded, bytes: entryBytes, expiresAt: time.Now().Add(cache.ttl)}
	cache.items[key] = cache.lru.PushFront(entry)
	cache.usedBytes += entryBytes
	return true
}

func (cache *analyticsQueryCache) removeExpired(now time.Time) {
	for element := cache.lru.Back(); element != nil; {
		previous := element.Prev()
		if now.After(element.Value.(*analyticsQueryCacheEntry).expiresAt) {
			cache.remove(element)
		}
		element = previous
	}
}

func (cache *analyticsQueryCache) remove(element *list.Element) {
	if cache == nil || element == nil {
		return
	}
	entry := element.Value.(*analyticsQueryCacheEntry)
	delete(cache.items, entry.key)
	cache.lru.Remove(element)
	if entry.bytes >= cache.usedBytes {
		cache.usedBytes = 0
	} else {
		cache.usedBytes -= entry.bytes
	}
}
