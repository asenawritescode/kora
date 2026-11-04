package analytics

import (
	"testing"
	"time"
)

func TestChannelBusDroppedCountsWALFailures(t *testing.T) {
	bus := &channelBus{
		ch:             make(chan ChangeEvent, 1),
		walDir:         "/proc/analytics_test_wal",
		capacity:       1,
		maxEventBytes:  DefaultMaxEventBytes,
		maxQueuedBytes: DefaultMaxQueuedBytes,
	}

	_ = bus.Publish(ChangeEvent{
		Site:      "test",
		Doctype:   "Customer",
		DocName:   "CUST-1",
		Operation: EventInsert,
		Timestamp: time.Now().UTC(),
	})

	if got := bus.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	if causes := bus.DropCauses(); causes.WALWriteFailures != 1 || causes.PayloadRejected != 0 {
		t.Fatalf("DropCauses() = %+v, want one WAL failure only", causes)
	}
}
