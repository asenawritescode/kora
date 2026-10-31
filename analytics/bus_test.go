package analytics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestChannelBus_PublishSubscribe(t *testing.T) {
	bus := NewChannelBus(10, t.TempDir()+"/wal")
	defer bus.Close()

	ch, err := bus.Subscribe()
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	event := ChangeEvent{
		Site:      "test.local",
		Doctype:   "Customer",
		DocName:   "CUST-0001",
		Operation: EventInsert,
		Timestamp: time.Now(),
		Data:      map[string]any{"name": "Acme Corp"},
	}

	if err := bus.Publish(event); err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	select {
	case received := <-ch:
		if received.Doctype != "Customer" {
			t.Errorf("expected doctype 'Customer', got %q", received.Doctype)
		}
		if received.DocName != "CUST-0001" {
			t.Errorf("expected doc 'CUST-0001', got %q", received.DocName)
		}
		if received.Operation != EventInsert {
			t.Errorf("expected op 'insert', got %q", received.Operation)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	// Verify the event was also written to WAL (WAL-first durability).
	walPath := filepath.Join(bus.(*channelBus).walDir, "events.jsonl")
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("WAL file not found after publish: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("WAL should contain the published event")
	}
	t.Logf("WAL content: %s", string(data))
}

func TestChannelBusRejectsOversizedEventBeforeWALWrite(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	bus := NewChannelBusWithMaxEventBytes(4, 96, walDir).(*channelBus)
	defer bus.Close()
	err := bus.Publish(ChangeEvent{
		Site: "tenant", Doctype: "Record", DocName: "large", Operation: EventInsert,
		Data: map[string]any{"payload": strings.Repeat("x", 512)},
	})
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("Publish error = %v, want ErrEventTooLarge", err)
	}
	if got := bus.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(walDir, "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("oversized event created WAL, stat error = %v", err)
	}
}

func TestChannelBusConstructorCapsChannelCapacity(t *testing.T) {
	bus := NewChannelBus(MaxChannelCapacity+1, "").(*channelBus)
	defer bus.Close()
	if got := cap(bus.ch); got > MaxChannelCapacity || got*bus.maxEventBytes > bus.maxQueuedBytes {
		t.Fatalf("channel capacity = %d violates item or payload-byte maximum", got)
	}
}

func TestChannelBusBoundsWakeupQueueByPayloadBytesWithoutLosingWALWork(t *testing.T) {
	bus := NewChannelBusWithLimits(10, 1024, 2048, filepath.Join(t.TempDir(), "wal")).(*channelBus)
	defer bus.Close()
	if got := cap(bus.ch); got != 2 {
		t.Fatalf("effective channel capacity = %d, want 2", got)
	}
	for i := 0; i < 3; i++ {
		if err := bus.Publish(ChangeEvent{ID: fmt.Sprintf("evt-%d", i), Site: "tenant", Doctype: "Thing", DocName: fmt.Sprint(i), Operation: EventInsert}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	snapshot := bus.QueueBudget()
	if snapshot.QueuedItems != 2 || snapshot.UpperBoundBytes != 2048 || snapshot.MaxQueuedBytes != 2048 || snapshot.OldestAge <= 0 {
		t.Fatalf("queue budget snapshot = %+v", snapshot)
	}
	var drained []string
	count, err := bus.DrainWAL(func(event ChangeEvent) { drained = append(drained, event.ID) })
	if err != nil {
		t.Fatal("drain durable events:", err)
	}
	if count != 3 || len(drained) != 3 {
		t.Fatalf("WAL drained %d events %v, want all three including event without a wake-up slot", count, drained)
	}
}

func TestChannelBusWALBacklogAgeSurvivesRestartAndAcknowledgment(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	bus := NewChannelBus(4, walDir).(*channelBus)
	if err := bus.Publish(ChangeEvent{ID: "older", Site: "tenant", Doctype: "Thing", DocName: "1", Operation: EventInsert}); err != nil {
		t.Fatal("publish older event:", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := bus.Publish(ChangeEvent{ID: "newer", Site: "tenant", Doctype: "Thing", DocName: "2", Operation: EventInsert}); err != nil {
		t.Fatal("publish newer event:", err)
	}
	before := bus.WALBacklog()
	walInfo, err := os.Stat(filepath.Join(walDir, "events.jsonl"))
	if err != nil {
		t.Fatal("stat live analytics WAL:", err)
	}
	if !before.Pending || !before.AgeKnown || before.OldestAge < 5*time.Millisecond || !before.BytesKnown || before.Bytes != walInfo.Size() {
		t.Fatalf("live WAL backlog = %+v, want known pending age", before)
	}
	if err := bus.Close(); err != nil {
		t.Fatal("close original bus:", err)
	}

	restarted := NewChannelBus(4, walDir).(*channelBus)
	defer restarted.Close()
	afterRestartAt := time.Now()
	afterRestart := restarted.WALBacklog()
	if !afterRestart.Pending || !afterRestart.AgeKnown || afterRestart.OldestAge < before.OldestAge || !afterRestart.BytesKnown || afterRestart.Bytes != before.Bytes {
		t.Fatalf("recovered WAL backlog = %+v, before restart %+v", afterRestart, before)
	}
	if err := restarted.AcknowledgeWAL([]string{"older"}); err != nil {
		t.Fatal("acknowledge oldest record:", err)
	}
	remainingAt := time.Now()
	remaining := restarted.WALBacklog()
	// Compare the persistence instants reconstructed from each snapshot. The
	// second age naturally includes time spent acknowledging the first record,
	// so comparing raw durations from different sampling times is racy.
	if !remaining.Pending || !remaining.AgeKnown || !remaining.BytesKnown || remaining.Bytes <= 0 || remaining.Bytes >= afterRestart.Bytes || !remainingAt.Add(-remaining.OldestAge).After(afterRestartAt.Add(-afterRestart.OldestAge)) {
		t.Fatalf("WAL after partial acknowledgment = %+v, want younger known record", remaining)
	}
	if err := restarted.AcknowledgeWAL([]string{"newer"}); err != nil {
		t.Fatal("acknowledge final record:", err)
	}
	if empty := restarted.WALBacklog(); empty.Pending || !empty.AgeKnown || empty.OldestAge != 0 || !empty.BytesKnown || empty.Bytes != 0 {
		t.Fatalf("empty WAL backlog = %+v, want no pending records and known age", empty)
	}
}

func TestChannelBusLegacyWALBacklogAgeIsExplicitlyUnknown(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	if err := os.MkdirAll(walDir, 0700); err != nil {
		t.Fatal("create WAL directory:", err)
	}
	legacy, err := json.Marshal(ChangeEvent{ID: "legacy", Site: "tenant", Doctype: "Thing", DocName: "1", Operation: EventInsert})
	if err != nil {
		t.Fatal("encode legacy event:", err)
	}
	if err := os.WriteFile(filepath.Join(walDir, "events.jsonl"), append(legacy, '\n'), 0600); err != nil {
		t.Fatal("write legacy WAL:", err)
	}
	bus := NewChannelBus(4, walDir).(*channelBus)
	defer bus.Close()
	if snapshot := bus.WALBacklog(); !snapshot.Pending || snapshot.AgeKnown || snapshot.OldestAge != 0 {
		t.Fatalf("legacy WAL backlog = %+v, want pending with explicitly unknown age", snapshot)
	}
	if err := bus.AcknowledgeWAL([]string{"legacy"}); err != nil {
		t.Fatal("acknowledge legacy record:", err)
	}
	if snapshot := bus.WALBacklog(); snapshot.Pending || !snapshot.AgeKnown {
		t.Fatalf("empty legacy WAL backlog = %+v, want known empty state", snapshot)
	}
}

func TestChannelBusRecoversInterruptedWALAcknowledgmentRewrite(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	if err := os.MkdirAll(walDir, 0700); err != nil {
		t.Fatal("create WAL directory:", err)
	}
	event := ChangeEvent{ID: "interrupted-ack", Site: "tenant", Doctype: "Thing", DocName: "1", Operation: EventInsert}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal("encode WAL event:", err)
	}
	ackPath := filepath.Join(walDir, ".wal-ack-interrupted")
	if err := os.WriteFile(ackPath, append(line, '\n'), 0600); err != nil {
		t.Fatal("write interrupted acknowledgment file:", err)
	}

	pending, err := WALHasPending(walDir)
	if err != nil || !pending {
		t.Fatalf("WALHasPending() = (%t, %v), want pending record", pending, err)
	}
	bus := NewChannelBus(4, walDir).(*channelBus)
	defer bus.Close()
	var replayed []ChangeEvent
	count, err := bus.DrainWAL(func(got ChangeEvent) { replayed = append(replayed, got) })
	if err != nil || count != 1 || len(replayed) != 1 || replayed[0].ID != event.ID {
		t.Fatalf("DrainWAL() = (%d, %v), events=%+v; want recovered event %q", count, err, replayed, event.ID)
	}
	if err := bus.AcknowledgeWAL([]string{event.ID}); err != nil {
		t.Fatal("acknowledge recovered event:", err)
	}
	if _, err := os.Stat(ackPath); !os.IsNotExist(err) {
		t.Fatalf("interrupted acknowledgment file still exists: %v", err)
	}
	if backlog := bus.WALBacklog(); backlog.Pending || !backlog.BytesKnown {
		t.Fatalf("WAL backlog after recovery = %+v, want known empty", backlog)
	}
}

func TestChannelBusByteBudgetBelowEventLimitLowersEffectiveEventLimit(t *testing.T) {
	bus := NewChannelBusWithLimits(4, 2048, 1024, filepath.Join(t.TempDir(), "wal")).(*channelBus)
	defer bus.Close()
	if bus.maxEventBytes != 1024 || cap(bus.ch) != 1 {
		t.Fatalf("effective event/channel limits = (%d, %d), want (1024, 1)", bus.maxEventBytes, cap(bus.ch))
	}
	err := bus.Publish(ChangeEvent{ID: "oversized-for-queue-budget", Site: "tenant", Doctype: "Thing", DocName: "1", Operation: EventInsert, Data: map[string]any{"body": strings.Repeat("x", 1500)}})
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("Publish error = %v, want ErrEventTooLarge under effective event limit", err)
	}
}

func TestChannelBusDrainReportsAndPreservesCorruptWALRecords(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal[recovery]")
	bus := NewChannelBus(8, walDir).(*channelBus)
	defer bus.Close()
	if err := bus.Publish(ChangeEvent{ID: "evt-before", Site: "tenant", Doctype: "Task", DocName: "1", Operation: EventInsert}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(bus.currentWALPath(), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{truncated record}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ChangeEvent{ID: "evt-after", Site: "tenant", Doctype: "Task", DocName: "2", Operation: EventInsert}); err != nil {
		t.Fatal(err)
	}

	var ids []string
	count, err := bus.DrainWAL(func(event ChangeEvent) { ids = append(ids, event.ID) })
	if err == nil {
		t.Fatalf("DrainWAL = (%d, %v), want two valid events and a corruption error", count, err)
	}
	if count != 2 || len(ids) != 2 || ids[0] != "evt-before" || ids[1] != "evt-after" {
		t.Fatalf("drained valid records = %d, %v", count, ids)
	}
	if err := bus.AcknowledgeWAL(ids); err != nil {
		t.Fatalf("acknowledge valid records: %v", err)
	}
	flushing, err := listFlushingWALs(walDir)
	if err != nil || len(flushing) != 1 {
		t.Fatalf("corrupt WAL file was not retained: %v, %v", flushing, err)
	}
	remaining, err := os.ReadFile(flushing[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(remaining) != "{truncated record}\n" {
		t.Fatalf("corrupt WAL bytes changed or were acknowledged: %q", remaining)
	}
}

func TestChannelBus_BufferFull_StillWritesToWAL(t *testing.T) {
	tmpDir := t.TempDir()
	walDir := filepath.Join(tmpDir, "wal")
	bus := NewChannelBus(1, walDir) // buffer of 1
	defer bus.Close()

	// With WAL-first, ALL events go to WAL regardless of channel state.
	// The channel is just a wake-up optimization.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "A", DocName: "1", Operation: EventInsert})
	bus.Publish(ChangeEvent{Site: "test", Doctype: "B", DocName: "2", Operation: EventInsert})

	// dropped should be 0 — WAL writes succeeded for both.
	if dropped := bus.Dropped(); dropped != 0 {
		t.Errorf("expected 0 dropped events, got %d", dropped)
	}

	// Both events should be in the WAL.
	walPath := filepath.Join(walDir, "events.jsonl")
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("WAL file not created: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("WAL file is empty")
	}
	t.Logf("WAL content: %s", string(data))
}

func TestChannelBusPublishReturnsWALWriteFailure(t *testing.T) {
	blockedWalPath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedWalPath, []byte("block"), 0600); err != nil {
		t.Fatal("create blocking file:", err)
	}
	bus := NewChannelBus(1, blockedWalPath)
	defer bus.Close()
	if err := bus.Publish(ChangeEvent{Site: "test", Doctype: "A", DocName: "1", Operation: EventInsert}); err == nil {
		t.Fatal("Publish returned success although WAL persistence failed")
	}
	if got := bus.(*channelBus).Dropped(); got != 1 {
		t.Fatalf("dropped events = %d, want 1", got)
	}
}

func TestChannelBus_DrainWAL(t *testing.T) {
	tmpDir := t.TempDir()
	walDir := filepath.Join(tmpDir, "wal")
	bus := NewChannelBus(1, walDir) // tiny buffer
	defer bus.Close()

	// With WAL-first, all events go to WAL. Publish 3 events.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "A", DocName: "1", Operation: EventInsert})
	bus.Publish(ChangeEvent{Site: "test", Doctype: "B", DocName: "2", Operation: EventInsert})
	bus.Publish(ChangeEvent{Site: "test", Doctype: "C", DocName: "3", Operation: EventInsert})

	// Drain WAL.
	var drained []ChangeEvent
	var mu sync.Mutex
	count, err := bus.DrainWAL(func(e ChangeEvent) {
		mu.Lock()
		drained = append(drained, e)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("DrainWAL failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 drained events (all written to WAL), got %d", count)
	}
	if len(drained) != 3 {
		t.Fatalf("expected 3 callbacks, got %d", len(drained))
	}

	// Replay is non-destructive until the database write is acknowledged.
	flushing, err := listFlushingWALs(walDir)
	if err != nil || len(flushing) != 1 {
		t.Fatalf("durable WAL files after drain = %v, %v; want one unacknowledged file", flushing, err)
	}
	info, err := os.Stat(flushing[0])
	if err != nil || info.Size() == 0 {
		t.Fatalf("WAL was cleared before durable acknowledgment: stat=%v, err=%v", info, err)
	}
	if err := bus.CommitWALRotation(""); err != nil {
		t.Fatal(err)
	}
}

func TestChannelBusDrainWALBatchRecoversOverflowInBoundedBatches(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	bus := NewChannelBus(1, walDir).(*channelBus)
	defer bus.Close()
	for i := 1; i <= 4; i++ {
		if err := bus.Publish(ChangeEvent{ID: fmt.Sprintf("evt-%d", i), Site: "tenant", Doctype: "Task", DocName: fmt.Sprint(i), Operation: EventInsert}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(bus.ch); got != 1 {
		t.Fatalf("wake channel length = %d, want capacity 1", got)
	}

	var first []ChangeEvent
	if count, err := bus.DrainWALBatch(2, func(event ChangeEvent) { first = append(first, event) }); err != nil || count != 2 {
		t.Fatalf("first bounded drain=(%d,%v), want 2 events", count, err)
	}
	if err := bus.AcknowledgeWAL([]string{first[0].ID, first[1].ID}); err != nil {
		t.Fatalf("acknowledge first batch: %v", err)
	}
	var second []ChangeEvent
	if count, err := bus.DrainWALBatch(2, func(event ChangeEvent) { second = append(second, event) }); err != nil || count != 2 {
		t.Fatalf("second bounded drain=(%d,%v), want remaining overflow events", count, err)
	}
	if second[0].ID != "evt-3" || second[1].ID != "evt-4" {
		t.Fatalf("second batch IDs = %q, %q; want evt-3, evt-4", second[0].ID, second[1].ID)
	}
}

func TestChannelBus_DrainWAL_WithFlushingFiles(t *testing.T) {
	tmpDir := t.TempDir()
	walDir := filepath.Join(tmpDir, "wal")
	bus := NewChannelBus(1, walDir)
	defer bus.Close()

	// Publish 2 events to the WAL.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "X", DocName: "1", Operation: EventInsert})
	bus.Publish(ChangeEvent{Site: "test", Doctype: "Y", DocName: "2", Operation: EventInsert})

	// Rotate simulates a pre-flush rotation.
	oldPath, err := bus.RotateWAL()
	if err != nil {
		t.Fatalf("RotateWAL failed: %v", err)
	}
	if oldPath == "" {
		t.Fatal("expected a rotated WAL path")
	}

	// Publish 1 more event to the new WAL.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "Z", DocName: "3", Operation: EventInsert})

	// Drain should replay both the .flushing file and the current WAL.
	count, err := bus.DrainWAL(func(e ChangeEvent) {})
	if err != nil {
		t.Fatalf("DrainWAL failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected 3 drained events (2 in .flushing + 1 in current), got %d", count)
	}

	// The .flushing file remains available until the flush acknowledges it.
	if _, err := os.Stat(oldPath); err != nil {
		t.Error(".flushing file disappeared before acknowledgment")
	}

	if err := bus.CommitWALRotation(oldPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error(".flushing file remained after acknowledgment")
	}
}

func TestChannelBusReplayCanRetryBeforeAcknowledgment(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	bus := NewChannelBus(4, walDir)
	if err := bus.Publish(ChangeEvent{Site: "test", Doctype: "Invoice", DocName: "1", Operation: EventInsert}); err != nil {
		t.Fatal(err)
	}
	count, err := bus.DrainWAL(func(ChangeEvent) {})
	if err != nil || count != 1 {
		t.Fatalf("first drain=(%d,%v)", count, err)
	}
	if err := bus.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := NewChannelBus(4, walDir)
	defer restarted.Close()
	count, err = restarted.DrainWAL(func(ChangeEvent) {})
	if err != nil || count != 1 {
		t.Fatalf("retry drain=(%d,%v), want 1", count, err)
	}
	if err := restarted.CommitWALRotation(""); err != nil {
		t.Fatal(err)
	}
}

func TestChannelBusAcknowledgesOnlyCommittedEventIDs(t *testing.T) {
	walDir := filepath.Join(t.TempDir(), "wal")
	bus := NewChannelBus(4, walDir)
	defer bus.Close()
	for i := 1; i <= 3; i++ {
		if err := bus.Publish(ChangeEvent{Site: "test", Doctype: "Invoice", DocName: fmt.Sprint(i), Operation: EventInsert}); err != nil {
			t.Fatal(err)
		}
	}
	var events []ChangeEvent
	if _, err := bus.DrainWAL(func(event ChangeEvent) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	if err := bus.(EventWALAcknowledger).AcknowledgeWAL([]string{events[0].ID, events[1].ID}); err != nil {
		t.Fatal(err)
	}
	var remaining []ChangeEvent
	if count, err := bus.DrainWAL(func(event ChangeEvent) { remaining = append(remaining, event) }); err != nil || count != 1 {
		t.Fatalf("remaining drain=(%d,%v), want one event", count, err)
	}
	if remaining[0].ID != events[2].ID {
		t.Fatalf("remaining event %q, want queued event %q", remaining[0].ID, events[2].ID)
	}
}

func TestChannelBus_RotateWAL_Empty(t *testing.T) {
	bus := NewChannelBus(1, t.TempDir()+"/wal")
	defer bus.Close()

	// Rotating an empty WAL (no events published) should return "".
	oldPath, err := bus.RotateWAL()
	if err != nil {
		t.Fatalf("RotateWAL on empty should not error: %v", err)
	}
	if oldPath != "" {
		t.Errorf("expected empty string for empty WAL rotation, got %q", oldPath)
	}
}

func TestChannelBus_Close(t *testing.T) {
	bus := NewChannelBus(10, t.TempDir()+"/wal")
	ch, _ := bus.Subscribe()

	if err := bus.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Channel should be closed after Close.
	_, ok := <-ch
	if ok {
		t.Error("channel should be closed after Close()")
	}

	// Publish after close should not panic.
	err := bus.Publish(ChangeEvent{})
	if err == nil {
		t.Error("Publish after Close should return error")
	}
}

func TestChannelBus_Dropped(t *testing.T) {
	tmpDir := t.TempDir()
	walDir := filepath.Join(tmpDir, "wal")

	// Create a WAL file where a directory should be to trigger write errors.
	// This simulates a disk-full or permission-error scenario.
	os.MkdirAll(walDir, 0755)
	// Create a FILE named "events.jsonl" inside walDir to make WAL writes fail
	// (os.OpenFile with O_APPEND on a directory will fail on Linux).
	// Actually, make the walDir itself a regular file — then MkdirAll fails.
	// Simpler: use a path that can't be written to.
	invalidBus := &channelBus{
		ch:     make(chan ChangeEvent, 1),
		walDir: "/proc/analytics_test_wal", // /proc is a virtual filesystem, can't create dirs
	}

	// Publish should fail the WAL write.
	invalidBus.Publish(ChangeEvent{Site: "a", Doctype: "a", DocName: "a", Operation: EventInsert})
	if d := invalidBus.Dropped(); d != 1 {
		t.Errorf("expected 1 dropped (WAL write failed), got %d", d)
	}
}
