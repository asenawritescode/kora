package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var ErrRealtimeStreamCapacity = errors.New("realtime stream capacity exhausted")

const (
	DefaultRealtimeMaxStreams = 128
	MaxRealtimeMaxStreams     = 4096
)

// RealtimeStreamAdmission is a process-owned, fail-fast limit for long-lived
// realtime connections. It is separate from database work admission because
// streams retain a slot for their lifetime but perform only intermittent DB
// authorization checks.
type RealtimeStreamAdmission struct {
	permits  chan struct{}
	rejected atomic.Uint64
}

type RealtimeStreamSnapshot struct {
	Active   int
	Capacity int
	Rejected uint64
}

func NewRealtimeStreamAdmission(maxStreams int) (*RealtimeStreamAdmission, error) {
	if maxStreams <= 0 || maxStreams > MaxRealtimeMaxStreams {
		return nil, errors.New("realtime stream capacity must be between 1 and 4096")
	}
	return &RealtimeStreamAdmission{permits: make(chan struct{}, maxStreams)}, nil
}

func (a *RealtimeStreamAdmission) Acquire(ctx context.Context) (func(), error) {
	if a == nil {
		return nil, errors.New("realtime stream admission is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case a.permits <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-a.permits
			a.rejected.Add(1)
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { <-a.permits }) }, nil
	default:
		a.rejected.Add(1)
		return nil, ErrRealtimeStreamCapacity
	}
}

func (a *RealtimeStreamAdmission) Snapshot() RealtimeStreamSnapshot {
	if a == nil {
		return RealtimeStreamSnapshot{}
	}
	return RealtimeStreamSnapshot{Active: len(a.permits), Capacity: cap(a.permits), Rejected: a.rejected.Load()}
}
