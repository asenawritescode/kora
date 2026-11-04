package auth

import (
	"errors"
	"sync/atomic"
)

const (
	DefaultSessionCacheBudgetBytes uint64 = 256 << 20
	MaxSessionCacheBudgetBytes     uint64 = 4 << 30
)

// SessionCacheBudget caps retained session snapshots shared by the Engine's
// process and tenant session managers. A full budget disables new cache
// retention until existing entries expire or are evicted; authentication still
// proceeds from the database result for that request.
type SessionCacheBudget struct {
	maxBytes uint64
	used     atomic.Uint64
}

type SessionCacheBudgetSnapshot struct {
	Bytes    uint64
	MaxBytes uint64
}

func NewSessionCacheBudget(maxBytes uint64) (*SessionCacheBudget, error) {
	if maxBytes == 0 || maxBytes > MaxSessionCacheBudgetBytes {
		return nil, errors.New("session cache budget must be between 1 byte and 4 GiB")
	}
	return &SessionCacheBudget{maxBytes: maxBytes}, nil
}

func (b *SessionCacheBudget) Snapshot() SessionCacheBudgetSnapshot {
	if b == nil {
		return SessionCacheBudgetSnapshot{}
	}
	return SessionCacheBudgetSnapshot{Bytes: b.used.Load(), MaxBytes: b.maxBytes}
}

func (b *SessionCacheBudget) acquire(bytes uint64) bool {
	if b == nil || bytes == 0 {
		return b == nil
	}
	for {
		used := b.used.Load()
		if bytes > b.maxBytes || used > b.maxBytes-bytes {
			return false
		}
		if b.used.CompareAndSwap(used, used+bytes) {
			return true
		}
	}
}

func (b *SessionCacheBudget) release(bytes uint64) {
	if b == nil || bytes == 0 {
		return
	}
	for {
		used := b.used.Load()
		next := uint64(0)
		if used > bytes {
			next = used - bytes
		}
		if b.used.CompareAndSwap(used, next) {
			return
		}
	}
}
