package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/dbaccess"
)

type humanWaitRecoveryTestStore struct {
	calls    atomic.Int32
	expiries atomic.Int32
	timers   atomic.Int32
	batch    atomic.Uint32
	started  chan struct{}
	err      error
}

func (s *humanWaitRecoveryTestStore) ExpireDue(ctx context.Context, tenant contract.TenantID, batch uint16) (uint16, error) {
	if tenant != "tenant-a" {
		return 0, errors.New("unexpected tenant")
	}
	s.batch.Store(uint32(batch))
	s.expiries.Add(1)
	return 0, s.err
}

func (s *humanWaitRecoveryTestStore) ResumeTerminalHumanWaits(ctx context.Context, tenant contract.TenantID, batch uint16) (uint16, error) {
	if tenant != "tenant-a" {
		return 0, errors.New("unexpected tenant")
	}
	s.batch.Store(uint32(batch))
	s.calls.Add(1)
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return 0, ctx.Err()
}

func (s *humanWaitRecoveryTestStore) ResumeDueTimedWaits(ctx context.Context, tenant contract.TenantID, batch uint16) (uint16, error) {
	if tenant != "tenant-a" {
		return 0, errors.New("unexpected tenant")
	}
	s.batch.Store(uint32(batch))
	s.timers.Add(1)
	return 0, nil
}

func TestHumanWaitRecoveryWorkerRunsBoundedPassAndReturnsOnCancel(t *testing.T) {
	store := &humanWaitRecoveryTestStore{started: make(chan struct{}, 1)}
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal("construct admission:", err)
	}
	worker, err := NewHumanWaitRecoveryWorker(store, "tenant-a", 12, time.Hour, nil, admission)
	if err != nil {
		t.Fatal("construct recovery worker:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx)
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not make its initial recovery pass")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not join after cancellation")
	}
	if got := store.batch.Load(); got != 12 {
		t.Fatalf("recovery batch = %d, want 12", got)
	}
	if got := store.expiries.Load(); got != 1 {
		t.Fatalf("expiry calls = %d, want one immediate pass", got)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("recovery calls = %d, want one immediate pass", got)
	}
	if got := store.timers.Load(); got != 1 {
		t.Fatalf("timed wait recovery calls = %d, want one immediate pass", got)
	}
	if got := admission.Snapshot().Active; got != 0 {
		t.Fatalf("admission permits left active = %d, want 0", got)
	}
}

func TestHumanWaitRecoveryWorkerSkipsStoreWhenAdmissionRejected(t *testing.T) {
	store := &humanWaitRecoveryTestStore{}
	admission, err := dbaccess.NewAdmission(1, 0, time.Second)
	if err != nil {
		t.Fatal("construct admission:", err)
	}
	blockRelease, err := admission.Acquire(context.Background())
	if err != nil {
		t.Fatal("occupy admission:", err)
	}
	worker, err := NewHumanWaitRecoveryWorker(store, "tenant-a", 12, time.Hour, nil, admission)
	if err != nil {
		t.Fatal("construct recovery worker:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	deadline := time.After(time.Second)
	for admission.Snapshot().Rejected == 0 {
		select {
		case <-deadline:
			t.Fatal("worker did not attempt admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	blockRelease()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	if store.expiries.Load() != 0 || store.calls.Load() != 0 {
		t.Fatalf("store called after admission rejection: expiry=%d recovery=%d", store.expiries.Load(), store.calls.Load())
	}
}

func TestHumanWaitRecoveryWorkerRejectsUnboundedConfiguration(t *testing.T) {
	store := &humanWaitRecoveryTestStore{}
	for _, test := range []struct {
		batch    uint16
		interval time.Duration
	}{
		{batch: 0, interval: time.Second},
		{batch: maxHumanWaitRecoveryBatch + 1, interval: time.Second},
		{batch: 1, interval: time.Millisecond},
		{batch: 1, interval: 2 * time.Hour},
	} {
		if _, err := NewHumanWaitRecoveryWorker(store, "tenant-a", test.batch, test.interval, nil); err == nil {
			t.Fatalf("accepted batch=%d interval=%s", test.batch, test.interval)
		}
	}
}
