package eventbus

import (
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/eventstream"
)

func TestChannelBusDroppedCountsWALFailures(t *testing.T) {
	bus := &channelBus{
		ch:             make(chan eventstream.ChangeEvent, 1),
		walDir:         "/proc/analytics_test_wal",
		capacity:       1,
		maxEventBytes:  DefaultMaxEventBytes,
		maxQueuedBytes: DefaultMaxQueuedBytes,
	}

	_ = bus.Publish(eventstream.ChangeEvent{
		Site:      "test",
		Doctype:   "Customer",
		DocName:   "CUST-1",
		Operation: eventstream.EventInsert,
		Timestamp: time.Now().UTC(),
	})

	if got := bus.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	if causes := bus.DropCauses(); causes.WALWriteFailures != 1 || causes.PayloadRejected != 0 {
		t.Fatalf("DropCauses() = %+v, want one WAL failure only", causes)
	}
}
