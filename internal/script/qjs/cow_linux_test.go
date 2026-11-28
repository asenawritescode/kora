//go:build linux

package qjs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unsafe"
)

// copyLimits is testLimits with the copy-reset fallback forced.
var copyLimits = func() Limits { l := testLimits; l.CopyReset = true; return l }()

// memoryBytes returns the instance's whole accessible linear memory.
func memoryBytes(t *testing.T, inst *Instance) []byte {
	t.Helper()
	mem := inst.mod.Memory()
	b, ok := mem.Read(0, mem.Size())
	if !ok {
		t.Fatal("read guest memory")
	}
	return b
}

// reseedSlack bounds the bytes a reseed may change: reseedRandom writes the 8-byte PRNG
// state into guest memory on every restore and reset, by design.
const reseedSlack = 16

// requireBytesEqual fails unless the instance memory matches the snapshot up to the
// snapshot size, apart from the PRNG reseed, and is zero beyond it. Dirty pages left by
// a run would differ by thousands of bytes, far over the slack.
func requireBytesEqual(t *testing.T, inst *Instance, snap *Snapshot) {
	t.Helper()
	got := memoryBytes(t, inst)
	diff := 0
	for i, b := range snap.memory {
		if got[i] != b {
			diff++
		}
	}
	if diff > reseedSlack {
		t.Fatalf("%d bytes below the snapshot size differ after reset, want at most %d (reseed)", diff, reseedSlack)
	}
	if tail := got[len(snap.memory):]; bytes.Count(tail, []byte{0}) != len(tail) {
		t.Fatal("memory above the snapshot size is not zero after reset")
	}
}

func TestResetRestoresSnapshotBytesForEachMode(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		cow    bool
	}{
		{name: "copy-on-write", limits: testLimits, cow: true},
		{name: "copy reset fallback", limits: copyLimits, cow: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, err := NewEngine(context.Background(), tc.limits)
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			t.Cleanup(func() { _ = eng.Close(context.Background()) })
			snap := snapshotOf(t, eng)
			t.Cleanup(snap.Close)
			if (snap.image != nil) != tc.cow {
				t.Fatalf("snapshot image present = %v, want %v", snap.image != nil, tc.cow)
			}
			inst := restoredInstance(t, eng, snap)
			if (inst.cow != nil) != tc.cow {
				t.Fatalf("instance cow memory = %v, want %v", inst.cow != nil, tc.cow)
			}
			for round := range 3 {
				if _, err := inst.Eval(context.Background(), `globalThis.keep = []; for (var i = 0; i < 2000; i++) keep.push("row" + i + "x".repeat(64)); keep.length`, 0); err != nil {
					t.Fatalf("round %d: dirty the instance: %v", round, err)
				}
				if err := inst.Reset(context.Background(), snap); err != nil {
					t.Fatalf("round %d: Reset: %v", round, err)
				}
				requireBytesEqual(t, inst, snap)
			}
			if mapped := inst.cow != nil && inst.cow.mapped > 0; mapped != tc.cow {
				t.Fatalf("snapshot mapped into memory = %v, want %v", mapped, tc.cow)
			}
		})
	}
}

func TestResetZeroesMemoryGrownPastSnapshot(t *testing.T) {
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	t.Cleanup(snap.Close)
	inst := restoredInstance(t, eng, snap)
	before := inst.MemoryBytes()
	if _, err := inst.Eval(context.Background(), `globalThis.big = new Array(300000).fill(7); big.length`, 0); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if inst.MemoryBytes() <= before {
		t.Fatalf("memory did not grow past %d bytes; the test needs a grown instance", before)
	}
	if err := inst.Reset(context.Background(), snap); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	requireBytesEqual(t, inst, snap)
}

func TestCowInstanceMemoryKeepsItsAddress(t *testing.T) {
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	t.Cleanup(snap.Close)
	inst := restoredInstance(t, eng, snap)
	if inst.cow == nil || inst.cow.reserved == nil {
		t.Fatal("expected copy-on-write memory with a reservation")
	}
	base := unsafe.Pointer(&inst.cow.reserved[0])
	if _, err := inst.Eval(context.Background(), `globalThis.big = new Array(300000).fill(1); 1`, 0); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if got := unsafe.Pointer(&inst.cow.reserved[0]); got != base {
		t.Fatalf("reservation moved from %p to %p on grow", base, got)
	}
	if err := inst.Reset(context.Background(), snap); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := unsafe.Pointer(&inst.cow.reserved[0]); got != base {
		t.Fatalf("reservation moved from %p to %p on reset", base, got)
	}
}

// mappingAt reports whether /proc/self/maps has a mapping starting at addr.
func mappingAt(t *testing.T, addr uintptr) bool {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Skipf("no /proc/self/maps: %v", err)
	}
	prefix := fmt.Sprintf("%x-", addr)
	for _, line := range strings.Split(string(maps), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func TestCowCloseUnmapsReservationAndImage(t *testing.T) {
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	if snap.image == nil {
		t.Fatal("expected a memfd image")
	}
	fd := snap.image.fd
	const instances = 5
	var bases []uintptr
	for range instances {
		inst := restoredInstance(t, eng, snap)
		bases = append(bases, uintptr(unsafe.Pointer(&inst.cow.reserved[0])))
		if _, err := inst.Eval(context.Background(), `1`, 0); err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if err := inst.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	for _, base := range bases {
		if mappingAt(t, base) {
			t.Fatalf("reservation at %#x still mapped after Close", base)
		}
	}
	snap.Close()
	if _, err := os.Stat(fmt.Sprintf("/proc/self/fd/%d", fd)); err == nil {
		t.Fatalf("memfd %d still open after Snapshot.Close", fd)
	}
	if snap.image.fd != -1 {
		t.Fatal("image fd not marked closed")
	}
}

func TestCowMemoryHeapFallback(t *testing.T) {
	// A zero max makes the reservation fail, so the allocator returns heap memory.
	var alloc cowAllocator
	mem := alloc.Allocate(wasmPageBytes, 0)
	b := mem.Reallocate(wasmPageBytes)
	if len(b) != wasmPageBytes {
		t.Fatalf("heap memory len = %d, want %d", len(b), wasmPageBytes)
	}
	b[0] = 42
	grown := mem.Reallocate(2 * wasmPageBytes)
	if len(grown) != 2*wasmPageBytes || grown[0] != 42 {
		t.Fatalf("grow lost contents: len %d, first byte %d", len(grown), grown[0])
	}
	mem.Free()
	if err := alloc.mem.mapImage(&cowImage{fd: -1}, wasmPageBytes); err == nil {
		t.Fatal("mapImage on heap memory succeeded, want errNoCOW")
	}
}

func TestCowMapImageRefusesClosedImage(t *testing.T) {
	eng := newTestEngine(t)
	snap := snapshotOf(t, eng)
	inst := restoredInstance(t, eng, snap)
	snap.Close()
	if err := inst.cow.mapImage(snap.image, len(snap.memory)); err == nil {
		t.Fatal("mapImage from a closed image succeeded")
	}
}
