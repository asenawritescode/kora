package analytics

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/asenawritescode/kora/contract"
)

// MultiBus wraps an EventBus and is its sole subscriber, fanning each event
// out to independent listeners. Consumers should subscribe through MultiBus
// rather than competing to read from the inner channel.
type MultiBus struct {
	inner                           EventBus
	mu                              sync.RWMutex
	channels                        []chan ChangeEvent
	realtime                        map[chan ChangeEvent]struct{}
	oldestQueuedAt                  map[chan ChangeEvent]time.Time
	queuedReservations              map[chan ChangeEvent][]uint64
	pendingSourceEvents             map[string]int
	queueAdmission                  *QueueAdmission
	maxListeners                    int
	listenerMaxBytes                int
	listenersClose                  bool
	closed                          bool
	started                         bool
	dropped                         atomic.Int64
	subscriberOverflows             atomic.Int64
	subscriberRegistrationsRejected atomic.Int64
	source                          <-chan ChangeEvent
	done                            chan struct{}
	closeOnce                       sync.Once
	doneOnce                        sync.Once
	closeErr                        error
}

func (mb *MultiBus) Publish(event ChangeEvent) error {
	if mb == nil {
		return fmt.Errorf("multi bus is required")
	}
	mb.mu.RLock()
	closed := mb.closed
	mb.mu.RUnlock()
	if closed {
		return fmt.Errorf("multi bus is closed")
	}
	// All Engine publishers call through MultiBus. Fan out after durable
	// publication so source-queue saturation cannot hide a realtime invalidation
	// or another in-memory consumer event. The source channel remains a worker
	// wake-up hint; suppress its copy when the hint was queued successfully.
	if event.ID == "" {
		event.ID = contract.NewEventID()
	}
	if publisher, ok := mb.inner.(interface {
		publishWithSourceQueueCallbacks(ChangeEvent, func(), func()) (bool, error)
	}); ok {
		_, err := publisher.publishWithSourceQueueCallbacks(event, func() {
			mb.mu.Lock()
			if mb.pendingSourceEvents == nil {
				mb.pendingSourceEvents = make(map[string]int)
			}
			mb.pendingSourceEvents[event.ID]++
			mb.mu.Unlock()
		}, func() {
			mb.mu.Lock()
			mb.removePendingSourceEventLocked(event.ID)
			mb.mu.Unlock()
		})
		if err != nil {
			return err
		}
		mb.fanout(event)
		return nil
	}
	return mb.inner.Publish(event)
}

// QueueBudget exposes the wrapped durable bus's bounded in-memory queue
// estimate when that implementation supports it.
func (mb *MultiBus) QueueBudget() QueueBudgetSnapshot {
	if mb == nil {
		return QueueBudgetSnapshot{}
	}
	if snapshotter, ok := mb.inner.(interface{ QueueBudget() QueueBudgetSnapshot }); ok {
		return snapshotter.QueueBudget()
	}
	return QueueBudgetSnapshot{}
}

// Subscribe registers a buffered listener. Long-lived consumers should remove
// their listener explicitly when their context ends.
func (mb *MultiBus) Subscribe() (<-chan ChangeEvent, error) {
	ch := mb.NewListenerQueue(false)
	if err := mb.TryAddListener(ch); err != nil {
		return nil, err
	}
	return ch, nil
}

func (mb *MultiBus) DrainWAL(handler func(ChangeEvent)) (int, error) {
	return mb.inner.DrainWAL(handler)
}
func (mb *MultiBus) DrainWALBatch(limit int, handler func(ChangeEvent)) (int, error) {
	if drainer, ok := mb.inner.(WALBatchDrainer); ok {
		return drainer.DrainWALBatch(limit, handler)
	}
	return mb.inner.DrainWAL(handler)
}
func (mb *MultiBus) RotateWAL() (string, error)          { return mb.inner.RotateWAL() }
func (mb *MultiBus) CommitWALRotation(path string) error { return mb.inner.CommitWALRotation(path) }
func (mb *MultiBus) AcknowledgeWAL(ids []string) error {
	if ack, ok := mb.inner.(EventWALAcknowledger); ok {
		return ack.AcknowledgeWAL(ids)
	}
	return fmt.Errorf("event bus does not support per-event WAL acknowledgment")
}

// NewMultiBus creates and starts a fan-out wrapper around an existing EventBus.
func NewMultiBus(inner EventBus) (*MultiBus, error) {
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		return nil, err
	}
	if err := mb.Start(); err != nil {
		_ = mb.Close()
		return nil, err
	}
	return mb, nil
}

// NewDeferredMultiBus prepares a fan-out wrapper without starting a goroutine.
// The owner calls Start when its process or tenant generation becomes active.
func NewDeferredMultiBus(inner EventBus) (*MultiBus, error) {
	if inner == nil {
		return nil, fmt.Errorf("multi bus inner event bus is required")
	}
	mb := &MultiBus{inner: inner, done: make(chan struct{}), maxListeners: DefaultMaxListeners, listenerMaxBytes: DefaultListenerQueueBytes, oldestQueuedAt: make(map[chan ChangeEvent]time.Time), queuedReservations: make(map[chan ChangeEvent][]uint64), pendingSourceEvents: make(map[string]int)}
	ch, err := inner.Subscribe()
	if err != nil {
		return nil, err
	}
	mb.source = ch
	return mb, nil
}

const DefaultMaxListeners = 128
const DefaultListenerQueueSize = 16
const DefaultRealtimeListenerQueueSize = 8
const DefaultListenerQueueBytes = 16 << 20
const DefaultRealtimeListenerQueueBytes = 16 << 20

// MaxListenerQueueSize bounds the number of events a listener may retain.
// Event payloads are already bounded by the underlying EventBus configuration.
const MaxListenerQueueSize = 64

type ListenerQueueSnapshot struct {
	Listeners       int
	MaxListeners    int
	QueuedItems     int
	UpperBoundBytes int
	MaxQueuedBytes  int
	// OldestAge is a conservative upper bound while this queue is nonempty.
	OldestAge time.Duration
}

// ListenerQueueCapacity derives a conservative event count from the largest
// event accepted by the wrapped durable bus and this listener's byte budget.
func (mb *MultiBus) ListenerQueueCapacity(realtime bool) int {
	if mb == nil {
		return 0
	}
	maxBytes := mb.listenerMaxBytes
	if realtime {
		maxBytes = DefaultRealtimeListenerQueueBytes
	}
	maxEventBytes := DefaultMaxEventBytes
	if snapshot := mb.QueueBudget(); snapshot.MaxEventBytes > 0 {
		maxEventBytes = snapshot.MaxEventBytes
	}
	capacity := maxBytes / maxEventBytes
	if capacity > MaxListenerQueueSize {
		capacity = MaxListenerQueueSize
	}
	return capacity
}

func (mb *MultiBus) NewListenerQueue(realtime bool) chan ChangeEvent {
	capacity := mb.ListenerQueueCapacity(realtime)
	return make(chan ChangeEvent, capacity)
}

func (mb *MultiBus) ListenerQueueBudget() ListenerQueueSnapshot {
	if mb == nil {
		return ListenerQueueSnapshot{}
	}
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	maxEventBytes := DefaultMaxEventBytes
	if snapshot := mb.QueueBudget(); snapshot.MaxEventBytes > 0 {
		maxEventBytes = snapshot.MaxEventBytes
	}
	queued, maxQueued := 0, 0
	oldest := time.Duration(0)
	for _, listener := range mb.channels {
		queued += len(listener)
		maxQueued += cap(listener)
		if len(listener) > 0 {
			if at := mb.oldestQueuedAt[listener]; !at.IsZero() {
				age := time.Since(at)
				if age < 0 {
					age = 0
				}
				if age > oldest {
					oldest = age
				}
			}
		}
	}
	return ListenerQueueSnapshot{Listeners: len(mb.channels), MaxListeners: mb.maxListeners, QueuedItems: queued, UpperBoundBytes: queued * maxEventBytes, MaxQueuedBytes: maxQueued * maxEventBytes, OldestAge: oldest}
}

// SetQueueAdmission attaches the process-shared subscriber byte budget before
// fanout starts or listeners are registered.
func (mb *MultiBus) SetQueueAdmission(admission *QueueAdmission) error {
	if mb == nil || admission == nil {
		return fmt.Errorf("multi bus and listener queue admission are required")
	}
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.started || len(mb.channels) != 0 || mb.closed {
		return fmt.Errorf("listener queue admission must be set before fanout or listeners start")
	}
	mb.queueAdmission = admission
	return nil
}

func (mb *MultiBus) DropCauses() DropCauseSnapshot {
	if mb == nil {
		return DropCauseSnapshot{}
	}
	var snapshot DropCauseSnapshot
	if source, ok := mb.inner.(interface{ DropCauses() DropCauseSnapshot }); ok {
		snapshot = source.DropCauses()
	}
	snapshot.SubscriberOverflows += mb.subscriberOverflows.Load()
	snapshot.SubscriberRegistrationsRejected += mb.subscriberRegistrationsRejected.Load()
	return snapshot
}

var ErrListenerLimit = fmt.Errorf("event bus listener limit reached")

// SetMaxListeners configures the subscriber count bound before listeners are
// registered. It is primarily used by tenant assembly and tests.
func (mb *MultiBus) SetMaxListeners(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("listener limit must be positive")
	}
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.started || len(mb.channels) > limit {
		return fmt.Errorf("listener limit must be set before fanout starts and cannot be below current listeners")
	}
	mb.maxListeners = limit
	return nil
}

func (mb *MultiBus) maxEventBytes() uint64 {
	if snapshot := mb.QueueBudget(); snapshot.MaxEventBytes > 0 {
		return uint64(snapshot.MaxEventBytes)
	}
	return DefaultMaxEventBytes
}

func (mb *MultiBus) reapConsumedLocked() {
	for _, listener := range mb.channels {
		reservations := mb.queuedReservations[listener]
		consumed := len(reservations) - len(listener)
		if consumed < 0 {
			// Listeners are expected to receive only from this channel. If a
			// caller violates that contract by sending into it, retain the
			// conservative reservations rather than releasing unaccounted bytes.
			consumed = 0
		}
		if consumed > 0 {
			var released uint64
			for _, bytes := range reservations[:consumed] {
				released += bytes
			}
			mb.queueAdmission.Release(released)
			remaining := append(reservations[:0], reservations[consumed:]...)
			if len(remaining) == 0 {
				delete(mb.queuedReservations, listener)
			} else {
				mb.queuedReservations[listener] = remaining
			}
		}
		if len(listener) == 0 {
			delete(mb.oldestQueuedAt, listener)
		}
	}
}

func (mb *MultiBus) releaseListenerReservationsLocked(listener chan ChangeEvent) {
	reservations := mb.queuedReservations[listener]
	var released uint64
	for _, bytes := range reservations {
		released += bytes
	}
	mb.queueAdmission.Release(released)
	delete(mb.queuedReservations, listener)
}

func (mb *MultiBus) releaseListenerSlotLocked() {
	if mb.queueAdmission != nil {
		mb.queueAdmission.ReleaseListener()
	}
}

// Start launches the single owned fan-out reader exactly once.
func (mb *MultiBus) Start() error {
	if mb == nil {
		return fmt.Errorf("multi bus is required")
	}
	mb.mu.Lock()
	if mb.closed {
		mb.mu.Unlock()
		return fmt.Errorf("multi bus is closed")
	}
	if mb.started {
		mb.mu.Unlock()
		return nil
	}
	mb.started = true
	ch := mb.source
	mb.mu.Unlock()
	go func() {
		defer mb.doneOnce.Do(func() { close(mb.done) })
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case event, ok := <-ch:
				if !ok {
					return
				}
				if source, ok := mb.inner.(interface{ releaseQueuedEvent() }); ok {
					source.releaseQueuedEvent()
				}
				mb.mu.Lock()
				alreadyFannedOut := mb.pendingSourceEvents[event.ID] > 0
				if alreadyFannedOut {
					mb.removePendingSourceEventLocked(event.ID)
				}
				mb.mu.Unlock()
				if !alreadyFannedOut {
					mb.fanout(event)
				}
			case <-ticker.C:
				mb.mu.Lock()
				mb.reapConsumedLocked()
				mb.mu.Unlock()
			}
		}
	}()
	return nil
}

func (mb *MultiBus) removePendingSourceEventLocked(id string) {
	pending := mb.pendingSourceEvents[id]
	if pending <= 1 {
		delete(mb.pendingSourceEvents, id)
		return
	}
	mb.pendingSourceEvents[id] = pending - 1
}

func (mb *MultiBus) fanout(event ChangeEvent) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.closed {
		return
	}
	mb.reapConsumedLocked()
	reservation := mb.maxEventBytes()
	for index := 0; index < len(mb.channels); {
		listener := mb.channels[index]
		_, closeOnOverflow := mb.realtime[listener]
		if !mb.queueAdmission.TryAcquire(reservation) {
			mb.recordListenerOverflowLocked(listener, closeOnOverflow, &index)
			continue
		}
		select {
		case listener <- event:
			mb.queuedReservations[listener] = append(mb.queuedReservations[listener], reservation)
			if len(listener) == 1 {
				mb.oldestQueuedAt[listener] = time.Now()
			}
			index++
		default:
			mb.queueAdmission.Release(reservation)
			mb.recordListenerOverflowLocked(listener, closeOnOverflow, &index)
		}
	}
}

func (mb *MultiBus) recordListenerOverflowLocked(listener chan ChangeEvent, closeOnOverflow bool, index *int) {
	mb.dropped.Add(1)
	mb.subscriberOverflows.Add(1)
	if !closeOnOverflow {
		*index++
		return
	}
	drainRealtimeQueue(listener)
	mb.releaseListenerReservationsLocked(listener)
	mb.releaseListenerSlotLocked()
	close(listener)
	delete(mb.realtime, listener)
	delete(mb.oldestQueuedAt, listener)
	mb.channels = append(mb.channels[:*index], mb.channels[*index+1:]...)
}

func drainRealtimeQueue(ch <-chan ChangeEvent) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// AddListener registers a new subscriber channel. Events are fanned out to all listeners.
// The channel should be buffered to avoid blocking the fan-out goroutine; the
// caller must only receive from it and must not send to or close it.
func (mb *MultiBus) AddListener(ch chan ChangeEvent) {
	_ = mb.TryAddListener(ch)
}

// TryAddListener registers a listener or returns a capacity/closed error.
// On error the supplied channel is closed and is not owned by the bus.
func (mb *MultiBus) TryAddListener(ch chan ChangeEvent) error {
	return mb.addListener(ch, false)
}

// AddRealtimeListener registers a bounded local realtime queue. If it falls
// behind, MultiBus closes and removes it so the stream handler can reconnect
// and the client can refetch authorized state.
func (mb *MultiBus) AddRealtimeListener(ch chan ChangeEvent) error {
	return mb.addListener(ch, true)
}

func (mb *MultiBus) addListener(ch chan ChangeEvent, closeOnOverflow bool) error {
	if ch == nil {
		return fmt.Errorf("listener channel is required")
	}
	maxBytes := mb.listenerMaxBytes
	if closeOnOverflow {
		maxBytes = DefaultRealtimeListenerQueueBytes
	}
	maxEventBytes := DefaultMaxEventBytes
	if snapshotter, ok := mb.inner.(interface{ QueueBudget() QueueBudgetSnapshot }); ok {
		if snapshot := snapshotter.QueueBudget(); snapshot.MaxEventBytes > 0 {
			maxEventBytes = snapshot.MaxEventBytes
		}
	}
	maxCapacity := maxBytes / maxEventBytes
	if maxCapacity > MaxListenerQueueSize {
		maxCapacity = MaxListenerQueueSize
	}
	if cap(ch) > maxCapacity {
		mb.dropped.Add(1)
		mb.subscriberRegistrationsRejected.Add(1)
		close(ch)
		return fmt.Errorf("listener channel capacity %d exceeds byte-derived limit %d", cap(ch), maxCapacity)
	}
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.closed || mb.listenersClose || mb.maxListeners <= 0 {
		mb.dropped.Add(1)
		mb.subscriberRegistrationsRejected.Add(1)
		close(ch)
		return ErrListenerLimit
	}
	for _, existing := range mb.channels {
		if existing == ch {
			if closeOnOverflow {
				if mb.realtime == nil {
					mb.realtime = make(map[chan ChangeEvent]struct{})
				}
				mb.realtime[ch] = struct{}{}
			}
			return nil
		}
	}
	if len(mb.channels) >= mb.maxListeners {
		mb.dropped.Add(1)
		mb.subscriberRegistrationsRejected.Add(1)
		close(ch)
		return ErrListenerLimit
	}
	if mb.queueAdmission != nil && !mb.queueAdmission.TryAcquireListener() {
		mb.dropped.Add(1)
		mb.subscriberRegistrationsRejected.Add(1)
		close(ch)
		return fmt.Errorf("%w: process listener capacity reached", ErrListenerLimit)
	}
	mb.channels = append(mb.channels, ch)
	if mb.oldestQueuedAt == nil {
		mb.oldestQueuedAt = make(map[chan ChangeEvent]time.Time)
	}
	if closeOnOverflow {
		if mb.realtime == nil {
			mb.realtime = make(map[chan ChangeEvent]struct{})
		}
		mb.realtime[ch] = struct{}{}
	}
	return nil
}

// RemoveListener removes a subscriber channel.
func (mb *MultiBus) RemoveListener(ch chan ChangeEvent) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	for i, c := range mb.channels {
		if c == ch {
			mb.reapConsumedLocked()
			drainRealtimeQueue(c)
			mb.releaseListenerReservationsLocked(c)
			mb.releaseListenerSlotLocked()
			mb.channels = append(mb.channels[:i], mb.channels[i+1:]...)
			delete(mb.realtime, ch)
			delete(mb.oldestQueuedAt, ch)
			return
		}
	}
}

func (mb *MultiBus) ListenerLimit() int {
	if mb == nil {
		return 0
	}
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	return mb.maxListeners
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
	mb.closeOnce.Do(func() {
		mb.mu.Lock()
		mb.closed = true
		started := mb.started
		mb.listenersClose = true
		mb.reapConsumedLocked()
		for _, ch := range mb.channels {
			drainRealtimeQueue(ch)
			mb.releaseListenerReservationsLocked(ch)
			mb.releaseListenerSlotLocked()
			close(ch)
		}
		mb.channels = nil
		mb.realtime = nil
		mb.oldestQueuedAt = make(map[chan ChangeEvent]time.Time)
		mb.mu.Unlock()
		mb.closeErr = mb.inner.Close()
		if !started {
			mb.releaseUnstartedSourceQueue()
			mb.mu.Lock()
			mb.pendingSourceEvents = nil
			mb.mu.Unlock()
			mb.doneOnce.Do(func() { close(mb.done) })
		} else {
			<-mb.done
		}
	})
	return mb.closeErr
}

func (mb *MultiBus) releaseUnstartedSourceQueue() {
	source, ok := mb.inner.(interface{ releaseQueuedEvent() })
	if !ok {
		return
	}
	for {
		select {
		case _, open := <-mb.source:
			if !open {
				return
			}
			source.releaseQueuedEvent()
		default:
			return
		}
	}
}

// Inner returns the wrapped EventBus.
func (mb *MultiBus) Inner() EventBus { return mb.inner }
