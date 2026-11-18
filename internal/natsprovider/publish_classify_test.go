package natsprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/nats-io/nats.go"
)

func TestClassifyPublishError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		full     bool
		wantSame bool
	}{
		{"nil", nil, false, true},
		{"messages at limit", &nats.APIError{Code: 500, Description: streamMaxMsgsDescription}, true, false},
		{"bytes at limit", &nats.APIError{Code: 500, Description: streamMaxBytesDescription}, true, false},
		{"deadline is an ordinary failure", fmt.Errorf("publish: %w", context.DeadlineExceeded), false, true},
		{"other api error", &nats.APIError{Code: 500, Description: "stream not found"}, false, true},
		{"plain error", errors.New("connection closed"), false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPublishError(tc.err)
			if errors.Is(got, contract.ErrDestinationFull) != tc.full {
				t.Fatalf("classifyPublishError(%v) full = %t, want %t", tc.err, !tc.full, tc.full)
			}
			if tc.wantSame && !errors.Is(got, tc.err) {
				t.Fatalf("classifyPublishError changed unrelated error: %v", got)
			}
			if tc.err != nil && !errors.Is(got, tc.err) {
				t.Fatalf("classified error %v does not wrap original %v", got, tc.err)
			}
		})
	}
}

func TestPublishReportsFullStreamAsDestinationFull(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	ctx := context.Background()
	p, err := New(ctx, Config{
		Name: "kora-full-stream-test", ServerURLs: []string{url}, StreamName: "KORA_FULL_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5, StreamMaxBytes: 1 << 20, StreamMaxMessages: 1,
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if bootErr := p.Bootstrap(ctx); bootErr != nil {
		t.Fatal("bootstrap:", bootErr)
	}
	event := func(id string) contract.EventEnvelope {
		return contract.EventEnvelope{ID: id, Type: "events.document.created", Version: 1, Site: "site-a", Data: json.RawMessage(`{}`)}
	}
	if firstErr := p.Publish(ctx, event("evt-1")); firstErr != nil {
		t.Fatal("first publish:", firstErr)
	}
	err = p.Publish(ctx, event("evt-2"))
	if !errors.Is(err, contract.ErrDestinationFull) {
		t.Fatalf("publish past message cap = %v, want contract.ErrDestinationFull", err)
	}
}

func TestStreamUsageIsStoredByRefreshOnly(t *testing.T) {
	s, url := runEmbeddedServer(t)
	defer s.Shutdown()
	ctx := context.Background()
	p, err := New(ctx, Config{
		Name: "kora-usage-test", ServerURLs: []string{url}, StreamName: "KORA_USAGE_EVENTS",
		SubjectPrefix: "kora", MaxDeliver: 5,
	})
	if err != nil {
		t.Fatal("new provider:", err)
	}
	defer p.Close()
	if err := p.Bootstrap(ctx); err != nil {
		t.Fatal("bootstrap:", err)
	}
	if _, ok := p.StreamUsage(); ok {
		t.Fatal("StreamUsage reported a sample before any refresh")
	}
	if err := p.RefreshStreamUsage(ctx); err != nil {
		t.Fatal("refresh stream usage:", err)
	}
	usage, ok := p.StreamUsage()
	if !ok || usage.StreamName != "KORA_USAGE_EVENTS" {
		t.Fatalf("StreamUsage = (%+v, %t), want stored sample for KORA_USAGE_EVENTS", usage, ok)
	}
}
