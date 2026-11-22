package eventbus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/eventstream"
)

// channelBus is the in-process source queue behind MultiBus. It holds no
// durable state: events live in memory until MultiBus fans them out. A
// subscriber that cannot keep up loses events, and the loss is counted.
type channelBus struct {
	ch             chan eventstream.ChangeEvent
	mu             sync.RWMutex
	closed         bool
	capacity       int
	maxEventBytes  int
	queueAdmission *QueueAdmission

	dropped                  atomic.Int64 // events refused before publication
	payloadRejected          atomic.Int64
	queueMu                  sync.Mutex
	oldestQueuedAt           time.Time
	queueReservedBytes       uint64
	queueAdmissionConfigured bool
}

// DefaultMaxEventBytes bounds one encoded change event unless configured otherwise.
const DefaultMaxEventBytes = 1 << 20

// MaxChannelCapacity caps the source queue's buffer size.
const MaxChannelCapacity = 10_000

const defaultChannelCapacity = 1000

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

func encodeBoundedEvent(event eventstream.ChangeEvent, limit int) ([]byte, error) {
	// Enforce the size limit while encoding, so an oversized event never
	// allocates its full encoding.
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

// NewChannelBus creates an in-process source queue. capacity is the channel
// buffer size (default 1000 if <= 0, capped at MaxChannelCapacity) and
// maxEventBytes bounds each encoded event (default DefaultMaxEventBytes).
func NewChannelBus(capacity, maxEventBytes int) eventstream.Bus {
	if capacity <= 0 {
		capacity = defaultChannelCapacity
	} else if capacity > MaxChannelCapacity {
		capacity = MaxChannelCapacity
	}
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxEventBytes
	}
	return &channelBus{
		ch:            make(chan eventstream.ChangeEvent, capacity),
		capacity:      capacity,
		maxEventBytes: maxEventBytes,
	}
}

// QueueBudgetSnapshot reports a conservative serialized-payload bound for the
// in-memory source queue. Actual eventstream.ChangeEvent object overhead is additional.
type QueueBudgetSnapshot struct {
	QueuedItems     int
	MaxQueuedItems  int
	UpperBoundBytes int
	MaxQueuedBytes  int
	MaxEventBytes   int
	// OldestAge is a conservative upper bound because eventstream.Bus subscribers read
	// directly from the public channel and cannot report dequeue timestamps.
	OldestAge time.Duration
}

// DropCauseSnapshot separates events refused before publication from
// subscriber overflow. Counts are cumulative for this bus instance.
type DropCauseSnapshot struct {
	PayloadRejected                 int64
	SubscriberOverflows             int64
	SubscriberRegistrationsRejected int64
}

func (b *channelBus) DropCauses() DropCauseSnapshot {
	if b == nil {
		return DropCauseSnapshot{}
	}
	return DropCauseSnapshot{PayloadRejected: b.payloadRejected.Load()}
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
		MaxQueuedBytes:  cap(b.ch) * b.maxEventBytes,
		MaxEventBytes:   b.maxEventBytes,
		OldestAge:       age,
	}
}

func (b *channelBus) Publish(event eventstream.ChangeEvent) error {
	_, err := b.publishWithSourceQueueCallbacks(event, nil, nil)
	return err
}

// publishWithSourceQueueCallbacks lets MultiBus preserve subscriber delivery
// when the bounded source queue is full. onQueue is called immediately
// before attempting the nonblocking send; onDrop clears any corresponding
// source-event deduplication marker when that send cannot fit.
//
// A full source queue is not a loss: MultiBus fans the event out from memory
// right after this call, so the source queue only carries events that MultiBus
// must dedupe against that fan-out.
func (b *channelBus) publishWithSourceQueueCallbacks(event eventstream.ChangeEvent, onQueue, onDrop func()) (bool, error) {
	if event.ID == "" {
		event.ID = contract.NewEventID()
	}
	if _, err := encodeBoundedEvent(event, b.maxEventBytes); err != nil {
		b.dropped.Add(1)
		b.payloadRejected.Add(1)
		return false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return false, fmt.Errorf("analytics: event bus closed")
	}
	reservation := uint64(b.maxEventBytes)
	if b.queueAdmission != nil && !b.queueAdmission.TryAcquire(reservation) {
		return false, nil
	}
	if b.queueAdmission != nil {
		b.queueMu.Lock()
		b.queueReservedBytes += reservation
		b.queueMu.Unlock()
	}
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
		return false, nil
	}
}

func (b *channelBus) Subscribe() (<-chan eventstream.ChangeEvent, error) {
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
// source queue. Each accepted source item reserves one max payload.
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
	return nil
}

// LogLegacyWALDir reports leftover files from the retired analytics
// write-ahead log. Nothing reads them any more; the operator may delete them.
func LogLegacyWALDir(dir string) {
	if !legacyWALHasFiles(dir) {
		return
	}
	slog.Info("old analytics WAL no longer used; safe to delete " + dir)
}
