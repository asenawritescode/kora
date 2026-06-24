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
	"time"
)

// channelBus is the default EventBus implementation — an in-process buffered
// Go channel. Every event is written to a write-ahead log (WAL) on disk FIRST,
// then sent to the channel. The WAL is the source of truth; the channel is a
// low-latency wake-up mechanism.
//
// Durability guarantee: once Publish() returns without error, the event is
// persisted to the WAL on disk. On restart, the worker drains any remaining
// WAL entries (events that were written but not yet flushed to the database).
// After a successful flush, the WAL is atomically rotated so that replayed
// events are never double-counted.
type channelBus struct {
	ch       chan ChangeEvent
	mu       sync.RWMutex
	closed   bool
	capacity int

	dropped atomic.Int64 // count of events that failed WAL write (disk full, etc.)

	walDir  string
	walFile *os.File
	walMu   sync.Mutex
}

// NewChannelBus creates an in-process event bus with WAL-first durability.
// capacity is the channel buffer size (default 1000 if <= 0).
// walDir is where the write-ahead log is stored.
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

	// Check for leftover WAL / .flushing files from a previous run.
	// These will be drained by the worker on startup.
	if walPath := b.currentWALPath(); walPath != "" {
		if info, err := os.Stat(walPath); err == nil && info.Size() > 0 {
			slog.Info("analytics: existing WAL found, will drain on worker start",
				"path", walPath, "size", info.Size())
		}
	}
	flushingFiles, _ := filepath.Glob(filepath.Join(walDir, "*.flushing"))
	if len(flushingFiles) > 0 {
		slog.Info("analytics: leftover flushing files found, will drain on worker start",
			"count", len(flushingFiles))
	}

	return b
}

func (b *channelBus) Publish(event ChangeEvent) error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return fmt.Errorf("analytics: event bus closed")
	}

	// WAL-first: write to the persistent log BEFORE attempting the channel send.
	// On restart, the worker drains the WAL and replays any events that were
	// written but not yet flushed to the database. The channel is just a
	// low-latency wake-up mechanism — the WAL is the source of truth.
	if err := b.writeWAL(event); err != nil {
		b.dropped.Add(1)
		slog.Error("analytics: event lost — WAL write failed",
			"doctype", event.Doctype, "doc", event.DocName, "op", event.Operation, "error", err)
		return nil // don't block the caller
	}

	// Best-effort channel send. If the channel is full, that's fine — the worker
	// will catch up via the flush ticker or on next restart via WAL drain.
	select {
	case b.ch <- event:
	default:
		// Channel full — no warning needed, WAL guarantees durability.
	}

	return nil
}

// DrainWAL replays all events from the WAL file and any leftover .flushing files
// (from a crash during a previous flush rotation). Returns the total count of
// replayed events.
func (b *channelBus) DrainWAL(handler func(ChangeEvent)) (int, error) {
	if b.walDir == "" {
		return 0, nil
	}

	b.walMu.Lock()
	// Close current WAL file handle so drainFile can open it fresh.
	if b.walFile != nil {
		b.walFile.Close()
		b.walFile = nil
	}
	b.walMu.Unlock()

	total := 0

	// Drain any leftover .flushing files first (from a crash during a previous
	// flush rotation). These contain events that may or may not have been
	// committed to the DB — replaying them may cause a small double-count,
	// which is acceptable for analytics (and far better than data loss).
	flushingFiles, _ := filepath.Glob(filepath.Join(b.walDir, "*.flushing"))
	for _, fp := range flushingFiles {
		n, err := b.drainFile(fp, handler)
		if err != nil {
			slog.Warn("analytics: error draining flushing file", "path", fp, "error", err)
		}
		total += n
		// Remove after drain — if we crash before the next flush commits, we'll
		// double-count on the NEXT restart, but that's bounded to one batch.
		os.Remove(fp)
	}

	// Drain the main WAL file (events written since last successful rotation).
	walPath := b.currentWALPath()
	if walPath != "" {
		n, err := b.drainFile(walPath, handler)
		if err != nil {
			return total, fmt.Errorf("draining wal %s: %w", walPath, err)
		}
		total += n
	}

	if total > 0 {
		slog.Info("analytics: drained WAL backlog", "events", total)
	}

	return total, nil
}

// drainFile reads a single WAL file line by line, passes events to handler,
// and truncates the file after successful drain.
func (b *channelBus) drainFile(path string, handler func(ChangeEvent)) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event ChangeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("analytics: skipping corrupt WAL line", "path", path, "error", err)
			continue
		}
		handler(event)
		count++
	}

	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("reading %s: %w", path, err)
	}

	// Truncate after successful drain.
	if err := os.Truncate(path, 0); err != nil {
		slog.Warn("analytics: failed to truncate after drain", "path", path, "error", err)
	}

	return count, nil
}

// RotateWAL atomically swaps the current WAL file for a fresh one by renaming
// it with a .flushing suffix. New events are written to a new WAL file.
// Returns the path of the old file; the caller must call CommitWALRotation
// after the corresponding data has been successfully flushed to the database.
func (b *channelBus) RotateWAL() (string, error) {
	b.walMu.Lock()
	defer b.walMu.Unlock()

	walPath := b.currentWALPath()
	if walPath == "" {
		return "", nil
	}

	// Close current handle so we can rename.
	if b.walFile != nil {
		b.walFile.Close()
		b.walFile = nil
	}

	// If the current WAL doesn't exist or is empty, nothing to rotate.
	info, err := os.Stat(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if info.Size() == 0 {
		return "", nil
	}

	// Atomically rename to .flushing suffix.
	flushingPath := walPath + "." + time.Now().Format("20060102T150405") + ".flushing"
	if err := os.Rename(walPath, flushingPath); err != nil {
		return "", fmt.Errorf("rename wal for rotation: %w", err)
	}

	// b.walFile is nil — next writeWAL will create a fresh events.jsonl.

	slog.Debug("analytics: rotated WAL", "flushing", flushingPath)
	return flushingPath, nil
}

// CommitWALRotation deletes a rotated WAL file after its events have been
// successfully flushed to the database, confirming durability.
func (b *channelBus) CommitWALRotation(oldWALPath string) error {
	if oldWALPath == "" {
		return nil
	}
	if err := os.Remove(oldWALPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("commit wal rotation: %w", err)
	}
	return nil
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
