package eventbus

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/eventstream"
)

func testChangeEvent(id string, data map[string]any) eventstream.ChangeEvent {
	return eventstream.ChangeEvent{
		ID:        id,
		Site:      "test",
		Doctype:   "Customer",
		DocName:   "CUST-1",
		Operation: eventstream.EventInsert,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
}

func TestChannelBusPublishSubscribeStaysInMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	bus := NewChannelBus(4, 0)
	ch, err := bus.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(testChangeEvent("evt-1", nil)); err != nil {
		t.Fatal("publish:", err)
	}
	select {
	case got := <-ch:
		if got.ID != "evt-1" {
			t.Fatalf("received event %q, want evt-1", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("published event was not queued in memory")
	}
	if _, err := os.Stat("data"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publish wrote to disk: stat data = %v", err)
	}
}

func TestChannelBusRejectsOversizedEvent(t *testing.T) {
	bus := NewChannelBus(1, 64).(*channelBus)
	err := bus.Publish(testChangeEvent("evt-big", map[string]any{"blob": strings.Repeat("x", 128)}))
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("Publish() error = %v, want ErrEventTooLarge", err)
	}
	if causes := bus.DropCauses(); causes.PayloadRejected != 1 || bus.Dropped() != 1 {
		t.Fatalf("DropCauses() = %+v dropped=%d, want one payload rejection", causes, bus.Dropped())
	}
}

func TestChannelBusConstructorBoundsCapacity(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		want     int
	}{
		{name: "default for zero", capacity: 0, want: 1000},
		{name: "default for negative", capacity: -5, want: 1000},
		{name: "explicit", capacity: 16, want: 16},
		{name: "capped", capacity: MaxChannelCapacity + 1, want: MaxChannelCapacity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := NewChannelBus(tt.capacity, 0).Subscribe()
			if err != nil {
				t.Fatal(err)
			}
			if got := cap(ch); got != tt.want {
				t.Fatalf("capacity = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestChannelBusRefusesPublishAfterClose(t *testing.T) {
	bus := NewChannelBus(1, 0)
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(testChangeEvent("late", nil)); err == nil {
		t.Fatal("publish after close succeeded")
	}
}

// A subscriber that never reads must lose events, and the loss must be
// counted. The overflow path must not touch disk.
func TestSlowSubscriberDropsAreCountedWithoutDiskWrites(t *testing.T) {
	t.Chdir(t.TempDir())
	mb, err := NewMultiBus(NewChannelBus(8, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mb.Close() })

	listener := make(chan eventstream.ChangeEvent, 1)
	if err := mb.TryAddListener(listener); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := mb.Publish(testChangeEvent(string(rune('a'+i)), nil)); err != nil {
			t.Fatal("publish:", err)
		}
	}
	if got := mb.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d, want 2 for a listener with one free slot", got)
	}
	if causes := mb.DropCauses(); causes.SubscriberOverflows != 2 {
		t.Fatalf("DropCauses() = %+v, want 2 subscriber overflows", causes)
	}
	if _, err := os.Stat("data"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflow wrote to disk: stat data = %v", err)
	}
}
