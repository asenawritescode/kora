package analytics

import "sync"

// MultiBus wraps an EventBus and fans out events to multiple subscribers.
// The primary subscriber continues using EventBus.Subscribe().
// Additional subscribers use MultiBus.AddListener().
type MultiBus struct {
	inner    EventBus
	mu       sync.RWMutex
	channels []chan ChangeEvent
	closed   bool
}

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
					// Listener is too slow — drop the event for this listener.
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
