package engine

import (
	"context"
	"errors"
	"testing"
)

func TestUploadAdmissionBoundsConcurrentCountAndReservedBytes(t *testing.T) {
	admission, err := NewUploadAdmission(2, 100)
	if err != nil {
		t.Fatal(err)
	}
	releaseFirst, err := admission.Acquire(context.Background(), 60)
	if err != nil {
		t.Fatal("first reservation:", err)
	}
	if _, err := admission.Acquire(context.Background(), 50); !errors.Is(err, ErrUploadCapacity) {
		t.Fatalf("byte-over-budget acquire error = %v, want capacity exhausted", err)
	}
	releaseSecond, err := admission.Acquire(context.Background(), 40)
	if err != nil {
		t.Fatal("reservation within remaining bytes:", err)
	}
	if _, err := admission.Acquire(context.Background(), 1); !errors.Is(err, ErrUploadCapacity) {
		t.Fatalf("count-over-budget acquire error = %v, want capacity exhausted", err)
	}
	releaseFirst()
	releaseFirst()
	releaseSecond()
	snapshot := admission.Snapshot()
	if snapshot.Active != 0 || snapshot.ActiveBytes != 0 || snapshot.Capacity != 2 || snapshot.MaxBytes != 100 || snapshot.Rejected != 2 {
		t.Fatalf("upload admission snapshot = %+v", snapshot)
	}
}

func TestUploadAdmissionRejectsOversizedAndCanceledReservations(t *testing.T) {
	admission, err := NewUploadAdmission(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Acquire(context.Background(), 101); !errors.Is(err, ErrUploadReservationTooBig) {
		t.Fatalf("oversized acquire error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := admission.Acquire(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v", err)
	}
	if snapshot := admission.Snapshot(); snapshot.Active != 0 || snapshot.ActiveBytes != 0 || snapshot.Rejected != 1 {
		t.Fatalf("invalid reservations changed budget: %+v", snapshot)
	}
}
