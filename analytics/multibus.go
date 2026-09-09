package analytics

import (
	"sync"
	"sync/atomic"
)

// MultiBus wraps an EventBus and fans out events to multiple subscribers.
// The primary subscriber continues using EventBus.Subscribe().
// Additional subscribers use MultiBus.AddListener().
type MultiBus struct {
	inner    EventBus
	mu       sync.RWMutex
	channels []chan ChangeEvent
	closed   bool
	dropped  atomic.Int64
}

func (mb *MultiBus) Publish(event ChangeEvent) error { return mb.inner.Publish(event) }

// Subscribe registers a buffered listener. Long-lived consumers should remove
// their listener explicitly when their context ends.
func (mb *MultiBus) Subscribe() (<-chan ChangeEvent, error) {
	ch := make(chan ChangeEvent, 256)
	mb.AddListener(ch)
	return ch, nil
}

func (mb *MultiBus) DrainWAL(handler func(ChangeEvent)) (int, error) {
	return mb.inner.DrainWAL(handler)
}
func (mb *MultiBus) RotateWAL() (string, error)          { return mb.inner.RotateWAL() }
func (mb *MultiBus) CommitWALRotation(path string) error { return mb.inner.CommitWALRotation(path) }

// NewMultiBus creates a fan-out wrapper around an existing EventBus.
// It starts a goroutine that reads from the inner bus and fans out to all listeners.
func NewMultiBus(inner EventBus) (*MultiBus, error) {
	mb := &MultiBus{inner: inner}

	// Subscribe to the inner bus and fan out.
	ch, err := inner.Subscribe()
	if err != nil {
		return nil, err
	}

	go func() {
		for event := range ch {
			mb.mu.RLock()
			if mb.closed {
				mb.mu.RUnlock()
				return
			}
			for _, listener := range mb.channels {
				select {
				case listener <- event:
				default:
					mb.dropped.Add(1)
				}
			}
			mb.mu.RUnlock()
		}
	}()

	return mb, nil
}

// AddListener registers a new subscriber channel. Events are fanned out to all listeners.
// The channel should be buffered to avoid blocking the fan-out goroutine.
func (mb *MultiBus) AddListener(ch chan ChangeEvent) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.channels = append(mb.channels, ch)
}

// RemoveListener removes a subscriber channel.
func (mb *MultiBus) RemoveListener(ch chan ChangeEvent) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	for i, c := range mb.channels {
		if c == ch {
			mb.channels = append(mb.channels[:i], mb.channels[i+1:]...)
			return
		}
	}
}

// ListenerCount returns the number of listeners.
func (mb *MultiBus) ListenerCount() int {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	return len(mb.channels)
}

// Dropped returns the number of fan-out events dropped for slow listeners.
func (mb *MultiBus) Dropped() int64 { return mb.inner.Dropped() + mb.dropped.Load() }

// Close shuts down the fan-out and closes the inner bus.
func (mb *MultiBus) Close() error {
	mb.mu.Lock()
	mb.closed = true
	for _, ch := range mb.channels {
		close(ch)
	}
	mb.channels = nil
	mb.mu.Unlock()
	return mb.inner.Close()
}

// Inner returns the wrapped EventBus.
func (mb *MultiBus) Inner() EventBus { return mb.inner }
