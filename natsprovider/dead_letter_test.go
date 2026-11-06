package natsprovider

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats.go"
)

func TestDeadLetterSubjectValidation(t *testing.T) {
	base := Config{ServerURLs: []string{"nats://127.0.0.1:4222"}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5}
	with := func(edit func(*Config)) Config {
		c := base
		edit(&c)
		return c
	}
	tests := []struct {
		name    string
		config  Config
		wantErr []string
	}{
		{name: "default dead letter is outside the default consumer filter", config: base},
		{name: "default from environment", config: func() Config {
			t.Setenv("KORA_NATS_URLS", "nats://127.0.0.1:4222")
			t.Setenv("KORA_NATS_DEAD_LETTER", "")
			t.Setenv("KORA_NATS_SUBJECT_PREFIX", "")
			return FromEnv()
		}()},
		{
			name:    "legacy dead letter inside the default filter",
			config:  with(func(c *Config) { c.DeadLetterSubject = "kora.deadletter" }),
			wantErr: []string{"kora.deadletter", "kora.>", "DeadLetterSubject", "ConsumerSubject"},
		},
		{
			name:    "dead letter inside an explicit filter",
			config:  with(func(c *Config) { c.DeadLetterSubject = "kora.events.dead"; c.ConsumerSubject = "kora.events.>" }),
			wantErr: []string{"kora.events.dead", "kora.events.>"},
		},
		{
			name:   "dead letter beside a narrow explicit filter",
			config: with(func(c *Config) { c.DeadLetterSubject = "kora.deadletter"; c.ConsumerSubject = "kora.events.>" }),
		},
		{name: "custom prefix with derived dead letter", config: with(func(c *Config) { c.SubjectPrefix = "acme" })},
		{
			name:   "custom prefix with custom dead letter",
			config: with(func(c *Config) { c.SubjectPrefix = "acme"; c.DeadLetterSubject = "ops.dead.letters" }),
		},
		{
			name:    "dead letter must be a literal subject",
			config:  with(func(c *Config) { c.DeadLetterSubject = "dead.>" }),
			wantErr: []string{"dead.>", "literal"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if len(test.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() accepted an unsafe dead-letter configuration")
			}
			for _, fragment := range test.wantErr {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("Validate() = %q, want it to mention %q", err, fragment)
				}
			}
		})
	}
}

func TestDefaultDeadLetterSubjectFollowsPrefix(t *testing.T) {
	t.Setenv("KORA_NATS_DEAD_LETTER", "")
	for prefix, want := range map[string]string{"": "kora_deadletter", "acme": "acme_deadletter"} {
		t.Setenv("KORA_NATS_SUBJECT_PREFIX", prefix)
		if got := FromEnv().DeadLetterSubject; got != want {
			t.Fatalf("prefix %q: default dead letter = %q, want %q", prefix, got, want)
		}
	}
	t.Setenv("KORA_NATS_DEAD_LETTER", "ops.dead")
	if got := FromEnv().DeadLetterSubject; got != "ops.dead" {
		t.Fatalf("explicit KORA_NATS_DEAD_LETTER = %q", got)
	}
}

func TestSubjectMatchesFilter(t *testing.T) {
	tests := []struct {
		filter, subject string
		want            bool
	}{
		{"kora.>", "kora.deadletter", true},
		{"kora.>", "kora", false},
		{"kora.>", "kora_deadletter", false},
		{"kora.events.>", "kora.deadletter", false},
		{"kora.*", "kora.deadletter", true},
		{"kora.*", "kora.a.b", false},
		{"kora.kora.>", "kora.deadletter", false},
		{"kora.tasks.x", "kora.tasks.x", true},
		{">", "anything.at.all", true},
	}
	for _, test := range tests {
		if got := subjectMatchesFilter(test.filter, test.subject); got != test.want {
			t.Errorf("subjectMatchesFilter(%q, %q) = %v, want %v", test.filter, test.subject, got, test.want)
		}
	}
}

func TestBootstrapAddsDeadLetterSubjectToLegacyStream(t *testing.T) {
	_, url := runEmbeddedServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	// Streams created by earlier releases only capture "<prefix>.>".
	if _, err := js.AddStream(&nats.StreamConfig{Name: "KORA_EVENTS", Subjects: []string{"kora.>"}, Storage: nats.FileStorage, Retention: nats.LimitsPolicy, MaxBytes: 1 << 20, MaxMsgs: 1000, Discard: nats.DiscardNew}); err != nil {
		t.Fatal(err)
	}
	p, err := New(context.Background(), Config{ServerURLs: []string{url}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("bootstrap legacy stream: %v", err)
	}
	info, err := js.StreamInfo("KORA_EVENTS")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(info.Config.Subjects, ","); got != "kora.>,kora_deadletter" {
		t.Fatalf("stream subjects = %q, want the legacy subject plus the dead-letter subject", got)
	}
	// A second bootstrap is a no-op.
	if err := p.Bootstrap(context.Background()); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
}

func TestBootstrapRejectsUnrelatedStreamSubjects(t *testing.T) {
	_, url := runEmbeddedServer(t)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{Name: "KORA_EVENTS", Subjects: []string{"kora.>", "unrelated.>"}, Storage: nats.FileStorage}); err != nil {
		t.Fatal(err)
	}
	p, err := New(context.Background(), Config{ServerURLs: []string{url}, StreamName: "KORA_EVENTS", SubjectPrefix: "kora", MaxDeliver: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Bootstrap(context.Background()); err == nil {
		t.Fatal("bootstrap accepted a stream with unrelated subjects")
	}
}

// TestDefaultConfigDeadLetterDoesNotFeedConsumer is the regression for the
// self-feeding loop: with the default configuration (consumer on the whole
// "<prefix>.>" tree) a poison message is dead-lettered exactly once and the
// stream does not keep growing.
func TestDefaultConfigDeadLetterDoesNotFeedConsumer(t *testing.T) {
	_, url := runEmbeddedServer(t)
	t.Setenv("KORA_NATS_URLS", url)
	for _, key := range []string{"KORA_NATS_SUBJECT_PREFIX", "KORA_NATS_DEAD_LETTER", "KORA_NATS_STREAM", "KORA_NATS_CONSUMER"} {
		t.Setenv(key, "")
	}
	cfg := FromEnv()
	if cfg.ConsumerSubject != "" {
		t.Fatalf("default config must use the default consumer filter, got %q", cfg.ConsumerSubject)
	}
	p, err := New(context.Background(), cfg)
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
	var deliveries atomic.Int32
	runCtx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- consumer.Run(runCtx, func(context.Context, contract.Delivery) error {
			deliveries.Add(1)
			return context.DeadlineExceeded
		})
	}()
	defer func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("consumer did not stop")
		}
	}()

	if _, err := p.js.Publish("kora.events.poison", []byte(`{"id":"poison"}`)); err != nil {
		t.Fatalf("publish poison message: %v", err)
	}
	streamMsgs := func() uint64 {
		info, err := p.js.StreamInfo(cfg.StreamName)
		if err != nil {
			t.Fatalf("stream info: %v", err)
		}
		return info.State.Msgs
	}
	deadline := time.Now().Add(5 * time.Second)
	for streamMsgs() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("dead letter was not stored in the stream (%d messages)", streamMsgs())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Several redelivery windows: a self-feeding loop writes hundreds of
	// messages in this time, a correct configuration writes none.
	time.Sleep(800 * time.Millisecond)
	if got := streamMsgs(); got != 2 {
		t.Fatalf("stream holds %d messages, want the source plus exactly one dead letter", got)
	}
	if got := deliveries.Load(); got != int32(cfg.MaxDeliver) {
		t.Fatalf("handler ran %d times, want %d (dead letters must not be re-consumed)", got, cfg.MaxDeliver)
	}
	msg, err := p.js.GetLastMsg(cfg.StreamName, cfg.DeadLetterSubject)
	if err != nil || msg == nil {
		t.Fatalf("dead letter not stored under %q: %v", cfg.DeadLetterSubject, err)
	}
}
