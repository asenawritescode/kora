package analytics

import (
	"sync"
	"testing"
)

func TestAnalyticsQueueAdmissionBoundsConcurrentReservations(t *testing.T) {
	budget, err := NewAnalyticsQueueAdmission(1024)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var accepted int
	var mu sync.Mutex
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if budget.TryAcquire(64) {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if accepted != 16 {
		t.Fatalf("accepted reservations = %d, want 16", accepted)
	}
	if snapshot := budget.Snapshot(); snapshot.UsedBytes != 1024 || snapshot.MaxBytes != 1024 || snapshot.RejectedItems != 16 {
		t.Fatalf("budget snapshot = %+v", snapshot)
	}
	budget.Release(512)
	if !budget.TryAcquire(512) {
		t.Fatal("released bytes were not reusable")
	}
	if snapshot := budget.Snapshot(); snapshot.UsedBytes != 1024 || snapshot.RejectedItems != 16 {
		t.Fatalf("budget snapshot after release/reacquire = %+v", snapshot)
	}
}

func TestAnalyticsQueueAdmissionRejectsInvalidCapacity(t *testing.T) {
	for _, capacity := range []uint64{0, MaxAnalyticsQueueAdmissionBytes + 1} {
		if _, err := NewAnalyticsQueueAdmission(capacity); err == nil {
			t.Fatalf("capacity %d unexpectedly accepted", capacity)
		}
	}
	for _, capacity := range []int{0, -1, MaxAnalyticsProcessListeners + 1} {
		if _, err := NewAnalyticsQueueAdmissionWithLimits(1024, capacity); err == nil {
			t.Fatalf("process listener capacity %d unexpectedly accepted", capacity)
		}
	}
}
