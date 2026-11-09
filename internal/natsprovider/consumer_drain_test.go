package natsprovider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDrainSubscriptionForceUnsubscribesAndJoinsAfterDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	started := make(chan struct{})
	unsubscribed := make(chan struct{})
	drainReturned := make(chan struct{})
	var once sync.Once
	err := drainSubscription(ctx, func() error {
		close(started)
		<-unsubscribed
		close(drainReturned)
		return nil
	}, func() error {
		once.Do(func() { close(unsubscribed) })
		return nil
	})
	if err != nil {
		t.Fatalf("drainSubscription: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("drain did not start")
	}
	select {
	case <-drainReturned:
	default:
		t.Fatal("drainSubscription returned before the drain goroutine exited")
	}
}

func TestDrainSubscriptionReturnsDrainFailure(t *testing.T) {
	want := errors.New("drain failed")
	err := drainSubscription(context.Background(), func() error { return want }, func() error { return nil })
	if !errors.Is(err, want) {
		t.Fatalf("drainSubscription error = %v, want %v", err, want)
	}
}

func TestDrainSubscriptionRequiresContext(t *testing.T) {
	if err := drainSubscription(nil, func() error { return nil }, func() error { return nil }); err == nil {
		t.Fatal("drainSubscription without context succeeded")
	}
}
