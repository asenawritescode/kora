// Package tokencache caches extension token verification for one Engine
// process. Invalidation is local: other replicas observe a revocation only
// after their entry expires, so TTL bounds cross-replica revocation lag.
package tokencache

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/asenawritescode/kora/extension"
)

// MaxTTL caps how long a revoked token can remain valid on another replica.
const MaxTTL = 30 * time.Second

// Verifier resolves the active extension owning a token verifier.
type Verifier interface {
	VerifyAccessToken(ctx context.Context, site, tokenHash string) (extension.Credential, error)
}

// Config sets entry lifetimes and capacity. Zero NegativeTTL disables caching
// of unknown tokens.
type Config struct {
	TTL         time.Duration
	NegativeTTL time.Duration
	MaxEntries  int
	Now         func() time.Time
}

type key struct{ site, tokenHash string }

type entry struct {
	credential extension.Credential
	found      bool
	expires    time.Time
}

// Cache is a bounded TTL cache of verification results keyed by site and token hash.
type Cache struct {
	mu      sync.Mutex
	cfg     Config
	entries map[key]entry
}

// New returns a cache, clamping TTL to MaxTTL and NegativeTTL to 5s.
func New(cfg Config) *Cache {
	cfg.TTL = min(max(cfg.TTL, 0), MaxTTL)
	cfg.NegativeTTL = min(max(cfg.NegativeTTL, 0), 5*time.Second)
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = 10000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Cache{cfg: cfg, entries: make(map[key]entry)}
}

// Wrap returns a Verifier that consults the cache before next.
func (c *Cache) Wrap(next Verifier) Verifier {
	return cachedVerifier{cache: c, next: next}
}

// InvalidateSite drops every cached result for site.
func (c *Cache) InvalidateSite(site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.site == site {
			delete(c.entries, k)
		}
	}
}

func (c *Cache) get(k key) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return entry{}, false
	}
	if !c.cfg.Now().Before(e.expires) {
		delete(c.entries, k)
		return entry{}, false
	}
	return e, true
}

func (c *Cache) put(k key, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.cfg.MaxEntries {
		now := c.cfg.Now()
		for existing, old := range c.entries {
			if !now.Before(old.expires) {
				delete(c.entries, existing)
			}
		}
		for existing := range c.entries {
			if len(c.entries) < c.cfg.MaxEntries {
				break
			}
			delete(c.entries, existing)
		}
	}
	c.entries[k] = e
}

type cachedVerifier struct {
	cache *Cache
	next  Verifier
}

func (v cachedVerifier) VerifyAccessToken(ctx context.Context, site, tokenHash string) (extension.Credential, error) {
	k := key{site: site, tokenHash: tokenHash}
	if e, ok := v.cache.get(k); ok {
		if !e.found {
			return extension.Credential{}, extension.ErrNotFound
		}
		return e.credential, nil
	}
	credential, err := v.next.VerifyAccessToken(ctx, site, tokenHash)
	now := v.cache.cfg.Now()
	switch {
	case err == nil && v.cache.cfg.TTL > 0:
		v.cache.put(k, entry{credential: credential, found: true, expires: now.Add(v.cache.cfg.TTL)})
	case errors.Is(err, extension.ErrNotFound) && v.cache.cfg.NegativeTTL > 0:
		v.cache.put(k, entry{expires: now.Add(v.cache.cfg.NegativeTTL)})
	}
	return credential, err
}
