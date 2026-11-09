package analytics

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// channelBus is the default EventBus implementation — an in-process buffered
// Go channel. Every event is written to a write-ahead log (WAL) on disk FIRST,
// then sent to the channel. The WAL is the source of truth; the channel is a
// low-latency wake-up mechanism.
//
// Durability guarantee: once Publish() returns without error, the event is
// synced to the WAL. The channel is only a wake-up hint; the analytics worker
// drains bounded WAL batches so channel overflow cannot discard work. Processed
// event IDs are removed only after their rollup transaction commits. If a
// crash occurs before acknowledgment, the transactional analytics receipt
// suppresses duplicate rollup effects when the WAL record is replayed.
type channelBus struct {
	ch             chan ChangeEvent
	mu             sync.RWMutex
	closed         bool
	capacity       int
	maxEventBytes  int
	maxQueuedBytes int
	queueAdmission *QueueAdmission

	dropped                  atomic.Int64 // count of events that failed WAL write (disk full, etc.)
	payloadRejected          atomic.Int64
	walWriteFailed           atomic.Int64
	queueMu                  sync.Mutex
	oldestQueuedAt           time.Time
	queueReservedBytes       uint64
	queueAdmissionConfigured bool
	walAgeMu                 sync.RWMutex
	walPending               bool
	walAgeKnown              bool
	walOldestAt              time.Time
	walBytes                 int64
	walBytesKnown            bool

	walDir         string
	walFile        *os.File
	walErr         error // sticky write/sync failure; restart is required before further publication
	walSyncPending bool
	walMu          sync.Mutex
	drainMu        sync.Mutex
	replayedWAL    []string
}

const (
	DefaultMaxEventBytes     = 1 << 20
	DefaultMaxQueuedBytes    = 64 << 20
	MaxConfiguredQueuedBytes = 1 << 30
	MaxChannelCapacity       = 10_000
	maxWALRecordScanBytes    = 16<<20 + 4096
)

var ErrEventTooLarge = errors.New("analytics: event exceeds configured byte limit")

type boundedEventBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedEventBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, ErrEventTooLarge
	}
	return b.Buffer.Write(p)
}

func encodeBoundedEvent(event ChangeEvent, limit int) ([]byte, error) {
	// Keep the durable and queued representation at or below the configured
	// limit. This encoding may allocate temporary memory proportional to the
	// caller-owned event while checking its encoded size.
	buffer := &boundedEventBuffer{limit: limit + 1} // include Encoder's trailing newline
	if err := json.NewEncoder(buffer).Encode(event); err != nil {
		if errors.Is(err, ErrEventTooLarge) {
			return nil, fmt.Errorf("%w (limit %d bytes)", ErrEventTooLarge, limit)
		}
		return nil, fmt.Errorf("analytics: encode event: %w", err)
	}
	data := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if len(data) > limit {
		return nil, fmt.Errorf("%w (limit %d bytes)", ErrEventTooLarge, limit)
	}
	return append([]byte(nil), data...), nil
}

// NewChannelBus creates an in-process event bus with WAL-first durability.
// capacity is the channel buffer size (default 1000 if <= 0).
// walDir is where the write-ahead log is stored.
func NewChannelBus(capacity int, walDir string) EventBus {
	return NewChannelBusWithMaxEventBytes(capacity, DefaultMaxEventBytes, walDir)
}

// NewChannelBusWithMaxEventBytes creates an analytics bus that rejects an
// event before WAL publication when its encoded payload exceeds maxEventBytes.
func NewChannelBusWithMaxEventBytes(capacity, maxEventBytes int, walDir string) EventBus {
	return NewChannelBusWithLimits(capacity, maxEventBytes, DefaultMaxQueuedBytes, walDir)
}

// NewChannelBusWithLimits bounds the wake-up channel by both event count and
// the maximum encoded payload bytes it may retain. The WAL remains the durable
// queue, so reducing the in-memory hint capacity cannot lose published work.
func NewChannelBusWithLimits(capacity, maxEventBytes, maxQueuedBytes int, walDir string) EventBus {
	if capacity <= 0 {
		capacity = 1000
	} else if capacity > MaxChannelCapacity {
		capacity = MaxChannelCapacity
	}
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxEventBytes
	}
	if maxQueuedBytes <= 0 {
		maxQueuedBytes = DefaultMaxQueuedBytes
	}
	if maxEventBytes > maxQueuedBytes {
		maxEventBytes = maxQueuedBytes
	}
	byteCapacity := maxQueuedBytes / maxEventBytes
	if byteCapacity < 1 {
		byteCapacity = 1
	}
	if capacity > byteCapacity {
		capacity = byteCapacity
	}
	if walDir == "" {
		walDir = "data/analytics/wal"
	}

	b := &channelBus{
		ch:             make(chan ChangeEvent, capacity),
		capacity:       capacity,
		maxEventBytes:  maxEventBytes,
		maxQueuedBytes: maxQueuedBytes,
		walDir:         walDir,
		walAgeKnown:    true,
		walBytesKnown:  true,
	}
	b.walMu.Lock()
	if err := b.refreshWALBacklogLocked(); err != nil {
		slog.Warn("analytics: could not determine existing WAL backlog age", "error", err)
	}
	b.walMu.Unlock()

	// Check for leftover WAL / .flushing files from a previous run.
	// These will be drained by the worker on startup.
	if walPath := b.currentWALPath(); walPath != "" {
		if info, err := os.Stat(walPath); err == nil && info.Size() > 0 {
			slog.Info("analytics: existing WAL found, will drain on worker start",
				"path", walPath, "size", info.Size())
		}
	}
	flushingFiles, flushErr := listFlushingWALs(walDir)
	if flushErr != nil {
		slog.Error("analytics: could not inspect leftover WAL files", "error", flushErr)
	}
	if len(flushingFiles) > 0 {
		slog.Info("analytics: leftover flushing files found, will drain on worker start",
			"count", len(flushingFiles))
	}

	return b
}

// QueueBudgetSnapshot reports a conservative serialized-payload bound for the
// in-memory wake-up channel. Actual ChangeEvent object overhead is additional.
type QueueBudgetSnapshot struct {
	QueuedItems     int
	MaxQueuedItems  int
	UpperBoundBytes int
	MaxQueuedBytes  int
	MaxEventBytes   int
	// OldestAge is a conservative upper bound because EventBus subscribers read
	// directly from the public channel and cannot report dequeue timestamps.
	OldestAge time.Duration
}

// WALBacklogSnapshot describes durable records waiting for analytics commit.
// OldestAge is measured from the local persistence timestamp stored with each
// new WAL record; a file whose oldest record lacks a timestamp makes AgeKnown
// false.
type WALBacklogSnapshot struct {
	Pending    bool
	AgeKnown   bool
	OldestAge  time.Duration
	Bytes      int64
	BytesKnown bool
}

// DropCauseSnapshot keeps durable publication failures distinct from optional
// in-memory wake-up queue behavior. Counts are cumulative for this bus instance.
type DropCauseSnapshot struct {
	PayloadRejected                 int64
	WALWriteFailures                int64
	SubscriberOverflows             int64
	SubscriberRegistrationsRejected int64
}

func (b *channelBus) DropCauses() DropCauseSnapshot {
	if b == nil {
		return DropCauseSnapshot{}
	}
	return DropCauseSnapshot{PayloadRejected: b.payloadRejected.Load(), WALWriteFailures: b.walWriteFailed.Load()}
}

func (b *channelBus) QueueBudget() QueueBudgetSnapshot {
	if b == nil {
		return QueueBudgetSnapshot{}
	}
	age := time.Duration(0)
	if len(b.ch) > 0 {
		b.queueMu.Lock()
		if !b.oldestQueuedAt.IsZero() {
			age = time.Since(b.oldestQueuedAt)
		}
		b.queueMu.Unlock()
		if age < 0 {
			age = 0
		}
	}
	return QueueBudgetSnapshot{
		QueuedItems: len(b.ch), MaxQueuedItems: cap(b.ch),
		UpperBoundBytes: len(b.ch) * b.maxEventBytes,
		MaxQueuedBytes:  b.maxQueuedBytes,
		MaxEventBytes:   b.maxEventBytes,
		OldestAge:       age,
	}
}

func (b *channelBus) WALBacklog() WALBacklogSnapshot {
	if b == nil {
		return WALBacklogSnapshot{AgeKnown: true, BytesKnown: true}
	}
	b.walAgeMu.RLock()
	defer b.walAgeMu.RUnlock()
	snapshot := WALBacklogSnapshot{Pending: b.walPending, AgeKnown: b.walAgeKnown, Bytes: b.walBytes, BytesKnown: b.walBytesKnown}
	if b.walPending && b.walAgeKnown && !b.walOldestAt.IsZero() {
		snapshot.OldestAge = time.Since(b.walOldestAt)
		if snapshot.OldestAge < 0 {
			snapshot.OldestAge = 0
		}
	}
	return snapshot
}

func (b *channelBus) Publish(event ChangeEvent) error {
	_, err := b.publishWithSourceQueueCallbacks(event, nil, nil)
	return err
}

// publishWithSourceQueueCallbacks lets MultiBus preserve subscriber delivery
// when the bounded source wake-up queue is full. onQueue is called immediately
// before attempting the nonblocking send; onDrop clears any corresponding
// source-event deduplication marker when that send cannot fit.
func (b *channelBus) publishWithSourceQueueCallbacks(event ChangeEvent, onQueue, onDrop func()) (bool, error) {
	if event.ID == "" {
		event.ID = contract.NewEventID()
	}
	data, err := encodeBoundedEvent(event, b.maxEventBytes)
	if err != nil {
		b.dropped.Add(1)
		b.payloadRejected.Add(1)
		return false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return false, fmt.Errorf("analytics: event bus closed")
	}

	// WAL-first: write to the persistent log BEFORE attempting the channel send.
	// On restart, the worker drains the WAL and replays any events that were
	// written but not yet flushed to the database. The channel is just a
	// low-latency wake-up mechanism — the WAL is the source of truth.
	persistedAt := time.Now().UTC()
	if err := b.writeWAL(data, persistedAt); err != nil {
		b.dropped.Add(1)
		b.walWriteFailed.Add(1)
		slog.Error("analytics: event lost — WAL write failed",
			"doctype", event.Doctype, "doc", event.DocName, "op", event.Operation, "error", err)
		return false, fmt.Errorf("analytics: persist event to WAL: %w", err)
	}
	reservation := uint64(b.maxEventBytes)
	if b.queueAdmission != nil && !b.queueAdmission.TryAcquire(reservation) {
		// The durable event is safe in WAL. Skipping this bounded in-memory
		// wake-up only delays processing until the worker's next WAL scan.
		return false, nil
	}
	if b.queueAdmission != nil {
		b.queueMu.Lock()
		b.queueReservedBytes += reservation
		b.queueMu.Unlock()
	}

	// Best-effort channel send. A full channel is safe because the worker also
	// scans durable WAL batches on its flush ticker.
	if onQueue != nil {
		onQueue()
	}
	select {
	case b.ch <- event:
		b.queueMu.Lock()
		if b.oldestQueuedAt.IsZero() || len(b.ch) == 1 {
			b.oldestQueuedAt = time.Now()
		}
		b.queueMu.Unlock()
		return true, nil
	default:
		b.releaseQueuedEvent()
		if onDrop != nil {
			onDrop()
		}
		// Channel full — no warning needed, WAL guarantees durability.
		return false, nil
	}
}

// DrainWAL replays all events from the WAL file and any leftover .flushing files
// (from a crash during a previous flush rotation). Returns the total count of
// replayed events.
func (b *channelBus) DrainWAL(handler func(ChangeEvent)) (int, error) {
	return b.DrainWALBatch(0, handler)
}

// DrainWALBatch rotates the current file to create a stable read set, then
// processes at most limit valid events. Processed records remain durable until
// the consumer acknowledges their IDs after its database commit.
func (b *channelBus) DrainWALBatch(limit int, handler func(ChangeEvent)) (int, error) {
	if b.walDir == "" {
		return 0, nil
	}
	b.drainMu.Lock()
	defer b.drainMu.Unlock()
	if _, err := b.RotateWAL(); err != nil {
		return 0, fmt.Errorf("rotate WAL before drain: %w", err)
	}

	total := 0

	// Read leftover .flushing files first. They remain until the caller
	// acknowledges committed event IDs; after a crash between DB commit and WAL
	// acknowledgment they may replay, so the SQL receipt gate suppresses
	// duplicate local rollup effects.
	flushingFiles, err := listFlushingWALs(b.walDir)
	if err != nil {
		return 0, fmt.Errorf("list flushing WAL files: %w", err)
	}
	var drainErr error
	for _, fp := range flushingFiles {
		remaining := 0
		if limit > 0 {
			remaining = limit - total
			if remaining <= 0 {
				break
			}
		}
		n, err := b.drainFileLimit(fp, remaining, handler)
		if err != nil {
			slog.Warn("analytics: error draining flushing file", "path", fp, "error", err)
			drainErr = errors.Join(drainErr, fmt.Errorf("drain flushing WAL %s: %w", fp, err))
		}
		total += n
		if n > 0 || err != nil {
			b.trackReplayedWAL(fp)
		}
	}

	if total > 0 {
		slog.Info("analytics: drained WAL backlog", "events", total)
	}

	return total, drainErr
}

func (b *channelBus) trackReplayedWAL(path string) {
	b.walMu.Lock()
	defer b.walMu.Unlock()
	for _, existing := range b.replayedWAL {
		if existing == path {
			return
		}
	}
	b.replayedWAL = append(b.replayedWAL, path)
}

// AcknowledgeWAL removes only committed event IDs from the current and
// rotated logs. The rewrite is atomic per file, so events not yet consumed by
// the worker remain durable across shutdown and retry.
func (b *channelBus) AcknowledgeWAL(eventIDs []string) error {
	if len(eventIDs) == 0 || b.walDir == "" {
		return nil
	}
	ack := make(map[string]struct{}, len(eventIDs))
	for _, id := range eventIDs {
		if id != "" {
			ack[id] = struct{}{}
		}
	}
	b.walMu.Lock()
	defer b.walMu.Unlock()
	if b.walSyncPending {
		if err := b.syncWALDir(); err != nil {
			return fmt.Errorf("sync pending WAL directory changes: %w", err)
		}
	}
	if b.walFile != nil {
		closeErr := b.walFile.Close()
		b.walFile = nil
		if closeErr != nil {
			return fmt.Errorf("close WAL before acknowledgment: %w", closeErr)
		}
	}
	paths := []string{b.currentWALPath()}
	flushing, err := listFlushingWALs(b.walDir)
	if err != nil {
		return fmt.Errorf("list flushing WAL files for acknowledgment: %w", err)
	}
	paths = append(paths, flushing...)
	seen := map[string]bool{}
	changed := false
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		content, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read WAL %s for acknowledgment: %w", path, err)
		}
		var remaining strings.Builder
		for _, line := range strings.SplitAfter(string(content), "\n") {
			if line == "" {
				continue
			}
			var event ChangeEvent
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) == nil {
				if _, ok := ack[event.ID]; ok {
					continue
				}
			}
			remaining.WriteString(line)
		}
		if remaining.Len() == 0 {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove acknowledged WAL %s: %w", path, err)
			}
			b.walSyncPending = true
			changed = true
			continue
		}
		if err := replaceFileAtomically(b.walDir, path, remaining.String()); err != nil {
			return fmt.Errorf("rewrite acknowledged WAL %s: %w", path, err)
		}
		b.walSyncPending = true
		changed = true
	}
	if changed {
		b.walSyncPending = true
		if err := b.syncWALDir(); err != nil {
			return fmt.Errorf("sync WAL directory after acknowledgment: %w", err)
		}
	}
	b.walFile, err = os.OpenFile(b.currentWALPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("reopen WAL after acknowledgment: %w", err)
	}
	b.walSyncPending = true
	if err := b.syncWALDir(); err != nil {
		return fmt.Errorf("sync WAL directory after reopen: %w", err)
	}
	b.replayedWAL = nil
	if err := b.refreshWALBacklogLocked(); err != nil {
		slog.Warn("analytics: could not refresh WAL backlog age after acknowledgment", "error", err)
	}
	return nil
}

// replaceFileAtomically writes content to a temporary file in dir, fsyncs it, and renames it over
// path, so a crash leaves either the old or the new file and never a partial one. Every step's
// error is returned: a failed write or fsync must not be followed by the rename.
func replaceFileAtomically(dir, path, content string) (err error) {
	tmp, err := os.CreateTemp(dir, ".wal-ack-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temporary file: %w", err)
	}
	return nil
}

func (b *channelBus) drainFileLimit(path string, limit int, handler func(ChangeEvent)) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only WAL file

	count := 0
	var recordErr error
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if limit > 0 && count >= limit {
			break
		}
		var event ChangeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("analytics: skipping corrupt WAL line", "path", path, "error", err)
			recordErr = errors.Join(recordErr, fmt.Errorf("corrupt WAL record in %s: %w", path, err))
			continue
		}
		handler(event)
		count++
	}

	if err := scanner.Err(); err != nil {
		recordErr = errors.Join(recordErr, fmt.Errorf("reading %s: %w", path, err))
	}

	return count, recordErr
}

func listFlushingWALs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && (strings.HasSuffix(entry.Name(), ".flushing") || strings.HasPrefix(entry.Name(), ".wal-ack-")) {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths, nil
}

// WALHasPending reports whether the durable current or rotated WAL contains
// bytes awaiting worker replay. A stat/read failure is returned so callers do
// not silently treat an unreadable WAL as empty.
func WALHasPending(dir string) (bool, error) {
	if strings.TrimSpace(dir) == "" {
		return false, nil
	}
	current := filepath.Join(dir, "events.jsonl")
	info, err := os.Stat(current)
	if err == nil && info.Size() > 0 {
		return true, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat analytics WAL: %w", err)
	}
	paths, err := listFlushingWALs(dir)
	if err != nil {
		return false, fmt.Errorf("list analytics WAL rotations: %w", err)
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("stat analytics WAL rotation: %w", err)
		}
		if info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// InspectWALBacklog scans the first durable record in each WAL file. Records
// are appended in persistence-time order within a file, so the first record
// provides the oldest timestamp without decoding the full backlog. Legacy or
// corrupt first records remain pending and make the age unknown.
func InspectWALBacklog(dir string) (WALBacklogSnapshot, error) {
	if strings.TrimSpace(dir) == "" {
		return WALBacklogSnapshot{AgeKnown: true, BytesKnown: true}, nil
	}
	b := &channelBus{walDir: dir, walAgeKnown: true}
	b.walMu.Lock()
	err := b.refreshWALBacklogLocked()
	b.walMu.Unlock()
	return b.WALBacklog(), err
}

// refreshWALBacklogLocked expects walMu to be held, preventing concurrent
// append/rotation/acknowledgment while the disk snapshot is rebuilt.
func (b *channelBus) refreshWALBacklogLocked() error {
	paths := []string{b.currentWALPath()}
	flushing, err := listFlushingWALs(b.walDir)
	if err != nil {
		b.walAgeMu.Lock()
		b.walPending, b.walAgeKnown, b.walOldestAt = true, false, time.Time{}
		b.walBytesKnown = false
		b.walAgeMu.Unlock()
		return fmt.Errorf("list analytics WAL files: %w", err)
	}
	paths = append(paths, flushing...)
	pending, ageKnown, bytesKnown := false, true, true
	var totalBytes int64
	var oldest time.Time
	var scanErr error
	for _, path := range paths {
		if path == "" {
			continue
		}
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("stat analytics WAL %s: %w", path, statErr))
			pending, ageKnown = true, false
			bytesKnown = false
			continue
		}
		if info.Size() == 0 {
			continue
		}
		maxInt64 := int64(^uint64(0) >> 1)
		if info.Size() > maxInt64-totalBytes {
			totalBytes = maxInt64
		} else {
			totalBytes += info.Size()
		}
		pending = true
		file, openErr := os.Open(path)
		if openErr != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("open analytics WAL %s: %w", path, openErr))
			ageKnown = false
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 4096), maxWALRecordScanBytes)
		if !scanner.Scan() {
			if scanner.Err() != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("read analytics WAL %s: %w", path, scanner.Err()))
			}
			ageKnown = false
			_ = file.Close()
			continue
		}
		var record struct {
			PersistedAt time.Time `json:"_kora_wal_written_at"`
		}
		if decodeErr := json.Unmarshal(scanner.Bytes(), &record); decodeErr != nil || record.PersistedAt.IsZero() {
			ageKnown = false
			if decodeErr != nil {
				scanErr = errors.Join(scanErr, fmt.Errorf("decode analytics WAL timestamp %s: %w", path, decodeErr))
			}
		} else if oldest.IsZero() || record.PersistedAt.Before(oldest) {
			oldest = record.PersistedAt.UTC()
		}
		if closeErr := file.Close(); closeErr != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("close analytics WAL %s: %w", path, closeErr))
			ageKnown = false
		}
	}
	if pending && oldest.IsZero() {
		ageKnown = false
	}
	b.walAgeMu.Lock()
	b.walPending, b.walAgeKnown, b.walOldestAt = pending, ageKnown, oldest
	b.walBytes, b.walBytesKnown = totalBytes, bytesKnown
	b.walAgeMu.Unlock()
	return scanErr
}

// RotateWAL atomically swaps the current WAL file for a fresh one by renaming
// it with a .flushing suffix. New events are written to a new WAL file.
// Returns the path of the old file; the caller must call CommitWALRotation
// after the corresponding data has been successfully flushed to the database.
func (b *channelBus) RotateWAL() (string, error) {
	b.walMu.Lock()
	defer b.walMu.Unlock()
	if b.walSyncPending {
		if err := b.syncWALDir(); err != nil {
			return "", fmt.Errorf("sync pending WAL directory changes: %w", err)
		}
	}

	walPath := b.currentWALPath()
	if walPath == "" {
		return "", nil
	}

	// Close current handle so we can rename.
	if b.walFile != nil {
		if err := b.walFile.Sync(); err != nil {
			return "", fmt.Errorf("sync wal before rotation: %w", err)
		}
		if err := b.walFile.Close(); err != nil {
			b.walFile = nil
			return "", fmt.Errorf("close wal before rotation: %w", err)
		}
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
	flushingPath := filepath.Join(b.walDir, "events."+contract.NewEventID()+".flushing")
	if err := os.Rename(walPath, flushingPath); err != nil {
		return "", fmt.Errorf("rename wal for rotation: %w", err)
	}
	b.walSyncPending = true
	if err := b.syncWALDir(); err != nil {
		return "", fmt.Errorf("sync wal directory after rotation: %w", err)
	}

	// b.walFile is nil — next writeWAL will create a fresh events.jsonl.

	slog.Debug("analytics: rotated WAL", "flushing", flushingPath)
	return flushingPath, nil
}

// CommitWALRotation deletes a rotated WAL file after its events have been
// successfully flushed to the database, confirming durability.
func (b *channelBus) CommitWALRotation(oldWALPath string) error {
	b.walMu.Lock()
	defer b.walMu.Unlock()
	paths := append([]string(nil), b.replayedWAL...)
	if oldWALPath != "" {
		paths = append(paths, oldWALPath)
	}
	if len(paths) > 0 {
		b.walSyncPending = true
	}
	var remaining []string
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			remaining = append(remaining, path)
			continue
		}
	}
	b.replayedWAL = remaining
	if b.walSyncPending {
		if err := b.syncWALDir(); err != nil {
			return fmt.Errorf("sync wal directory after commit: %w", err)
		}
	}
	if err := b.refreshWALBacklogLocked(); err != nil {
		slog.Warn("analytics: could not refresh WAL backlog age after commit", "error", err)
	}
	if len(remaining) > 0 {
		return fmt.Errorf("commit wal rotation: could not remove %d WAL file(s)", len(remaining))
	}
	return nil
}

func (b *channelBus) Subscribe() (<-chan ChangeEvent, error) {
	return b.ch, nil
}

// SetQueueAdmission applies the Engine's process-wide analytics queued-payload
// budget. It must be configured before the bus has accepted any publication.
func (b *channelBus) SetQueueAdmission(admission *QueueAdmission) error {
	if b == nil || admission == nil {
		return fmt.Errorf("analytics bus and process queue admission are required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("analytics bus is closed")
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	if b.queueAdmissionConfigured || b.queueReservedBytes != 0 || len(b.ch) != 0 {
		return fmt.Errorf("analytics process queue admission must be set before publication")
	}
	b.queueAdmission = admission
	b.queueAdmissionConfigured = true
	return nil
}

// releaseQueuedEvent is called by MultiBus when it removes an event from the
// source wake-up channel. Each accepted source item reserves one max payload.
func (b *channelBus) releaseQueuedEvent() {
	if b == nil {
		return
	}
	b.queueMu.Lock()
	if b.queueReservedBytes < uint64(b.maxEventBytes) {
		b.queueMu.Unlock()
		return
	}
	reservation := uint64(b.maxEventBytes)
	b.queueReservedBytes -= reservation
	admission := b.queueAdmission
	b.queueMu.Unlock()
	if admission != nil {
		admission.Release(reservation)
	}
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
		syncErr := b.walFile.Sync()
		closeErr := b.walFile.Close()
		b.walFile = nil
		if err := errors.Join(syncErr, closeErr); err != nil {
			return fmt.Errorf("close analytics WAL: %w", err)
		}
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

func (b *channelBus) writeWAL(data []byte, persistedAt time.Time) error {
	b.walMu.Lock()
	defer b.walMu.Unlock()
	if b.walSyncPending {
		if err := b.syncWALDir(); err != nil {
			return fmt.Errorf("sync pending WAL directory changes: %w", err)
		}
	}
	if b.walErr != nil {
		return fmt.Errorf("WAL is unavailable after an earlier write failure: %w", b.walErr)
	}

	if b.walFile == nil {
		if err := os.MkdirAll(b.walDir, 0755); err != nil {
			return fmt.Errorf("create wal dir: %w", err)
		}
		f, err := os.OpenFile(b.currentWALPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("open wal: %w", err)
		}
		b.walFile = f
		// The first creation of the WAL file must be durable before Publish
		// reports success. Syncing the directory persists the new filename.
		b.walSyncPending = true
		if err := b.syncWALDir(); err != nil {
			_ = b.walFile.Close()
			b.walFile = nil
			return fmt.Errorf("sync wal directory after create: %w", err)
		}
	}

	encodedAt, err := json.Marshal(persistedAt.UTC())
	if err != nil {
		return fmt.Errorf("encode WAL persistence timestamp: %w", err)
	}
	// Add the persistence timestamp as a top-level WAL-only field. Existing
	// ChangeEvent decoders ignore unknown fields, preserving old readers.
	line := make([]byte, 0, len(data)+len(encodedAt)+32)
	line = append(line, data[:len(data)-1]...)
	line = append(line, `,"_kora_wal_written_at":`...)
	line = append(line, encodedAt...)
	line = append(line, '}')
	line = append(line, '\n')

	n, err := b.walFile.Write(line)
	if err != nil {
		b.walErr = err
		b.markWALStateUnknown()
		return fmt.Errorf("write wal: %w", err)
	}
	if n != len(line) {
		b.walErr = io.ErrShortWrite
		b.markWALStateUnknown()
		return fmt.Errorf("write wal: %w", b.walErr)
	}
	if err := b.walFile.Sync(); err != nil {
		b.walErr = err
		b.markWALStateUnknown()
		return fmt.Errorf("sync wal: %w", err)
	}
	b.walAgeMu.Lock()
	if b.walBytesKnown {
		maxInt64 := int64(^uint64(0) >> 1)
		if int64(len(line)) > maxInt64-b.walBytes {
			b.walBytes = maxInt64
		} else {
			b.walBytes += int64(len(line))
		}
	}
	if !b.walPending {
		b.walPending = true
		b.walAgeKnown = true
		b.walOldestAt = persistedAt
	} else if b.walAgeKnown && (b.walOldestAt.IsZero() || persistedAt.Before(b.walOldestAt)) {
		b.walOldestAt = persistedAt
	}
	b.walAgeMu.Unlock()
	return nil
}

func (b *channelBus) markWALStateUnknown() {
	b.walAgeMu.Lock()
	b.walPending = true
	b.walAgeKnown = false
	b.walBytesKnown = false
	b.walAgeMu.Unlock()
}

func (b *channelBus) syncWALDir() error {
	if b.walDir == "" {
		return nil
	}
	dir, err := os.Open(b.walDir)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	b.walSyncPending = false
	return nil
}
