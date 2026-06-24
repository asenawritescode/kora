package analytics

import (
	"os"
	"path/filepath"
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
		Site:    "test.local",
		Doctype: "Customer",
		DocName: "CUST-0001",
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

	// WAL should be empty after drain.
	walPath := filepath.Join(walDir, "events.jsonl")
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("cannot stat WAL after drain: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("WAL should be empty after drain, got %d bytes", info.Size())
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

	// The .flushing file should be deleted after drain.
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error(".flushing file should be deleted after drain")
	}

	// Commit the rotation (clean up — file already deleted by drain).
	bus.CommitWALRotation(oldPath)
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
		ch:      make(chan ChangeEvent, 1),
		walDir:  "/proc/analytics_test_wal", // /proc is a virtual filesystem, can't create dirs
	}

	// Publish should fail the WAL write.
	invalidBus.Publish(ChangeEvent{Site: "a", Doctype: "a", DocName: "a", Operation: EventInsert})
	if d := invalidBus.Dropped(); d != 1 {
		t.Errorf("expected 1 dropped (WAL write failed), got %d", d)
	}
}
