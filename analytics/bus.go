package analytics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// channelBus is the default EventBus implementation — an in-process buffered
// Go channel. When the channel is full, events spill to a write-ahead log (WAL)
// on disk so they are not lost. The worker drains the WAL on startup.
type channelBus struct {
	ch       chan ChangeEvent
	mu       sync.RWMutex
	closed   bool
	capacity int

	dropped atomic.Int64 // count of events spilled to WAL

	walDir  string
	walFile *os.File
	walMu   sync.Mutex
}

// NewChannelBus creates an in-process event bus.
// capacity is the channel buffer size (default 1000 if <= 0).
// walDir is where spilled events are written when the channel is full.
func NewChannelBus(capacity int, walDir string) EventBus {
	if capacity <= 0 {
		capacity = 1000
	}
	if walDir == "" {
		walDir = "data/analytics/wal"
	}

	b := &channelBus{
		ch:       make(chan ChangeEvent, capacity),
		capacity: capacity,
		walDir:   walDir,
	}

	// Try to open WAL for draining any existing backlog.
	if walPath := b.currentWALPath(); walPath != "" {
		if f, err := os.Open(walPath); err == nil {
			// WAL exists from a previous run — it will be drained by the worker.
			f.Close()
			slog.Info("analytics: existing WAL found, will drain on worker start",
				"path", walPath)
		}
	}

	return b
}

func (b *channelBus) Publish(event ChangeEvent) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return fmt.Errorf("analytics: event bus closed")
	}

	select {
	case b.ch <- event:
		return nil
	default:
		// Channel full — spill to WAL.
		if err := b.writeWAL(event); err != nil {
			slog.Error("analytics: event lost — channel full and WAL write failed",
				"doctype", event.Doctype, "doc", event.DocName, "op", event.Operation, "error", err)
			return nil // don't block the caller
		}
		b.dropped.Add(1)
		slog.Warn("analytics: channel full, event spilled to WAL",
			"doctype", event.Doctype, "doc", event.DocName, "op", event.Operation)
		return nil
	}
}

// DrainWAL reads any events from the WAL file and passes them to the handler.
// After successful replay, the WAL file is truncated. Returns the count of
// replayed events.
func (b *channelBus) DrainWAL(handler func(ChangeEvent)) (int, error) {
	walPath := b.currentWALPath()
	if walPath == "" {
		return 0, nil
	}

	b.walMu.Lock()
	defer b.walMu.Unlock()

	// Close current WAL file handle if open.
	if b.walFile != nil {
		b.walFile.Close()
		b.walFile = nil
	}

	f, err := os.Open(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open wal for drain: %w", err)
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event ChangeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("analytics: skipping corrupt WAL line", "error", err)
			continue
		}
		handler(event)
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("reading wal: %w", err)
	}

	// Truncate WAL after successful drain.
	if err := os.Truncate(walPath, 0); err != nil {
		slog.Warn("analytics: failed to truncate WAL after drain", "error", err)
	}

	if count > 0 {
		slog.Info("analytics: drained WAL backlog", "events", count)
	}

	return count, nil
}

func (b *channelBus) Subscribe() (<-chan ChangeEvent, error) {
	return b.ch, nil
}

func (b *channelBus) Dropped() int64 {
	return b.dropped.Load()
}

func (b *channelBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		close(b.ch)
		b.closed = true
	}
	b.walMu.Lock()
	defer b.walMu.Unlock()
	if b.walFile != nil {
		b.walFile.Close()
		b.walFile = nil
	}
	return nil
}

// --- WAL (write-ahead log) ---

func (b *channelBus) currentWALPath() string {
	if b.walDir == "" {
		return ""
	}
	return filepath.Join(b.walDir, "events.jsonl")
}

func (b *channelBus) writeWAL(event ChangeEvent) error {
	b.walMu.Lock()
	defer b.walMu.Unlock()

	if b.walFile == nil {
		if err := os.MkdirAll(b.walDir, 0755); err != nil {
			return fmt.Errorf("create wal dir: %w", err)
		}
		f, err := os.OpenFile(b.currentWALPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open wal: %w", err)
		}
		b.walFile = f
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')

	if _, err := b.walFile.Write(data); err != nil {
		return fmt.Errorf("write wal: %w", err)
	}
	return nil
}
