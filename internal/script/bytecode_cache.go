//go:build !gojascript

package script

import (
	"container/list"
	"context"
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/asenawritescode/kora/internal/script/qjs"
)

// bytecodeCacheBytes caps the total bytecode kept across the process.
const bytecodeCacheBytes = 32 << 20

// bytecodeKey identifies compiled bytecode: the reactor build that compiled it and the
// source it came from.
type bytecodeKey struct {
	build [sha256.Size]byte
	src   [sha256.Size]byte
}

// bytecodeEntry is one cached compilation.
type bytecodeEntry struct {
	key  bytecodeKey
	code []byte
}

// bytecodeCache is a bounded LRU of compiled sources. Only successful compilations are
// stored: a syntax error is never cached, so it is reported again on every run. The
// bytecode is produced in this process from trusted sources, never accepted from outside.
type bytecodeCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List // front is most recently used; values are *bytecodeEntry
	index map[bytecodeKey]*list.Element
}

// scriptBytecode is the process-wide cache shared by every runner.
var scriptBytecode = newBytecodeCache(bytecodeCacheBytes)

func newBytecodeCache(maxBytes int) *bytecodeCache {
	return &bytecodeCache{max: maxBytes, order: list.New(), index: make(map[bytecodeKey]*list.Element)}
}

// keyFor returns the cache key of src compiled by the current reactor build.
func keyFor(src string) bytecodeKey {
	return bytecodeKey{build: qjs.BuildID(), src: sha256.Sum256([]byte(src))}
}

// get returns the cached bytecode of key and marks it recently used.
func (c *bytecodeCache) get(key bytecodeKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*bytecodeEntry).code, true
}

// put stores code under key and evicts the least recently used entries until the cache
// fits. Code larger than the whole cache is not stored.
func (c *bytecodeCache) put(key bytecodeKey, code []byte) {
	if len(code) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.index[key] = c.order.PushFront(&bytecodeEntry{key: key, code: code})
	c.size += len(code)
	for c.size > c.max {
		back := c.order.Back()
		entry := c.order.Remove(back).(*bytecodeEntry)
		delete(c.index, entry.key)
		c.size -= len(entry.code)
	}
}

// evalCached runs src as a global script on inst, compiling it once per build. The
// cached bytecode is evaluated with the same mode and result semantics as Eval. A
// compile that fails is returned as the Result (Thrown) and nothing is stored.
func (r *EmbeddedRunner) evalCached(ctx context.Context, inst *qjs.Instance, src string, mode qjs.Mode) (qjs.Result, error) {
	key := keyFor(src)
	code, ok := scriptBytecode.get(key)
	if !ok {
		var res qjs.Result
		var err error
		code, res, err = r.compile(ctx, src)
		if err != nil || res.Thrown {
			return res, err
		}
		scriptBytecode.put(key, code)
	}
	return inst.EvalBytecode(ctx, code, mode)
}

// compile returns the bytecode of src. It compiles on a throwaway instance, never on the
// one that runs the script: reading bytecode back into the runtime that compiled it
// fails with an out-of-bounds access for a library of the real erp-accounting pack.
// Only a cache miss pays for the extra instance.
func (r *EmbeddedRunner) compile(ctx context.Context, src string) ([]byte, qjs.Result, error) {
	scratch, err := r.newPrewarmed(ctx)
	if err != nil {
		return nil, qjs.Result{}, fmt.Errorf("script: compile instance: %w", err)
	}
	defer func() { _ = scratch.Close(context.WithoutCancel(ctx)) }() // best effort: the bytecode is already taken
	return scratch.Compile(ctx, src)
}
