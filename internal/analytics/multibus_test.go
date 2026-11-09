package analytics

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type multiBusStub struct {
	ch chan ChangeEvent
}

func (s *multiBusStub) Publish(event ChangeEvent) error { return nil }
func (s *multiBusStub) Subscribe() (<-chan ChangeEvent, error) {
	s.ch = make(chan ChangeEvent, 1)
	return s.ch, nil
}
func (s *multiBusStub) DrainWAL(handler func(ChangeEvent)) (int, error) { return 0, nil }
func (s *multiBusStub) RotateWAL() (string, error)                      { return "", nil }
func (s *multiBusStub) CommitWALRotation(string) error                  { return nil }
func (s *multiBusStub) Dropped() int64                                  { return 0 }
func (s *multiBusStub) Close() error {
	close(s.ch)
	return nil
}

func TestMultiBusDroppedCountsSlowListeners(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewMultiBus(inner)
	if err != nil {
		t.Fatalf("NewMultiBus: %v", err)
	}
	defer mb.Close()

	listener := make(chan ChangeEvent)
	mb.AddListener(listener)

	inner.ch <- ChangeEvent{Site: "test", Doctype: "Customer", Timestamp: time.Now().UTC()}

	time.Sleep(10 * time.Millisecond)

	if got := mb.Dropped(); got == 0 {
		t.Fatalf("Dropped() = %d, want > 0 when listener is slow", got)
	}
}

func TestMultiBusCloseJoinsFanout(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewMultiBus(inner)
	if err != nil {
		t.Fatalf("NewMultiBus: %v", err)
	}
	if err := mb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-mb.done:
	default:
		t.Fatal("Close returned before the fan-out goroutine exited")
	}
}

func TestMultiBusProcessListenerAdmissionBoundsAndReleasesAcrossTenants(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmissionWithLimits(4*DefaultMaxEventBytes, 2)
	if err != nil {
		t.Fatal("create process listener budget:", err)
	}
	newBus := func() *MultiBus {
		t.Helper()
		inner := NewChannelBusWithLimits(2, DefaultMaxEventBytes, 2*DefaultMaxEventBytes, t.TempDir())
		multi, err := NewDeferredMultiBus(inner)
		if err != nil {
			t.Fatal("create tenant multi bus:", err)
		}
		if err := multi.SetQueueAdmission(budget); err != nil {
			t.Fatal("set process queue admission:", err)
		}
		t.Cleanup(func() {
			if err := multi.Close(); err != nil {
				t.Errorf("close tenant multi bus: %v", err)
			}
		})
		return multi
	}

	firstTenant, secondTenant := newBus(), newBus()
	first := firstTenant.NewListenerQueue(false)
	second := secondTenant.NewListenerQueue(false)
	if err := firstTenant.TryAddListener(first); err != nil {
		t.Fatal("register first tenant listener:", err)
	}
	if err := secondTenant.TryAddListener(second); err != nil {
		t.Fatal("register second tenant listener:", err)
	}
	third := firstTenant.NewListenerQueue(false)
	if err := firstTenant.TryAddListener(third); err == nil {
		t.Fatal("third process listener unexpectedly passed the shared cap")
	}
	if _, open := <-third; open {
		t.Fatal("rejected listener channel was not closed")
	}
	if snapshot := budget.Snapshot(); snapshot.Listeners != 2 || snapshot.MaxListeners != 2 || snapshot.RejectedListeners != 1 {
		t.Fatalf("process listener budget at capacity = %+v", snapshot)
	}

	firstTenant.RemoveListener(first)
	if snapshot := budget.Snapshot(); snapshot.Listeners != 1 {
		t.Fatalf("removing a listener did not return its process slot: %+v", snapshot)
	}
	third = firstTenant.NewListenerQueue(false)
	if err := firstTenant.TryAddListener(third); err != nil {
		t.Fatalf("released process listener slot was not reusable: %v", err)
	}
	if err := secondTenant.Close(); err != nil {
		t.Fatal("close second tenant bus:", err)
	}
	if err := firstTenant.Close(); err != nil {
		t.Fatal("close first tenant bus:", err)
	}
	if snapshot := budget.Snapshot(); snapshot.Listeners != 0 {
		t.Fatalf("closing tenant buses leaked process listener slots: %+v", snapshot)
	}
}

func TestMultiBusProcessListenerAdmissionSerializesConcurrentRegistrations(t *testing.T) {
	const limit = 3
	budget, err := NewAnalyticsQueueAdmissionWithLimits(8*DefaultMaxEventBytes, limit)
	if err != nil {
		t.Fatal("create process listener budget:", err)
	}
	const tenantCount = 12
	buses := make([]*MultiBus, tenantCount)
	for i := range buses {
		inner := NewChannelBusWithLimits(2, DefaultMaxEventBytes, 2*DefaultMaxEventBytes, t.TempDir())
		buses[i], err = NewDeferredMultiBus(inner)
		if err != nil {
			t.Fatal("create tenant multi bus:", err)
		}
		if err := buses[i].SetQueueAdmission(budget); err != nil {
			t.Fatal("set process queue admission:", err)
		}
		t.Cleanup(func() {
			if closeErr := buses[i].Close(); closeErr != nil {
				t.Errorf("close tenant multi bus: %v", closeErr)
			}
		})
	}

	var wait sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for _, multi := range buses {
		wait.Add(1)
		go func(multi *MultiBus) {
			defer wait.Done()
			if err := multi.TryAddListener(multi.NewListenerQueue(false)); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(multi)
	}
	wait.Wait()
	if accepted != limit {
		t.Fatalf("concurrent process listener registrations accepted %d, want %d", accepted, limit)
	}
	if snapshot := budget.Snapshot(); snapshot.Listeners != limit || snapshot.RejectedListeners != tenantCount-limit {
		t.Fatalf("process listener budget after concurrent registrations = %+v", snapshot)
	}
	for _, multi := range buses {
		if err := multi.Close(); err != nil {
			t.Fatal("close tenant multi bus:", err)
		}
	}
	if snapshot := budget.Snapshot(); snapshot.Listeners != 0 {
		t.Fatalf("concurrent registrations leaked process listener slots: %+v", snapshot)
	}
}

func TestMultiBusSharesProcessWideListenerQueueByteBudget(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	newBus := func() (*MultiBus, *multiBusStub, chan ChangeEvent) {
		inner := &multiBusStub{}
		bus, err := NewDeferredMultiBus(inner)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.SetQueueAdmission(budget); err != nil {
			t.Fatal(err)
		}
		listener := bus.NewListenerQueue(false)
		if err := bus.TryAddListener(listener); err != nil {
			t.Fatal(err)
		}
		if err := bus.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus, inner, listener
	}
	_, firstInner, firstListener := newBus()
	_, secondInner, secondListener := newBus()
	event := ChangeEvent{Site: "tenant", Doctype: "Thing", Timestamp: time.Now().UTC()}
	firstInner.ch <- event
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == DefaultMaxEventBytes })
	secondInner.ch <- event
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.RejectedItems == 1 })
	if len(secondListener) != 0 {
		t.Fatal("second tenant received an event after the process-wide byte budget was exhausted")
	}
	<-firstListener
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == 0 })
	secondInner.ch <- event
	select {
	case <-secondListener:
	case <-time.After(time.Second):
		t.Fatal("second tenant did not receive an event after queued bytes were released")
	}
	if snapshot := budget.Snapshot(); snapshot.UsedBytes > DefaultMaxEventBytes || snapshot.MaxBytes != DefaultMaxEventBytes || snapshot.RejectedItems != 1 {
		t.Fatalf("process listener budget snapshot = %+v", snapshot)
	}
}

func TestMultiBusProcessBudgetClosesRealtimeListenerAndReleasesBytes(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	newBus := func(realtime bool) (*MultiBus, *multiBusStub, chan ChangeEvent) {
		inner := &multiBusStub{}
		bus, err := NewDeferredMultiBus(inner)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.SetQueueAdmission(budget); err != nil {
			t.Fatal(err)
		}
		listener := bus.NewListenerQueue(realtime)
		if realtime {
			err = bus.AddRealtimeListener(listener)
		} else {
			err = bus.TryAddListener(listener)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus, inner, listener
	}
	_, source, queued := newBus(false)
	second, realtimeSource, realtime := newBus(true)
	event := ChangeEvent{Site: "tenant", Doctype: "Thing", Timestamp: time.Now().UTC()}
	source.ch <- event
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == DefaultMaxEventBytes })
	realtimeSource.ch <- event
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool {
		return snapshot.RejectedItems == 1 && second.ListenerCount() == 0
	})
	if _, ok := <-realtime; ok {
		t.Fatal("realtime listener remained open after process queue budget rejection")
	}
	if got := len(queued); got != 1 {
		t.Fatalf("unrelated tenant queue length = %d, want 1", got)
	}
	<-queued
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == 0 })
}

func TestChannelBusWakeupQueuesShareProcessWideByteBudget(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	newBus := func(name string) (*MultiBus, EventBus) {
		inner := NewChannelBusWithLimits(2, DefaultMaxEventBytes, 2*DefaultMaxEventBytes, filepath.Join(t.TempDir(), name))
		queueAware := inner.(interface {
			SetQueueAdmission(*QueueAdmission) error
		})
		if err := queueAware.SetQueueAdmission(budget); err != nil {
			t.Fatal(err)
		}
		bus, err := NewDeferredMultiBus(inner)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.SetQueueAdmission(budget); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = bus.Close() })
		return bus, inner
	}
	first, firstInner := newBus("first")
	second, secondInner := newBus("second")
	event := ChangeEvent{Site: "tenant", Doctype: "Thing", Timestamp: time.Now().UTC()}
	if err := first.Publish(event); err != nil {
		t.Fatal("publish first durable wake-up:", err)
	}
	if err := second.Publish(event); err != nil {
		t.Fatal("publish second durable wake-up:", err)
	}
	firstQueue := firstInner.(interface{ QueueBudget() QueueBudgetSnapshot }).QueueBudget()
	secondQueue := secondInner.(interface{ QueueBudget() QueueBudgetSnapshot }).QueueBudget()
	if firstQueue.QueuedItems != 1 || secondQueue.QueuedItems != 0 {
		t.Fatalf("tenant wake-up queues = (%+v, %+v), want only the first queued", firstQueue, secondQueue)
	}
	if snapshot := budget.Snapshot(); snapshot.UsedBytes != DefaultMaxEventBytes || snapshot.RejectedItems != 1 {
		t.Fatalf("aggregate analytics queue budget = %+v", snapshot)
	}
	if err := first.Start(); err != nil {
		t.Fatal("start first tenant fanout:", err)
	}
	if err := second.Start(); err != nil {
		t.Fatal("start second tenant fanout:", err)
	}
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == 0 })
	var replayed int
	count, err := secondInner.(interface {
		DrainWAL(func(ChangeEvent)) (int, error)
	}).DrainWAL(func(ChangeEvent) { replayed++ })
	if err != nil || count != 1 || replayed != 1 {
		t.Fatalf("WAL replay after skipped wake-up = (%d events, %d callbacks, %v), want durable event", count, replayed, err)
	}
}

func TestMultiBusPublishFansOutOnceWhenSourceWakeupsAreConsumed(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(8 * DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	inner := NewChannelBusWithLimits(4, DefaultMaxEventBytes, 4*DefaultMaxEventBytes, filepath.Join(t.TempDir(), "wal"))
	if err := inner.(interface {
		SetQueueAdmission(*QueueAdmission) error
	}).SetQueueAdmission(budget); err != nil {
		t.Fatal("configure source queue admission:", err)
	}
	bus, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.SetQueueAdmission(budget); err != nil {
		t.Fatal("configure subscriber queue admission:", err)
	}
	defer bus.Close()
	listener := bus.NewListenerQueue(false)
	if err := bus.TryAddListener(listener); err != nil {
		t.Fatal("register listener:", err)
	}
	event := ChangeEvent{ID: "duplicate-source-event", Site: "tenant", Doctype: "Thing", Timestamp: time.Now().UTC()}
	for range 2 {
		if err := bus.Publish(event); err != nil {
			t.Fatal("publish through MultiBus:", err)
		}
	}
	if len(listener) != 2 {
		t.Fatalf("direct fan-out delivered %d events, want 2", len(listener))
	}
	if err := bus.Start(); err != nil {
		t.Fatal("start fanout:", err)
	}
	innerQueue := inner.(interface{ QueueBudget() QueueBudgetSnapshot })
	deadline := time.Now().Add(time.Second)
	for innerQueue.QueueBudget().QueuedItems != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(listener) != 2 {
		t.Fatalf("source wake-up duplicated subscriber events; queue length=%d", len(listener))
	}
	for range 2 {
		if got := <-listener; got.ID != event.ID {
			t.Fatalf("delivered event ID = %q, want %q", got.ID, event.ID)
		}
	}
	waitForListenerBudget(t, budget, func(snapshot QueueSnapshot) bool { return snapshot.UsedBytes == 0 })
}

func TestMultiBusCloseBeforeStartReleasesAllQueueReservations(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(4 * DefaultMaxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	inner := NewChannelBusWithLimits(2, DefaultMaxEventBytes, 2*DefaultMaxEventBytes, filepath.Join(t.TempDir(), "wal"))
	if err := inner.(interface {
		SetQueueAdmission(*QueueAdmission) error
	}).SetQueueAdmission(budget); err != nil {
		t.Fatal(err)
	}
	bus, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.SetQueueAdmission(budget); err != nil {
		t.Fatal(err)
	}
	listener := bus.NewListenerQueue(false)
	if err := bus.TryAddListener(listener); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ChangeEvent{ID: "pending-close-event", Site: "tenant", Doctype: "Thing"}); err != nil {
		t.Fatal("publish before fanout start:", err)
	}
	if got := budget.Snapshot().UsedBytes; got != 2*DefaultMaxEventBytes {
		t.Fatalf("queued reservations before close = %d, want source plus listener payloads", got)
	}
	if err := bus.Close(); err != nil {
		t.Fatal("close unstarted fanout:", err)
	}
	if got := budget.Snapshot().UsedBytes; got != 0 {
		t.Fatalf("queued reservations after close = %d, want zero", got)
	}
}

func waitForListenerBudget(t *testing.T, budget *QueueAdmission, predicate func(QueueSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate(budget.Snapshot()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("listener queue budget did not reach expected state; snapshot=%+v", budget.Snapshot())
}

func TestDeferredMultiBusStartsFanoutOnlyWhenOwnedWorkerStarts(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal("NewDeferredMultiBus:", err)
	}
	defer mb.Close()
	listener := make(chan ChangeEvent, 1)
	mb.AddListener(listener)
	want := ChangeEvent{Site: "tenant", Doctype: "Invoice", Timestamp: time.Now().UTC()}
	inner.ch <- want
	select {
	case event := <-listener:
		t.Fatalf("event fanned out before Start: %+v", event)
	case <-time.After(10 * time.Millisecond):
	}
	if err := mb.Start(); err != nil {
		t.Fatal("Start:", err)
	}
	select {
	case got := <-listener:
		if got.Doctype != want.Doctype || got.Site != want.Site {
			t.Fatalf("fanned event = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("fanout did not start")
	}
}

func TestMultiBusBoundsListenerCountAndClosesRejectedListener(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.SetMaxListeners(1); err != nil {
		t.Fatal(err)
	}
	first := make(chan ChangeEvent, 1)
	mb.AddListener(first)
	second := make(chan ChangeEvent, 1)
	mb.AddListener(second)
	if _, ok := <-second; ok {
		t.Fatal("listener beyond limit remained open")
	}
	if mb.ListenerCount() != 1 || mb.Dropped() != 1 {
		t.Fatalf("listeners=%d dropped=%d", mb.ListenerCount(), mb.Dropped())
	}
	if causes := mb.DropCauses(); causes.SubscriberRegistrationsRejected != 1 || causes.SubscriberOverflows != 0 {
		t.Fatalf("DropCauses() = %+v, want one rejected registration only", causes)
	}
	mb.RemoveListener(first)
	if mb.ListenerCount() != 0 {
		t.Fatal("removed listener remains registered")
	}
	if err := mb.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMultiBusSubscribeReturnsClosedChannelWhenAtCapacity(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.SetMaxListeners(1); err != nil {
		t.Fatal(err)
	}
	mb.AddListener(make(chan ChangeEvent, 1))
	if ch, err := mb.Subscribe(); !errors.Is(err, ErrListenerLimit) || ch != nil {
		t.Fatalf("Subscribe beyond capacity = (%v, %v)", ch, err)
	}
	_ = mb.Close()
}

func TestMultiBusRejectsListenerQueueOverHardLimit(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	listener := make(chan ChangeEvent, MaxListenerQueueSize+1)
	if err := mb.TryAddListener(listener); err == nil {
		t.Fatal("listener exceeding queue capacity limit was accepted")
	}
	if _, ok := <-listener; ok {
		t.Fatal("rejected listener should be closed")
	}
	if got := mb.ListenerCount(); got != 0 {
		t.Fatalf("listener count = %d, want 0", got)
	}
}

func TestMultiBusListenerQueueCapacityUsesEventByteLimit(t *testing.T) {
	inner := NewChannelBusWithLimits(4, 4<<20, 16<<20, t.TempDir())
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal("create multi bus:", err)
	}
	defer mb.Close()
	if got := mb.ListenerQueueCapacity(false); got != 4 {
		t.Fatalf("generic queue capacity = %d, want 4", got)
	}
	if got := mb.ListenerQueueCapacity(true); got != 4 {
		t.Fatalf("realtime queue capacity = %d, want 4", got)
	}
	listener := make(chan ChangeEvent, 5)
	if err := mb.TryAddListener(listener); err == nil {
		t.Fatal("listener queue exceeding byte-derived limit was accepted")
	}
	if _, ok := <-listener; ok {
		t.Fatal("rejected listener remained open")
	}

	bounded := mb.NewListenerQueue(false)
	if cap(bounded) != 4 {
		t.Fatalf("created queue capacity = %d, want 4", cap(bounded))
	}
	if err := mb.TryAddListener(bounded); err != nil {
		t.Fatal("add byte-bounded listener:", err)
	}
	snapshot := mb.ListenerQueueBudget()
	if snapshot.Listeners != 1 || snapshot.MaxQueuedBytes != 16<<20 {
		t.Fatalf("listener queue snapshot = %+v", snapshot)
	}
}

func TestRealtimeListenerClosesAndIsRemovedOnOverflow(t *testing.T) {
	inner := &multiBusStub{}
	mb, err := NewDeferredMultiBus(inner)
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	listener := make(chan ChangeEvent, 1)
	if err := mb.AddRealtimeListener(listener); err != nil {
		t.Fatal("add realtime listener:", err)
	}
	if err := mb.Start(); err != nil {
		t.Fatal("start fanout:", err)
	}
	inner.ch <- ChangeEvent{ID: "evt-1"}
	deadline := time.After(time.Second)
	for len(listener) != 1 {
		select {
		case <-deadline:
			t.Fatal("fanout did not fill realtime listener")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	inner.ch <- ChangeEvent{ID: "evt-2"}
	deadline = time.After(time.Second)
	for mb.ListenerCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("overflowing event was not processed by fanout")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case _, ok := <-listener:
		if ok {
			t.Fatal("overflow did not discard queued realtime events")
		}
	case <-deadline:
		t.Fatal("realtime listener was not closed on overflow")
	}
	if mb.ListenerCount() != 0 || mb.Dropped() != 1 {
		t.Fatalf("listeners=%d dropped=%d, want 0 and 1", mb.ListenerCount(), mb.Dropped())
	}
	if causes := mb.DropCauses(); causes.SubscriberOverflows != 1 || causes.SubscriberRegistrationsRejected != 0 {
		t.Fatalf("DropCauses() = %+v, want one subscriber overflow only", causes)
	}
}
