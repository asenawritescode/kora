package dbaccess

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundLaneSizing(t *testing.T) {
	tests := []struct {
		name            string
		capacity        int
		maintenance     int
		wantBackground  int
		wantOrdinary    int
		wantMaintenance int
	}{
		{name: "sixteen with no maintenance", capacity: 16, wantBackground: 4, wantOrdinary: 12},
		{name: "sixteen with one maintenance", capacity: 16, maintenance: 1, wantBackground: 4, wantOrdinary: 11, wantMaintenance: 1},
		{name: "twelve", capacity: 12, wantBackground: 3, wantOrdinary: 9},
		{name: "eight floors at two", capacity: 8, wantBackground: 2, wantOrdinary: 6},
		{name: "two keeps one ordinary", capacity: 2, wantBackground: 1, wantOrdinary: 1},
		{name: "one has no spare slot", capacity: 1, wantBackground: 0, wantOrdinary: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewAdmissionWithMaintenance(tt.capacity, 8, time.Second, tt.maintenance)
			if err != nil {
				t.Fatal(err)
			}
			snap := a.Snapshot()
			if snap.BackgroundCapacity != tt.wantBackground || cap(a.ordinary) != tt.wantOrdinary || snap.MaintenanceCapacity != tt.wantMaintenance {
				t.Fatalf("lanes background=%d ordinary=%d maintenance=%d, want %d/%d/%d",
					snap.BackgroundCapacity, cap(a.ordinary), snap.MaintenanceCapacity, tt.wantBackground, tt.wantOrdinary, tt.wantMaintenance)
			}
			if got := cap(a.ordinary) + snap.BackgroundCapacity + snap.MaintenanceCapacity; got != tt.capacity {
				t.Fatalf("lanes sum to %d, want capacity %d", got, tt.capacity)
			}
		})
	}
}

func TestSaturatedBackgroundLaneDoesNotBlockRequests(t *testing.T) {
	a, err := NewAdmission(16, 8, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for range backgroundLaneCapacity(16, 0) {
		release, err := a.Background().Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.Background().Acquire(ctx); !IsBackgroundBusy(err) {
		t.Fatalf("saturated background acquire error = %v, want busy", err)
	}
	var requests []func()
	defer func() {
		for _, release := range requests {
			release()
		}
	}()
	for range cap(a.ordinary) {
		release, err := a.Acquire(context.Background())
		if err != nil {
			t.Fatalf("request admission blocked by background lane: %v", err)
		}
		requests = append(requests, release)
	}
	full, cancelFull := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelFull()
	if _, err := a.Acquire(full); err == nil {
		t.Fatal("request lane accepted more than its capacity")
	}
}

func TestBackgroundBusyIsSkipNotFailure(t *testing.T) {
	tests := []struct {
		name     string
		deadline time.Duration
		wantCtx  bool
	}{
		{name: "bounded by caller deadline", deadline: 15 * time.Millisecond, wantCtx: true},
		{name: "unbounded falls back to admission wait", deadline: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewAdmission(4, 4, 15*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			for range backgroundLaneCapacity(4, 0) {
				if _, acquireErr := a.Background().Acquire(context.Background()); acquireErr != nil {
					t.Fatal(acquireErr)
				}
			}
			ctx := context.Background()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			_, err = a.Background().Acquire(ctx)
			if !IsBackgroundBusy(err) {
				t.Fatalf("error = %v, want background busy", err)
			}
			if got := errors.Is(err, context.DeadlineExceeded); got != tt.wantCtx {
				t.Fatalf("wraps deadline = %v, want %v", got, tt.wantCtx)
			}
			if got := a.Snapshot().BackgroundSkipped; got != 1 {
				t.Fatalf("skipped = %d, want 1", got)
			}
			if got := a.Snapshot().Rejected; got != 0 {
				t.Fatalf("request rejected counter = %d, want 0 for background skips", got)
			}
		})
	}
}

func TestBackgroundAcquireWaitsForSlotWithinDeadline(t *testing.T) {
	a, err := NewAdmission(4, 4, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Snapshot().BackgroundCapacity; got != 2 {
		t.Fatalf("background capacity = %d, want 2", got)
	}
	first, err := a.Background().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Background().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	time.AfterFunc(20*time.Millisecond, first)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := a.Background().Acquire(ctx)
	if err != nil {
		t.Fatalf("released slot not acquired within deadline: %v", err)
	}
	release()
}

func TestBackgroundWaiterBoundAndNilLane(t *testing.T) {
	a, err := NewAdmission(4, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var held []func()
	defer func() {
		for _, release := range held {
			release()
		}
	}()
	for range a.Snapshot().BackgroundCapacity {
		release, acquireErr := a.Background().Acquire(context.Background())
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		held = append(held, release)
	}
	if _, err := a.Background().Acquire(context.Background()); !IsBackgroundBusy(err) {
		t.Fatalf("waiter over bound error = %v, want busy", err)
	}
	var nilLane *BackgroundLane
	if _, err := nilLane.Acquire(context.Background()); err == nil {
		t.Fatal("nil background lane acquired a slot")
	}
	var nilAdmission *Admission
	if nilAdmission.Background() != nil {
		t.Fatal("nil admission returned a background lane")
	}
}
