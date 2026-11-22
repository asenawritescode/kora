package eventbus

import (
	"log/slog"
	"sync"
	"time"
)

// overflowInterval bounds how often a saturated queue writes a warning.
// Counters record every drop; the log only shows that drops are happening.
const overflowInterval = 30 * time.Second

// overflowWarning is the process-wide rate limiter for drop warnings.
var overflowWarning = &rateLimitedWarning{interval: overflowInterval}

type rateLimitedWarning struct {
	mu       sync.Mutex
	interval time.Duration
	last     time.Time
	now      func() time.Time
}

func (w *rateLimitedWarning) warn(msg string) {
	w.mu.Lock()
	now := time.Now()
	if w.now != nil {
		now = w.now()
	}
	if !w.last.IsZero() && now.Sub(w.last) < w.interval {
		w.mu.Unlock()
		return
	}
	w.last = now
	w.mu.Unlock()
	slog.Warn(msg)
}
