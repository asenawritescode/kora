package natsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func runEmbeddedServer(t *testing.T) (*server.Server, string) {
	return runEmbeddedServerWithStoreDir(t, t.TempDir())
}

func runEmbeddedServerWithStoreDir(t *testing.T, storeDir string) (*server.Server, string) {
	t.Helper()
	// Explicit limits keep embedded JetStream tests deterministic on hosts whose
	// free disk/RAM changes while another suite runs.
	opts := &server.Options{JetStream: true, JetStreamMaxMemory: 2 << 30, JetStreamMaxStore: 2 << 30, StoreDir: storeDir, Port: -1, MaxPayload: 4 << 20}
	s, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("server not ready")
	}
	return s, fmt.Sprintf("nats://%s", s.Addr().String())
}

func TestBootstrapIsIdempotentAndRequestReplyWorks(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:          "kora-test",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		MaxDeliver:    5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()

	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap 1: %v", err)
	}
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap 2: %v", err)
	}
	streamInfo, err := p.js.StreamInfo(p.cfg.StreamName)
	if err != nil {
		t.Fatal("stream info:", err)
	}
	if streamInfo.Config.Storage != nats.FileStorage || streamInfo.Config.MaxBytes != DefaultEventStreamMaxBytes || streamInfo.Config.MaxMsgs != DefaultEventStreamMaxMessages || streamInfo.Config.Discard != nats.DiscardNew {
		t.Fatalf("default event stream storage/limits = %v/%d bytes, %d messages, discard %v", streamInfo.Config.Storage, streamInfo.Config.MaxBytes, streamInfo.Config.MaxMsgs, streamInfo.Config.Discard)
	}
	diagnostics, err := p.Diagnostics(context.Background())
	if err != nil {
		t.Fatal("read event stream diagnostics:", err)
	}
	if diagnostics.StreamStorage != fmt.Sprintf("%v", nats.FileStorage) {
		t.Fatalf("new stream diagnostic storage = %q, want %q", diagnostics.StreamStorage, nats.FileStorage)
	}

	if err := p.Publish(context.Background(), contract.EventEnvelope{
		ID:   "evt-1",
		Type: "events.document.created",
		Site: "site-a",
		Data: json.RawMessage(`{"data":{"name":"Test"}}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestFileBackedEventStreamSurvivesServerRestart(t *testing.T) {
	storeDir := t.TempDir()
	server1, url1 := runEmbeddedServerWithStoreDir(t, storeDir)
	provider1, err := New(context.Background(), Config{
		Name: "kora-restart-test", ServerURLs: []string{url1}, StreamName: "KORA_RESTART_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("create first provider:", err)
	}
	if err := provider1.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap first provider:", err)
	}
	event := contract.EventEnvelope{ID: "restart-event", Type: "events.restart", Site: "tenant-a", Data: json.RawMessage(`{"ok":true}`)}
	if err := provider1.Publish(context.Background(), event); err != nil {
		t.Fatal("publish before restart:", err)
	}
	provider1.Close()
	server1.Shutdown()
	server1.WaitForShutdown()

	server2, url2 := runEmbeddedServerWithStoreDir(t, storeDir)
	defer server2.Shutdown()
	provider2, err := New(context.Background(), Config{
		Name: "kora-restart-test", ServerURLs: []string{url2}, StreamName: "KORA_RESTART_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("create restarted provider:", err)
	}
	defer provider2.Close()
	if err := provider2.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap restarted provider:", err)
	}
	message, err := provider2.js.GetLastMsg(provider2.cfg.StreamName, "kora.events.restart")
	if err != nil {
		t.Fatal("read message after broker restart:", err)
	}
	var got contract.EventEnvelope
	if err := json.Unmarshal(message.Data, &got); err != nil {
		t.Fatal("decode message after broker restart:", err)
	}
	if got.ID != event.ID || got.Site != event.Site {
		t.Fatalf("recovered event id/site = %q/%q, want %q/%q", got.ID, got.Site, event.ID, event.Site)
	}
}

func TestBootstrapPreservesLegacyMemoryStorageAndReportsIt(t *testing.T) {
	server, url := runEmbeddedServer(t)
	defer server.Shutdown()
	provider, err := New(context.Background(), Config{
		Name: "kora-memory-compat-test", ServerURLs: []string{url}, StreamName: "KORA_MEMORY_COMPAT_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("create provider:", err)
	}
	defer provider.Close()
	if _, err := provider.js.AddStream(&nats.StreamConfig{
		Name: provider.cfg.StreamName, Subjects: []string{"kora.>"}, Retention: nats.LimitsPolicy,
		Storage: nats.MemoryStorage, Discard: nats.DiscardNew, MaxBytes: provider.cfg.StreamMaxBytes,
		MaxMsgs: int64(provider.cfg.StreamMaxMessages),
	}, nats.Context(context.Background())); err != nil {
		t.Fatal("create legacy memory stream:", err)
	}
	if err := provider.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap legacy memory stream:", err)
	}
	info, err := provider.js.StreamInfo(provider.cfg.StreamName)
	if err != nil {
		t.Fatal("read legacy stream:", err)
	}
	if info.Config.Storage != nats.MemoryStorage {
		t.Fatalf("legacy stream storage changed to %v, want memory compatibility", info.Config.Storage)
	}
	diagnostics, err := provider.Diagnostics(context.Background())
	if err != nil {
		t.Fatal("read provider diagnostics:", err)
	}
	if diagnostics.StreamStorage != fmt.Sprintf("%v", nats.MemoryStorage) {
		t.Fatalf("diagnostic stream storage = %q, want %q", diagnostics.StreamStorage, nats.MemoryStorage)
	}
	event := contract.EventEnvelope{ID: "must-remain-in-outbox", Type: "events.migration", Site: "tenant-a", Data: json.RawMessage(`{"value":1}`)}
	if err := provider.Publish(context.Background(), event); !errors.Is(err, ErrNonDurableEventStream) {
		t.Fatalf("publish to legacy memory stream error = %v, want ErrNonDurableEventStream", err)
	}
	info, err = provider.js.StreamInfo(provider.cfg.StreamName)
	if err != nil {
		t.Fatal("recheck legacy stream after rejected publish:", err)
	}
	if info.State.Msgs != 0 {
		t.Fatalf("legacy stream accepted %d events after durable publish was rejected", info.State.Msgs)
	}
}

func TestBootstrapBoundsEventStreamAndRejectsOverflow(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	ctx := context.Background()
	p, err := New(ctx, Config{
		Name: "kora-bounded-stream-test", ServerURLs: []string{url}, StreamName: "KORA_BOUNDED_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: 4096, StreamMaxMessages: 2,
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	// Simulate an older deployment that created this stream without limits and
	// with discard-old behavior. Bootstrap upgrades its config without deleting
	// existing messages.
	if _, err := p.js.AddStream(&nats.StreamConfig{
		Name: p.cfg.StreamName, Subjects: []string{"kora.>"},
		Retention: nats.LimitsPolicy, Storage: nats.MemoryStorage, Discard: nats.DiscardOld, MaxConsumers: 17,
	}, nats.Context(ctx)); err != nil {
		t.Fatal("create legacy stream:", err)
	}
	if err := p.Bootstrap(ctx); err != nil {
		t.Fatal("upgrade stream limits:", err)
	}
	if err := p.PublishSubject(ctx, "kora.test", []byte("existing"), "existing"); err != nil {
		t.Fatal("publish existing event:", err)
	}
	if err := p.Bootstrap(ctx); err != nil {
		t.Fatal("repeat bootstrap:", err)
	}
	for i := 0; i < 5; i++ {
		if err := p.PublishSubject(ctx, "kora.test", []byte(strings.Repeat("x", 512)), fmt.Sprintf("bounded-%d", i)); err != nil {
			break // DiscardNew must reject rather than evict an accepted event.
		}
		if i == 4 {
			t.Fatal("bounded stream accepted every message beyond its configured cap")
		}
	}
	info, err := p.js.StreamInfo(p.cfg.StreamName, nats.Context(ctx))
	if err != nil {
		t.Fatal("read bounded stream:", err)
	}
	if info.Config.MaxBytes != 4096 || info.Config.MaxMsgs != 2 || info.Config.Discard != nats.DiscardNew || info.Config.MaxConsumers != 17 {
		t.Fatalf("stream config = max bytes %d, max messages %d, discard %v, max consumers %d", info.Config.MaxBytes, info.Config.MaxMsgs, info.Config.Discard, info.Config.MaxConsumers)
	}
	if info.State.Msgs != 2 || info.State.Bytes > uint64(info.Config.MaxBytes) {
		t.Fatalf("stream state = %d messages / %d bytes; expected retained messages within %d bytes", info.State.Msgs, info.State.Bytes, info.Config.MaxBytes)
	}
	diagnostics, err := p.Diagnostics(ctx)
	if err != nil {
		t.Fatal("read stream diagnostics:", err)
	}
	if diagnostics.StreamMsgs != info.State.Msgs || diagnostics.StreamBytes != info.State.Bytes || diagnostics.StreamMaxMsgs != uint64(info.Config.MaxMsgs) || diagnostics.StreamMaxBytes != uint64(info.Config.MaxBytes) {
		t.Fatalf("stream diagnostics do not expose occupancy/capacity: %+v", diagnostics)
	}
}

func TestBootstrapRefusesCapsThatWouldEvictExistingEvents(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	legacy, err := New(context.Background(), Config{
		Name: "kora-existing-stream", ServerURLs: []string{url}, StreamName: "KORA_EXISTING_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("new legacy provider:", err)
	}
	defer legacy.Close()
	if _, err := legacy.js.AddStream(&nats.StreamConfig{
		Name: legacy.cfg.StreamName, Subjects: []string{"kora.>"},
		Retention: nats.LimitsPolicy, Storage: nats.MemoryStorage, Discard: nats.DiscardOld,
		MaxBytes: 4096, MaxMsgs: 10,
	}, nats.Context(context.Background())); err != nil {
		t.Fatal("create existing stream:", err)
	}
	if err := legacy.PublishSubject(context.Background(), "kora.test", []byte(strings.Repeat("x", 512)), "retained"); err != nil {
		t.Fatal("publish retained event:", err)
	}
	before, err := legacy.js.StreamInfo(legacy.cfg.StreamName)
	if err != nil {
		t.Fatal("inspect existing stream:", err)
	}
	bounded, err := New(context.Background(), Config{
		Name: "kora-existing-stream-upgrade", ServerURLs: []string{url}, StreamName: legacy.cfg.StreamName,
		SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: 128, StreamMaxMessages: 1,
	})
	if err != nil {
		t.Fatal("new bounded provider:", err)
	}
	defer bounded.Close()
	if err := bounded.Bootstrap(context.Background()); err == nil {
		t.Fatal("bootstrap lowered limits below retained event usage")
	}
	after, err := legacy.js.StreamInfo(legacy.cfg.StreamName)
	if err != nil {
		t.Fatal("inspect stream after rejected migration:", err)
	}
	if after.State.Msgs != before.State.Msgs || after.State.Bytes != before.State.Bytes {
		t.Fatalf("rejected limit change modified retained events: before %d/%d, after %d/%d", before.State.Msgs, before.State.Bytes, after.State.Msgs, after.State.Bytes)
	}
}

func TestBootstrapBoundsEventStreamByBytes(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	ctx := context.Background()
	p, err := New(ctx, Config{
		Name: "kora-byte-bounded-stream-test", ServerURLs: []string{url}, StreamName: "KORA_BYTE_BOUNDED_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: 2048, StreamMaxMessages: 100,
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if err := p.Bootstrap(ctx); err != nil {
		t.Fatal("bootstrap:", err)
	}
	accepted := 0
	for i := 0; i < 10; i++ {
		if err := p.PublishSubject(ctx, "kora.test", []byte(strings.Repeat("x", 512)), fmt.Sprintf("byte-bound-%d", i)); err != nil {
			break
		}
		accepted++
	}
	if accepted == 0 || accepted == 10 {
		t.Fatalf("accepted %d messages before byte cap, want a nonzero count below 10", accepted)
	}
	info, err := p.js.StreamInfo(p.cfg.StreamName, nats.Context(ctx))
	if err != nil {
		t.Fatal("read stream:", err)
	}
	if info.State.Bytes > uint64(info.Config.MaxBytes) || info.State.Msgs != uint64(accepted) {
		t.Fatalf("stream bytes/messages = %d/%d after %d accepted publishes; caps are %d/%d", info.State.Bytes, info.State.Msgs, accepted, info.Config.MaxBytes, info.Config.MaxMsgs)
	}
}

func TestSiteDirectoryNotificationsUseDedicatedJetStream(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	prefix := fmt.Sprintf("kora_test_%d", time.Now().UnixNano())
	p, err := New(context.Background(), Config{
		Name: "kora-directory-test", ServerURLs: []string{url},
		StreamName: strings.ToUpper(prefix) + "_EVENTS", SubjectPrefix: prefix, MaxDeliver: 5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap event stream:", err)
	}
	if err := p.BootstrapSiteDirectory(context.Background()); err != nil {
		t.Fatal("bootstrap directory stream:", err)
	}
	if err := p.BootstrapSiteDirectory(context.Background()); err != nil {
		t.Fatal("repeat directory bootstrap:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakeups, err := p.SubscribeSiteDirectoryWakeups(ctx, "engine:replica-1")
	if err != nil {
		t.Fatal("subscribe directory wakeups:", err)
	}
	if err := p.PublishSiteDirectoryRevision(context.Background(), 42); err != nil {
		t.Fatal("publish directory revision:", err)
	}
	select {
	case revision := <-wakeups:
		if revision != 42 {
			t.Fatalf("directory wakeup revision = %d, want 42", revision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for directory wakeup")
	}
	cancel()
	for range wakeups {
	}
	if err := p.PublishSiteDirectoryRevision(context.Background(), 43); err != nil {
		t.Fatal("publish while subscriber is offline:", err)
	}
	resumeCtx, resumeCancel := context.WithCancel(context.Background())
	defer resumeCancel()
	resumed, err := p.SubscribeSiteDirectoryWakeups(resumeCtx, "engine:replica-1")
	if err != nil {
		t.Fatal("resume durable directory subscription:", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case revision := <-resumed:
			if revision == 43 {
				goto resumed
			}
			if revision != 42 {
				t.Fatalf("unexpected resumed directory wakeup revision = %d", revision)
			}
		case <-deadline:
			t.Fatal("durable directory subscription did not resume missed wakeup")
		}
	}
resumed:
	directoryInfo, err := p.js.StreamInfo(p.SiteDirectoryStreamName())
	if err != nil {
		t.Fatal("read directory stream:", err)
	}
	eventInfo, err := p.js.StreamInfo(p.cfg.StreamName)
	if err != nil {
		t.Fatal("read event stream:", err)
	}
	if directoryInfo.State.Msgs != 2 || eventInfo.State.Msgs != 0 || directoryInfo.Config.Storage != nats.FileStorage || eventInfo.Config.Storage != nats.FileStorage || directoryInfo.Config.MaxBytes != DirectoryStreamMaxBytes || directoryInfo.Config.MaxMsgs != DirectoryStreamMaxMessages {
		t.Fatalf("directory/event stream state = messages %d/%d, storage %v/%v; want 2/0 and both file-backed", directoryInfo.State.Msgs, eventInfo.State.Msgs, directoryInfo.Config.Storage, eventInfo.Config.Storage)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("expected validation error")
	}
	legacy := Config{ServerURLs: []string{"nats://127.0.0.1:4222"}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy config without optional stream limits should use defaults: %v", err)
	}
	for _, config := range []Config{
		{ServerURLs: []string{"nats://127.0.0.1:4222"}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: MaximumEventStreamMaxBytes + 1, StreamMaxMessages: 1},
		{ServerURLs: []string{"nats://127.0.0.1:4222"}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: 1, StreamMaxMessages: MaximumEventStreamMaxMessages + 1},
	} {
		if err := config.Validate(); err == nil {
			t.Fatal("accepted out-of-range event stream limits")
		}
	}
	t.Setenv("KORA_NATS_URLS", "nats://127.0.0.1:4222")
	t.Setenv("KORA_NATS_STREAM_MAX_BYTES", "not-a-number")
	if err := FromEnv().Validate(); err == nil {
		t.Fatal("accepted invalid KORA_NATS_STREAM_MAX_BYTES")
	}
}

func TestPublishUsesCanonicalSubject(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:          "kora-test",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		MaxDeliver:    5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	ch := make(chan *nats.Msg, 1)
	_, err = p.nc.ChanSubscribe("kora.events.document.created", ch)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := p.Publish(context.Background(), contract.EventEnvelope{
		ID:      "evt-1",
		Type:    "events.document.created",
		Version: 1,
		Site:    "site-a",
		Data:    json.RawMessage(`{"data":{"name":"Test"}}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for published event")
	}
}

func TestConsumerDeadLettersAfterMaxDeliver(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:              "kora-test",
		ServerURLs:        []string{url},
		StreamName:        "KORA_EVENTS",
		SubjectPrefix:     "kora",
		ConsumerName:      "kora-worker",
		DeadLetterSubject: "kora.deadletter",
		MaxDeliver:        1,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	c, err := NewConsumer(p, p.cfg)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = c.Run(runCtx, func(ctx context.Context, d contract.Delivery) error {
			return fmt.Errorf("boom")
		})
	}()

	dlCh := make(chan *nats.Msg, 1)
	_, err = p.nc.ChanSubscribe("kora.deadletter", dlCh)
	if err != nil {
		t.Fatalf("deadletter subscribe: %v", err)
	}
	if err := p.nc.Publish("kora.events.test", []byte(`{"id":"evt-1"}`)); err != nil {
		t.Fatalf("publish message: %v", err)
	}
	select {
	case msg := <-dlCh:
		if msg == nil {
			t.Fatal("expected deadletter message")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for deadletter message")
	}
	_ = c.Drain(context.Background())
	cancel()
}

func TestConsumerTermFailureAfterDeadLetterLeavesSourceRedeliverable(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	p, err := New(context.Background(), Config{
		Name: "kora-term-failure", ServerURLs: []string{url}, StreamName: "KORA_TERM_FAILURE_EVENTS",
		SubjectPrefix: "kora_term_failure", ConsumerSubject: "kora_term_failure.events.>",
		ConsumerName: "term-failure-worker", AckWait: 50 * time.Millisecond,
		MaxDeliver: 1, DeadLetterSubject: "kora_term_failure.deadletter",
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap:", err)
	}
	consumer, err := NewConsumer(p, p.cfg)
	if err != nil {
		t.Fatal("new consumer:", err)
	}
	// The dead-letter publish is synchronously confirmed before this hook runs.
	// Closing the consumer connection then makes the real Msg.Term call fail.
	consumer.termDelivery = func(msg *nats.Msg) error {
		p.nc.Close()
		return msg.Term()
	}
	observer, err := nats.Connect(url)
	if err != nil {
		t.Fatal("connect dead-letter observer:", err)
	}
	defer observer.Close()
	deadLetters := make(chan *nats.Msg, 1)
	if _, err := observer.ChanSubscribe("kora_term_failure.deadletter", deadLetters); err != nil {
		t.Fatal("subscribe to dead letters:", err)
	}
	if err := observer.FlushTimeout(time.Second); err != nil {
		t.Fatal("flush dead-letter observer subscription:", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(runCtx, func(context.Context, contract.Delivery) error {
			return fmt.Errorf("permanent handler failure")
		})
	}()
	if err := observer.Publish("kora_term_failure.events.test", []byte(`{"id":"term-failure-event"}`)); err != nil {
		t.Fatal("publish source event:", err)
	}
	if err := observer.FlushTimeout(time.Second); err != nil {
		t.Fatal("flush source event:", err)
	}
	select {
	case message := <-deadLetters:
		if message == nil || message.Subject != "kora_term_failure.deadletter" {
			t.Fatalf("dead-letter message = %#v", message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dead-letter publication was not observed")
	}
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "terminate dead-lettered delivery") {
			t.Fatalf("Run error = %v, want surfaced TERM failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not return after TERM failure")
	}

	recoveryProvider, err := New(context.Background(), Config{
		Name: "kora-term-recovery", ServerURLs: []string{url}, StreamName: "KORA_TERM_FAILURE_EVENTS",
		SubjectPrefix: "kora_term_failure", ConsumerSubject: "kora_term_failure.events.>",
		ConsumerName: "term-failure-worker", AckWait: 50 * time.Millisecond,
		MaxDeliver: 1, DeadLetterSubject: "kora_term_failure.deadletter",
	})
	if err != nil {
		t.Fatal("connect recovery provider:", err)
	}
	defer recoveryProvider.Close()
	recovery, err := NewConsumer(recoveryProvider, recoveryProvider.cfg)
	if err != nil {
		t.Fatal("new recovery consumer:", err)
	}
	recoveryCtx, cancelRecovery := context.WithCancel(context.Background())
	defer cancelRecovery()
	recovered := make(chan contract.Delivery, 1)
	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- recovery.Run(recoveryCtx, func(_ context.Context, delivery contract.Delivery) error {
			recovered <- delivery
			return nil
		})
	}()
	select {
	case delivery := <-recovered:
		if delivery.Attempt < 2 {
			t.Fatalf("redelivered source attempt = %d, want at least 2", delivery.Attempt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("source delivery was lost after dead-letter publication and TERM failure")
	}
	cancelRecovery()
	select {
	case err := <-recoveryDone:
		if err != nil {
			t.Fatalf("recovery consumer shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovery consumer did not stop")
	}
	stream, err := recoveryProvider.js.StreamInfo(recoveryProvider.cfg.StreamName)
	if err != nil {
		t.Fatal("read stream after TERM recovery:", err)
	}
	if stream.State.Msgs != 2 {
		t.Fatalf("stream retained %d messages after TERM recovery, want source plus dead letter", stream.State.Msgs)
	}
}

func TestConsumerRetryableDeliveryDoesNotConsumeMaxDeliver(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name: "kora-test", ServerURLs: []string{url}, StreamName: "KORA_EVENTS",
		SubjectPrefix: "kora", ConsumerName: "kora-retry-worker", MaxDeliver: 1,
		DeadLetterSubject: "kora.deadletter",
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	consumer, err := NewConsumer(p, p.cfg)
	if err != nil {
		t.Fatalf("new consumer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveries := make(chan int, 2)
	go func() {
		_ = consumer.Run(ctx, func(_ context.Context, delivery contract.Delivery) error {
			deliveries <- delivery.Attempt
			if delivery.Attempt == 1 {
				return RetryAfter(fmt.Errorf("temporary capacity rejection"), 25*time.Millisecond)
			}
			return nil
		})
	}()
	if err := p.nc.Publish("kora.events.retry", []byte(`{"id":"evt-retry"}`)); err != nil {
		t.Fatalf("publish message: %v", err)
	}
	select {
	case attempt := <-deliveries:
		if attempt != 1 {
			t.Fatalf("first delivery attempt = %d, want 1", attempt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first delivery")
	}
	select {
	case attempt := <-deliveries:
		if attempt < 2 {
			t.Fatalf("retried delivery attempt = %d, want at least 2", attempt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("capacity retry was lost at MaxDeliver")
	}
	_ = consumer.Drain(context.Background())
	cancel()
}

func TestConsumerLeaseHeartbeatPreventsRedeliveryDuringHandler(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	p, err := New(context.Background(), Config{
		Name: "kora-heartbeat-test", ServerURLs: []string{url}, StreamName: "KORA_HEARTBEAT_EVENTS",
		SubjectPrefix: "kora_heartbeat", ConsumerName: "heartbeat-worker", AckWait: 60 * time.Millisecond, MaxDeliver: 3,
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap:", err)
	}
	consumer, err := NewConsumer(p, p.cfg)
	if err != nil {
		t.Fatal("new consumer:", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	handled := make(chan struct{}, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(runCtx, func(context.Context, contract.Delivery) error {
			if calls.Add(1) == 1 {
				time.Sleep(240 * time.Millisecond)
				handled <- struct{}{}
			}
			return nil
		})
	}()
	eventID := "heartbeat-event-1"
	if err := p.Publish(context.Background(), contract.EventEnvelope{ID: eventID, Type: "work.test", Version: 1, Source: "test", Site: "tenant-a", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal("publish delivery:", err)
	}
	select {
	case <-handled:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("consumer stopped after cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler invoked %d times during a lease-extended delivery, want once", got)
	}
}

func TestConsumerShutdownLeavesDeliveryPending(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	p, err := New(context.Background(), Config{
		Name: "kora-shutdown-test", ServerURLs: []string{url}, StreamName: "KORA_SHUTDOWN_EVENTS",
		SubjectPrefix: "kora_shutdown", ConsumerName: "shutdown-worker", AckWait: 5 * time.Second,
		MaxDeliver: 1, DeadLetterSubject: "kora_shutdown.deadletter",
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatal("bootstrap:", err)
	}
	consumer, err := NewConsumer(p, p.cfg)
	if err != nil {
		t.Fatal("new consumer:", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(runCtx, func(ctx context.Context, _ contract.Delivery) error {
			entered <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	if err := p.Publish(context.Background(), contract.EventEnvelope{ID: "shutdown-event-1", Type: "work.test", Version: 1, Source: "test", Site: "tenant-a", Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal("publish delivery:", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not receive delivery")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("consumer shutdown returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
	info, err := p.js.ConsumerInfo(p.cfg.StreamName, durableName(p.cfg.ConsumerName))
	if err != nil {
		t.Fatal("read durable consumer state:", err)
	}
	if info.NumAckPending != 1 {
		t.Fatalf("shutdown left %d pending acknowledgments, want 1", info.NumAckPending)
	}
	stream, err := p.js.StreamInfo(p.cfg.StreamName)
	if err != nil {
		t.Fatal("read stream after shutdown:", err)
	}
	if stream.State.Msgs != 1 {
		t.Fatalf("stream has %d messages after shutdown, want original delivery only", stream.State.Msgs)
	}
}

func TestConsumerReturnsUnsettledAcknowledgmentErrors(t *testing.T) {
	for _, test := range []struct {
		name        string
		handlerErr  error
		maxDeliver  int
		wantMessage string
	}{
		{name: "ack", maxDeliver: 3, wantMessage: "acknowledge delivery"},
		{name: "nak", handlerErr: fmt.Errorf("handler failed"), maxDeliver: 3, wantMessage: "negatively acknowledge delivery"},
		{name: "dead-letter", handlerErr: fmt.Errorf("handler failed"), maxDeliver: 1, wantMessage: "publish dead-letter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, url := runEmbeddedServer(t)
			defer s.Shutdown()
			p, err := New(context.Background(), Config{
				Name: "kora-ack-test", ServerURLs: []string{url}, StreamName: "KORA_ACK_EVENTS",
				SubjectPrefix: "kora_ack", ConsumerName: "ack-worker-" + test.name,
				AckWait: 100 * time.Millisecond, MaxDeliver: test.maxDeliver,
			})
			if err != nil {
				t.Fatal("new provider:", err)
			}
			defer p.Close()
			if err := p.Bootstrap(context.Background()); err != nil {
				t.Fatal("bootstrap:", err)
			}
			consumer, err := NewConsumer(p, p.cfg)
			if err != nil {
				t.Fatal("new consumer:", err)
			}
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			runDone := make(chan error, 1)
			go func() {
				runDone <- consumer.Run(runCtx, func(context.Context, contract.Delivery) error {
					entered <- struct{}{}
					<-release
					return test.handlerErr
				})
			}()
			if err := p.Publish(context.Background(), contract.EventEnvelope{ID: "ack-event-1", Type: "work.test", Version: 1, Source: "test", Site: "tenant-a", Data: json.RawMessage(`{}`)}); err != nil {
				t.Fatal("publish delivery:", err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("handler did not receive delivery")
			}
			p.nc.Close()
			close(release)
			select {
			case err := <-runDone:
				if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
					t.Fatalf("Run error = %v, want an error containing %q", err, test.wantMessage)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Run did not surface the unsettled delivery error")
			}
			recoveryProvider, err := New(context.Background(), Config{
				Name: "kora-ack-recovery-test", ServerURLs: []string{url}, StreamName: "KORA_ACK_EVENTS",
				SubjectPrefix: "kora_ack", ConsumerName: "ack-worker-" + test.name,
				AckWait: 100 * time.Millisecond, MaxDeliver: test.maxDeliver,
			})
			if err != nil {
				t.Fatal("reconnect provider for durable redelivery:", err)
			}
			defer recoveryProvider.Close()
			recoveryConsumer, err := NewConsumer(recoveryProvider, recoveryProvider.cfg)
			if err != nil {
				t.Fatal("create recovery consumer:", err)
			}
			recoveryCtx, cancelRecovery := context.WithCancel(context.Background())
			defer cancelRecovery()
			recovered := make(chan contract.Delivery, 1)
			recoveryDone := make(chan error, 1)
			go func() {
				recoveryDone <- recoveryConsumer.Run(recoveryCtx, func(_ context.Context, delivery contract.Delivery) error {
					recovered <- delivery
					return nil
				})
			}()
			select {
			case delivery := <-recovered:
				if delivery.Attempt < 2 {
					t.Fatalf("recovered delivery attempt=%d, want at least 2", delivery.Attempt)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("unsettled delivery was not redelivered to a reconnecting consumer")
			}
			cancelRecovery()
			select {
			case err := <-recoveryDone:
				if err != nil {
					t.Fatalf("recovery consumer shutdown: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("recovery consumer did not stop")
			}
		})
	}
}

func TestDiagnosticsReportsStreamHealth(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:          "kora-test",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		MaxDeliver:    5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	diag, err := p.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !diag.Connected {
		t.Fatal("expected provider to report connected")
	}
	if diag.StreamName != "KORA_EVENTS" {
		t.Fatalf("stream name = %q, want KORA_EVENTS", diag.StreamName)
	}
	if diag.ConsumerCount < 0 {
		t.Fatalf("consumer count = %d, want non-negative", diag.ConsumerCount)
	}
}

func TestPublishDeduplicatesByMessageID(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:          "kora-test",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		MaxDeliver:    5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	event := contract.EventEnvelope{
		ID:   "evt-dedupe-1",
		Type: "events.document.created",
		Site: "site-a",
		Data: json.RawMessage(`{"data":{"name":"Test"}}`),
	}
	if err := p.Publish(context.Background(), event); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if err := p.Publish(context.Background(), event); err != nil {
		t.Fatalf("duplicate publish: %v", err)
	}

	info, err := p.js.StreamInfo(p.cfg.StreamName)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Fatalf("stream msgs = %d, want 1", info.State.Msgs)
	}
}

func TestSubscribeDrainClosesChannel(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()

	p, err := New(context.Background(), Config{
		Name:          "kora-test",
		ServerURLs:    []string{url},
		StreamName:    "KORA_EVENTS",
		SubjectPrefix: "kora",
		MaxDeliver:    5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, drain, err := p.Subscribe(ctx, "kora.events.test")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	active := p.RealtimeBudget()
	if active.Subscriptions != 1 || active.ApplicationCapacityItems != realtimeSubscriptionQueueItems || active.ApplicationCapacityBytes != realtimeSubscriptionQueueItems*realtimeSubscriptionMaxMessage {
		t.Fatalf("active realtime budget = %+v", active)
	}
	if err := p.nc.Publish("kora.events.test", []byte("event")); err != nil {
		t.Fatalf("publish realtime queue fixture: %v", err)
	}
	if err := p.nc.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush realtime queue fixture: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for active.ApplicationPendingItems == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		active = p.RealtimeBudget()
	}
	if active.ApplicationPendingItems != 1 || active.ApplicationPendingBytes != realtimeSubscriptionMaxMessage || active.ApplicationOldestAge <= 0 {
		t.Fatalf("queued realtime budget = %+v", active)
	}
	cancel()
	drain()
	if stopped := p.RealtimeBudget(); stopped.Subscriptions != 0 || stopped.ApplicationCapacityItems != 0 {
		t.Fatalf("realtime budget after drain = %+v", stopped)
	}

	queued := 0
	closed := make(chan struct{})
	go func() {
		for range ch {
			queued++
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for drained channel to close")
	}
	if queued != 1 {
		t.Fatalf("drained application queue items = %d, want 1", queued)
	}
}

func TestClientPendingObservedSpanTracksConsecutiveSamples(t *testing.T) {
	start := time.Unix(100, 0).UTC()
	since := time.Time{}
	if span := updatePendingNonemptySpan(&since, 3, start); span != 0 || !since.Equal(start) {
		t.Fatalf("first pending observation = span %s, since %s; want zero span at sample time", span, since)
	}
	if span := updatePendingNonemptySpan(&since, 2, start.Add(250*time.Millisecond)); span != 250*time.Millisecond {
		t.Fatalf("consecutive pending observation span = %s, want 250ms", span)
	}
	if span := updatePendingNonemptySpan(&since, 0, start.Add(300*time.Millisecond)); span != 0 || !since.IsZero() {
		t.Fatalf("empty pending sample = span %s, since %s; want cleared state", span, since)
	}
	if span := updatePendingNonemptySpan(&since, 1, start.Add(time.Second)); span != 0 || !since.Equal(start.Add(time.Second)) {
		t.Fatalf("new pending episode = span %s, since %s; want fresh observation", span, since)
	}
}

func TestSubscribeClosesRealtimeChannelWhenBoundedQueueOverflows(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	p, err := New(context.Background(), Config{
		Name: "kora-slow-realtime-test", ServerURLs: []string{url},
		StreamName: "KORA_SLOW_REALTIME", SubjectPrefix: "kora_slow_realtime", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, drain, err := p.Subscribe(ctx, "kora_slow_realtime.realtime.tenant.>")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer drain()
	payload := make([]byte, 256<<10)
	for range 128 {
		if err := p.PublishSubject(ctx, "kora_slow_realtime.realtime.tenant.changes", payload, ""); err != nil {
			t.Fatalf("publish slow-consumer fixture: %v", err)
		}
	}
	if err := p.nc.FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("flush slow-consumer fixture: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("bounded realtime subscription did not close after client queue overflow")
		}
	}
}

func TestSubscribeClosesWhenRealtimeMessageExceedsPayloadLimit(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	p, err := New(context.Background(), Config{
		Name: "kora-oversized-realtime-test", ServerURLs: []string{url},
		StreamName: "KORA_OVERSIZED_REALTIME", SubjectPrefix: "kora_oversized_realtime", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, drain, err := p.Subscribe(ctx, "kora_oversized_realtime.realtime.tenant.>")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer drain()
	if err := p.PublishSubject(ctx, "kora_oversized_realtime.realtime.tenant.changes", make([]byte, realtimeSubscriptionMaxMessage+1), ""); err != nil {
		t.Fatalf("publish oversized fixture: %v", err)
	}
	if err := p.nc.FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("flush oversized fixture: %v", err)
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("oversized realtime message was delivered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("realtime subscription did not close for oversized message")
	}
}
