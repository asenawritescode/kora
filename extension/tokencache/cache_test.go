package tokencache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/asenawritescode/kora/extension"
)

type countingVerifier struct {
	calls  int
	active map[string]extension.Credential
}

func (v *countingVerifier) VerifyAccessToken(_ context.Context, site, tokenHash string) (extension.Credential, error) {
	v.calls++
	c, ok := v.active[site+"/"+tokenHash]
	if !ok {
		return extension.Credential{}, extension.ErrNotFound
	}
	return c, nil
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newFixture(cfg Config) (*Cache, Verifier, *countingVerifier, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_000, 0)}
	cfg.Now = clock.Now
	backing := &countingVerifier{active: map[string]extension.Credential{"acme/h1": {Name: "bot"}}}
	cache := New(cfg)
	return cache, cache.Wrap(backing), backing, clock
}

func TestCachedTokenExpiresAfterTTL(t *testing.T) {
	_, verifier, backing, clock := newFixture(Config{TTL: 30 * time.Second})
	ctx := context.Background()
	for range 3 {
		if _, err := verifier.VerifyAccessToken(ctx, "acme", "h1"); err != nil {
			t.Fatal(err)
		}
	}
	if backing.calls != 1 {
		t.Fatalf("backing calls = %d, want 1 within TTL", backing.calls)
	}
	delete(backing.active, "acme/h1")
	clock.now = clock.now.Add(30 * time.Second)
	if _, err := verifier.VerifyAccessToken(ctx, "acme", "h1"); !errors.Is(err, extension.ErrNotFound) {
		t.Fatalf("after TTL err = %v, want ErrNotFound", err)
	}
}

func TestInvalidateSiteObservesRevocationImmediately(t *testing.T) {
	cache, verifier, backing, _ := newFixture(Config{TTL: 30 * time.Second})
	ctx := context.Background()
	if _, err := verifier.VerifyAccessToken(ctx, "acme", "h1"); err != nil {
		t.Fatal(err)
	}
	delete(backing.active, "acme/h1")
	cache.InvalidateSite("other")
	if _, err := verifier.VerifyAccessToken(ctx, "acme", "h1"); err != nil {
		t.Fatalf("other-site invalidation evicted acme: %v", err)
	}
	cache.InvalidateSite("acme")
	if _, err := verifier.VerifyAccessToken(ctx, "acme", "h1"); !errors.Is(err, extension.ErrNotFound) {
		t.Fatalf("after invalidation err = %v, want ErrNotFound", err)
	}
}

func TestNegativeResults(t *testing.T) {
	tests := []struct {
		name        string
		negativeTTL time.Duration
		wantCalls   int
	}{
		{"disabled", 0, 2},
		{"cached", 5 * time.Second, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, verifier, backing, _ := newFixture(Config{TTL: 30 * time.Second, NegativeTTL: tt.negativeTTL})
			for range 2 {
				_, _ = verifier.VerifyAccessToken(context.Background(), "acme", "unknown")
			}
			if backing.calls != tt.wantCalls {
				t.Fatalf("backing calls = %d, want %d", backing.calls, tt.wantCalls)
			}
		})
	}
}

func TestTTLIsClampedAndCapacityBounded(t *testing.T) {
	cache := New(Config{TTL: time.Hour, NegativeTTL: time.Minute, MaxEntries: 2})
	if cache.cfg.TTL != MaxTTL || cache.cfg.NegativeTTL != 5*time.Second {
		t.Fatalf("clamped TTLs = %v/%v", cache.cfg.TTL, cache.cfg.NegativeTTL)
	}
	backing := &countingVerifier{active: map[string]extension.Credential{"a/1": {}, "a/2": {}, "a/3": {}}}
	verifier := cache.Wrap(backing)
	for _, h := range []string{"1", "2", "3"} {
		_, _ = verifier.VerifyAccessToken(context.Background(), "a", h)
	}
	if len(cache.entries) > 2 {
		t.Fatalf("entries = %d, want <= 2", len(cache.entries))
	}
}
