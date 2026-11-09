package api

import (
	"sync"
)

// RealtimeConnectionOwner tracks hijacked WebSocket connections, which
// net/http.Server.Shutdown does not close or wait for. The process owner calls
// Close before tenant runtimes and their database pools are retired.
type RealtimeConnectionOwner struct {
	mu      sync.Mutex
	closed  bool
	streams map[uint64]func()
	nextID  uint64
}

func NewRealtimeConnectionOwner() *RealtimeConnectionOwner {
	return &RealtimeConnectionOwner{streams: make(map[uint64]func())}
}

// Register adds a connection and returns an idempotent unregister function.
// Connections accepted after shutdown begins are closed immediately.
func (o *RealtimeConnectionOwner) Register(closeStream func()) func() {
	if o == nil || closeStream == nil {
		return func() {}
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		closeStream()
		return func() {}
	}
	o.nextID++
	id := o.nextID
	o.streams[id] = closeStream
	o.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			delete(o.streams, id)
			o.mu.Unlock()
		})
	}
}

// Close terminates active WebSockets immediately and prevents new streams
// from surviving process shutdown.
func (o *RealtimeConnectionOwner) Close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.closed = true
	streams := make([]func(), 0, len(o.streams))
	for _, closeStream := range o.streams {
		streams = append(streams, closeStream)
	}
	clear(o.streams)
	o.mu.Unlock()
	for _, closeStream := range streams {
		closeStream()
	}
}
