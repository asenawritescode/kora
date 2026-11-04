package engine

import (
	"context"
	"errors"
	"testing"
)

func TestRealtimeStreamAdmissionEnforcesCapacityAndIdempotentRelease(t *testing.T) {
	admission, err := NewRealtimeStreamAdmission(1)
	if err != nil {
		t.Fatal(err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal("first acquire:", err)
	}
	if _, err := admission.Acquire(context.Background()); !errors.Is(err, ErrRealtimeStreamCapacity) {
		t.Fatalf("second acquire error = %v, want capacity exhausted", err)
	}
	release()
	release()
	secondRelease, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal("acquire after release:", err)
	}
	secondRelease()
	snapshot := admission.Snapshot()
	if snapshot.Active != 0 || snapshot.Capacity != 1 || snapshot.Rejected != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRealtimeStreamAdmissionRejectsCanceledContext(t *testing.T) {
	admission, err := NewRealtimeStreamAdmission(1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := admission.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire error = %v, want context canceled", err)
	}
	if snapshot := admission.Snapshot(); snapshot.Active != 0 || snapshot.Rejected != 0 {
		t.Fatalf("canceled acquire changed admission: %+v", snapshot)
	}
}
