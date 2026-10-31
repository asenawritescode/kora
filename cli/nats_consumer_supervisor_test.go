package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/engine"
	"github.com/asenawritescode/kora/natsprovider"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestNATSConsumerSupervisorRetriesAndStopsWithGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	started := time.Now()
	engine.RunNATSConsumerWithBackoff(ctx, "tenant-a", "sideeffects", func(ctx context.Context) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected transient settlement failure")
		}
		cancel()
		return ctx.Err()
	})
	if attempts != 2 {
		t.Fatalf("consumer attempts = %d, want retry after first failure", attempts)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("retry wait elapsed %s, want bounded initial backoff near 250ms", elapsed)
	}
}

func TestNATSConsumerSupervisorCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		engine.RunNATSConsumerWithBackoff(ctx, "tenant-a", "rebuilds", func(context.Context) error {
			called <- struct{}{}
			return errors.New("injected transport failure")
		})
	}()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("consumer was not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("runtime cancellation did not interrupt retry backoff")
	}
}

func TestNATSConsumerSupervisorRecreatesDurableAndRedelivers(t *testing.T) {
	options := &server.Options{JetStream: true, JetStreamMaxMemory: 2 << 30, JetStreamMaxStore: 2 << 30, Port: -1, StoreDir: t.TempDir()}
	broker, err := server.NewServer(options)
	if err != nil {
		t.Fatal("create embedded JetStream:", err)
	}
	go broker.Start()
	if !broker.ReadyForConnections(10 * time.Second) {
		t.Fatal("embedded JetStream did not become ready")
	}
	defer broker.Shutdown()
	brokerURL := fmt.Sprintf("nats://%s", broker.Addr().String())
	provider, err := natsprovider.New(context.Background(), natsprovider.Config{
		Name: "kora-supervisor-test", ServerURLs: []string{brokerURL}, StreamName: "KORA_SUPERVISOR_EVENTS",
		SubjectPrefix: "kora_supervisor", ConsumerName: "tenant-a-sideeffects", AckWait: time.Second, MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("create NATS provider:", err)
	}
	defer provider.Close()
	if err := provider.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap JetStream:", err)
	}
	consumer, err := natsprovider.NewConsumer(provider, provider.Config())
	if err != nil {
		t.Fatal("create durable consumer:", err)
	}
	adminConn, err := nats.Connect(brokerURL)
	if err != nil {
		t.Fatal("create JetStream admin connection:", err)
	}
	defer adminConn.Close()
	adminJS, err := adminConn.JetStream()
	if err != nil {
		t.Fatal("create JetStream admin context:", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDelivery := make(chan contract.Delivery, 1)
	redelivered := make(chan contract.Delivery, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	runDone := make(chan struct{})
	defer func() {
		cancel()
		release()
	}()
	go func() {
		defer close(runDone)
		calls := 0
		engine.RunNATSConsumerWithBackoff(runCtx, "tenant-a", "tenant-a-sideeffects", func(ctx context.Context) error {
			return consumer.Run(ctx, func(_ context.Context, delivery contract.Delivery) error {
				calls++
				if calls == 1 {
					firstDelivery <- delivery
					<-releaseFirst
					return nil
				}
				redelivered <- delivery
				return nil
			})
		})
	}()
	eventID := contract.NewEventID()
	if err := provider.Publish(context.Background(), contract.EventEnvelope{
		ID: eventID, Type: "events.record.updated", Version: 1, Source: "test", Site: "tenant-a", Data: []byte(`{}`),
	}); err != nil {
		t.Fatal("publish durable source event:", err)
	}
	select {
	case delivery := <-firstDelivery:
		if delivery.ID != eventID {
			t.Fatalf("initial delivery ID=%q, want %q", delivery.ID, eventID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("consumer did not receive initial event")
	}
	if err := adminJS.DeleteConsumer("KORA_SUPERVISOR_EVENTS", "tenant-a-sideeffects"); err != nil {
		t.Fatal("delete active durable to force settlement failure:", err)
	}
	release()
	select {
	case delivery := <-redelivered:
		if delivery.ID != eventID {
			t.Fatalf("redelivered ID=%q, want original %q", delivery.ID, eventID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not recreate the durable and redeliver the unsettled event")
	}
	ackDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(ackDeadline) {
		info, err := adminJS.ConsumerInfo("KORA_SUPERVISOR_EVENTS", "tenant-a-sideeffects")
		if err != nil {
			t.Fatal("inspect redelivery acknowledgment:", err)
		}
		if info.NumAckPending == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := adminJS.ConsumerInfo("KORA_SUPERVISOR_EVENTS", "tenant-a-sideeffects")
	if err != nil {
		t.Fatal("inspect recreated durable:", err)
	}
	if info.NumAckPending != 0 {
		t.Fatalf("recreated durable has %d pending acknowledgments after successful redelivery", info.NumAckPending)
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(3 * time.Second):
		t.Fatal("supervised consumer did not stop after generation cancellation")
	}
}
