package tenantruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestGenerationRetiresHandlesAndWorkersTogether(t *testing.T) {
	generation := NewGeneration(12, context.Background())
	handle, ok := generation.Acquire()
	if !ok || handle.Generation() != 12 {
		t.Fatal("active generation did not return a handle with its identity")
	}
	workerStarted := make(chan struct{})
	workerExited := make(chan struct{})
	if !generation.StartWorker(func(ctx context.Context) {
		close(workerStarted)
		<-ctx.Done()
		close(workerExited)
	}) {
		t.Fatal("active generation did not admit worker")
	}
	<-workerStarted
	generation.Retire()
	if _, ok := generation.Acquire(); ok {
		t.Fatal("retired generation admitted a new request handle")
	}
	if generation.StartWorker(func(context.Context) {}) {
		t.Fatal("retired generation admitted a new worker")
	}
	select {
	case <-generation.Drained():
		t.Fatal("generation drained while a request handle remained held")
	default:
	}
	generation.StopWorkers()
	select {
	case <-workerExited:
	default:
		t.Fatal("worker join returned before the generation worker exited")
	}
	handle.Release()
	handle.Release()
	if err := generation.Wait(context.Background()); err != nil {
		t.Fatal("wait for retired generation:", err)
	}
}

func TestWorkersStopContextTimeoutCanBeRetried(t *testing.T) {
	workers := NewWorkers(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	if !workers.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release
	}) {
		t.Fatal("worker was not registered")
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := workers.StopContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopContext error = %v, want deadline", err)
	}
	cancel()
	close(release)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := workers.StopContext(ctx); err != nil {
		t.Fatalf("retry StopContext: %v", err)
	}
}

func TestWorkersCancelAndJoin(t *testing.T) {
	workers := NewWorkers(context.Background())
	started := make(chan struct{})
	exited := make(chan struct{})
	if !workers.Go(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(exited)
	}) {
		t.Fatal("worker was not registered")
	}
	<-started
	workers.Stop()
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the worker exited")
	}
	if workers.Go(func(context.Context) {}) {
		t.Fatal("worker was accepted after Stop")
	}
}

func TestConcurrentStopCallersJoinTheSameGeneration(t *testing.T) {
	workers := NewWorkers(context.Background())
	enteredCancellation := make(chan struct{})
	release := make(chan struct{})
	if !workers.Go(func(ctx context.Context) {
		<-ctx.Done()
		close(enteredCancellation)
		<-release
	}) {
		t.Fatal("worker was not registered")
	}
	stopped := make(chan struct{}, 2)
	for range 2 {
		go func() {
			workers.Stop()
			stopped <- struct{}{}
		}()
	}
	<-enteredCancellation
	select {
	case <-stopped:
		t.Fatal("Stop returned while the generation worker was still running")
	default:
	}
	close(release)
	<-stopped
	<-stopped
}

func TestWorkersConcurrentAdmissionAndStop(t *testing.T) {
	workers := NewWorkers(context.Background())
	const count = 64
	var started sync.WaitGroup
	started.Add(count)
	for range count {
		if !workers.Go(func(ctx context.Context) {
			started.Done()
			<-ctx.Done()
		}) {
			t.Fatal("worker admission failed before stop")
		}
	}
	started.Wait()
	workers.Stop()
}
