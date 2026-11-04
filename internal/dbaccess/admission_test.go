package dbaccess

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAdmissionBoundsActiveUnitsAndWaiters(t *testing.T) {
	admission, err := NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	failure := make(chan error, 1)
	go func() {
		release, err := admission.Acquire(context.Background())
		if err != nil {
			failure <- err
			return
		}
		acquired <- release
	}()
	deadline := time.Now().Add(time.Second)
	for admission.Snapshot().Waiters != 1 {
		if time.Now().After(deadline) {
			t.Fatal("caller did not enter bounded active-work queue")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := admission.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("over-capacity waiter error=%v, want ErrExhausted", err)
	}
	first()
	select {
	case release := <-acquired:
		release()
	case err := <-failure:
		t.Fatalf("queued work failed after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("queued work did not acquire released slot")
	}
	if got := admission.Snapshot(); got.Active != 0 || got.Waiters != 0 || got.WaitCount != 1 || got.Rejected != 1 || got.WaitDuration <= 0 {
		t.Fatalf("unexpected admission snapshot: %#v", got)
	}
}

func TestAdmissionHonorsDeadlineAndCancellation(t *testing.T) {
	admission, err := NewAdmission(1, 2, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	release, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("timed out work error=%v, want ErrExhausted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); time.Sleep(time.Millisecond); cancel() }()
	if _, err := admission.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled work error=%v, want context.Canceled", err)
	}
	wg.Wait()
	release()
	if got := admission.Snapshot(); got.Active != 0 || got.Waiters != 0 || got.WaitCount != 2 || got.Rejected != 2 {
		t.Fatalf("waiters were not retired after timeout/cancellation: %#v", got)
	}
}

func TestAdmissionReleasesPermitWhenContextCancelsDuringAcquire(t *testing.T) {
	admission, err := NewAdmission(1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Acquire(newCancelOnSecondErrContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire error = %v, want cancellation", err)
	}
	if got := admission.Snapshot(); got.Active != 0 || got.Rejected != 1 {
		t.Fatalf("canceled acquisition leaked active-work permit: %#v", got)
	}
}

func TestMaintenanceLaneReservesInteractiveCapacityAndCapsHeavyWork(t *testing.T) {
	admission, err := NewAdmissionWithMaintenance(3, 2, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	heavy, err := admission.AcquireMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.AcquireMaintenance(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("second maintenance admission error=%v, want bounded rejection", err)
	}
	first, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admission.Acquire(context.Background()); !errors.Is(err, ErrExhausted) {
		t.Fatalf("ordinary admission consumed reserved maintenance capacity: %v", err)
	}
	snapshot := admission.Snapshot()
	if snapshot.Active != 3 || snapshot.MaintenanceActive != 1 || snapshot.MaintenanceCapacity != 1 {
		t.Fatalf("unexpected lane snapshot: %#v", snapshot)
	}
	second()
	first()
	heavy()
	if got := admission.Snapshot(); got.Active != 0 || got.MaintenanceActive != 0 {
		t.Fatalf("maintenance or ordinary permit leaked: %#v", got)
	}
}
