package analytics

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestChannelBus_PublishSubscribe(t *testing.T) {
	bus := NewChannelBus(10, "")
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
}

func TestChannelBus_BufferFull_SpillsToWAL(t *testing.T) {
	tmpDir := t.TempDir()
	walDir := filepath.Join(tmpDir, "wal")
	bus := NewChannelBus(1, walDir) // buffer of 1
	defer bus.Close()

	// First event fills the buffer.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "A", DocName: "1", Operation: EventInsert})
	// Second event should spill to WAL (buffer full).
	bus.Publish(ChangeEvent{Site: "test", Doctype: "B", DocName: "2", Operation: EventInsert})

	if dropped := bus.Dropped(); dropped != 1 {
		t.Errorf("expected 1 dropped event, got %d", dropped)
	}

	// Verify WAL file exists and has content.
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

	// First event fills the buffer (not WAL). Drain it.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "A", DocName: "1", Operation: EventInsert})
	ch, _ := bus.Subscribe()
	<-ch // consume the buffered event

	// Now publish 3 more — all should spill to WAL since the buffer is empty
	// but Publish tries channel first. Actually, the channel IS empty now (we consumed),
	// so the next events go to channel, not WAL.

	// The right approach: fill channel, then publish MORE to force WAL.
	bus.Publish(ChangeEvent{Site: "test", Doctype: "X1", DocName: "d1", Operation: EventInsert}) // fills channel
	bus.Publish(ChangeEvent{Site: "test", Doctype: "X2", DocName: "d2", Operation: EventInsert}) // WAL
	bus.Publish(ChangeEvent{Site: "test", Doctype: "X3", DocName: "d3", Operation: EventInsert}) // WAL

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
	if count != 2 {
		t.Fatalf("expected 2 drained events, got %d", count)
	}
	if len(drained) != 2 {
		t.Fatalf("expected 2 callbacks, got %d", len(drained))
	}
	if drained[0].Doctype != "X2" {
		t.Errorf("first drained should be X2, got %s", drained[0].Doctype)
	}
	if drained[1].Doctype != "X3" {
		t.Errorf("second drained should be X3, got %s", drained[1].Doctype)
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

func TestChannelBus_Close(t *testing.T) {
	bus := NewChannelBus(10, "")
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
	bus := NewChannelBus(1, t.TempDir()+"/wal")
	defer bus.Close()

	if d := bus.Dropped(); d != 0 {
		t.Errorf("initial dropped count should be 0, got %d", d)
	}

	// Fill buffer.
	bus.Publish(ChangeEvent{Site: "a", Doctype: "a", DocName: "a", Operation: EventInsert})
	// Spill to WAL.
	bus.Publish(ChangeEvent{Site: "b", Doctype: "b", DocName: "b", Operation: EventInsert})

	if d := bus.Dropped(); d != 1 {
		t.Errorf("expected 1 dropped, got %d", d)
	}
}
