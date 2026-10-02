package natsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func runEmbeddedServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := &server.Options{JetStream: true, Port: -1}
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

	if err := p.Publish(context.Background(), contract.EventEnvelope{
		ID:   "evt-1",
		Type: "events.document.created",
		Site: "site-a",
		Data: json.RawMessage(`{"data":{"name":"Test"}}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
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
	if directoryInfo.State.Msgs != 2 || eventInfo.State.Msgs != 0 || directoryInfo.Config.Storage != nats.FileStorage {
		t.Fatalf("directory/event stream state = messages %d/%d, storage %v; want 2/0 and file storage", directoryInfo.State.Msgs, eventInfo.State.Msgs, directoryInfo.Config.Storage)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("expected validation error")
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
	drain()
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected drained channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for drained channel to close")
	}
}
