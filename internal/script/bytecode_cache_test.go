//go:build !gojascript

package script

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBytecodeCacheEviction(t *testing.T) {
	key := func(s string) bytecodeKey { return keyFor(s) }
	tests := []struct {
		name     string
		max      int
		puts     []string // sources, each stored as 10 bytes of code
		wantHeld []string // still cached after the puts
		wantGone []string
	}{
		{name: "fits", max: 30, puts: []string{"a", "b", "c"}, wantHeld: []string{"a", "b", "c"}},
		{name: "oldest evicted", max: 20, puts: []string{"a", "b", "c"}, wantHeld: []string{"b", "c"}, wantGone: []string{"a"}},
		{name: "get refreshes recency", max: 20, puts: []string{"a", "b", "a", "c"}, wantHeld: []string{"a", "c"}, wantGone: []string{"b"}},
		{name: "oversized not stored", max: 5, puts: []string{"a"}, wantGone: []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newBytecodeCache(tc.max)
			for _, src := range tc.puts {
				if _, ok := c.get(key(src)); !ok {
					c.put(key(src), bytes.Repeat([]byte{1}, 10))
				}
			}
			for _, src := range tc.wantHeld {
				if _, ok := c.get(key(src)); !ok {
					t.Errorf("%q evicted, want held", src)
				}
			}
			for _, src := range tc.wantGone {
				if _, ok := c.get(key(src)); ok {
					t.Errorf("%q held, want evicted", src)
				}
			}
			if c.size > tc.max {
				t.Errorf("size %d over cap %d", c.size, tc.max)
			}
		})
	}
}

func TestSyntaxErrorIsNotCached(t *testing.T) {
	const bad = `var = ;`
	r := NewEmbeddedRunner(DefaultEmbeddedConfig())
	defer func() { _ = r.Close() }()
	req := ExecuteRequest{Script: `return {result: 1};`, Libraries: []Library{{Name: "broken", Source: bad}}}
	for range 2 {
		_, err := r.Execute(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), `library "broken" failed to load`) {
			t.Fatalf("Execute = %v; want the library load error every time", err)
		}
	}
	scriptBytecode.mu.Lock()
	_, cached := scriptBytecode.index[keyFor(bad)]
	scriptBytecode.mu.Unlock()
	if cached {
		t.Fatal("a syntax error must not be cached")
	}
}

// TestCachedBytecodeRunsLikeSource runs the real books library twice: the first run
// compiles it, the second runs the cached bytecode. Both must give the same answer, and
// the library must be cached afterwards.
func TestCachedBytecodeRunsLikeSource(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(packConfigRoot, "erp-accounting", "scripts", "lib", "books.js"))
	if err != nil {
		t.Fatalf("read books.js: %v", err)
	}
	req := ExecuteRequest{
		Script:    `return {result: typeof postEntry + ":" + typeof postingMoney(1.234)};`,
		Libraries: []Library{{Name: "lib_books", Source: string(src)}},
	}
	r := NewEmbeddedRunner(DefaultEmbeddedConfig())
	defer func() { _ = r.Close() }()
	var got [2]any
	for i := range got {
		res, err := r.Execute(context.Background(), req)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		got[i] = res.Result
	}
	if got[0] != got[1] || got[0] != "function:number" {
		t.Fatalf("results %v; want the same answer, function:number, on compile and cache hit", got)
	}
	if _, ok := scriptBytecode.get(keyFor(string(src))); !ok {
		t.Fatal("library bytecode was not cached")
	}
}
