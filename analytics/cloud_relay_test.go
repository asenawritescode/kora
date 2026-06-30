package analytics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type stubBus struct {
	ch chan ChangeEvent
}

func (s *stubBus) Publish(event ChangeEvent) error         { s.ch <- event; return nil }
func (s *stubBus) Subscribe() (<-chan ChangeEvent, error)  { return s.ch, nil }
func (s *stubBus) DrainWAL(func(ChangeEvent)) (int, error) { return 0, nil }
func (s *stubBus) RotateWAL() (string, error)              { return "", nil }
func (s *stubBus) CommitWALRotation(string) error          { return nil }
func (s *stubBus) Dropped() int64                          { return 0 }
func (s *stubBus) Close() error                            { close(s.ch); return nil }

func TestCloudRelayPostsFirstRecordOnlyOncePerDoctype(t *testing.T) {
	t.Parallel()

	var got []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		defer r.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		got = append(got, payload)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	bus := &stubBus{ch: make(chan ChangeEvent, 10)}
	mb, err := NewMultiBus(bus)
	if err != nil {
		t.Fatalf("NewMultiBus() error = %v", err)
	}
	defer mb.Close()

	relay := NewCloudRelay(mb, "tenant.example.com", CloudRelayConfig{
		EventsURL:   server.URL,
		SiteID:      "site-1",
		AccountID:   "acct-1",
		EventsToken: "secret",
	})
	relay.Start()
	defer relay.Stop()

	now := time.Now().UTC()
	_ = bus.Publish(ChangeEvent{Site: "tenant.example.com", Doctype: "Invoice", DocName: "INV-1", Operation: EventInsert, Timestamp: now, ModifiedBy: "a@test"})
	_ = bus.Publish(ChangeEvent{Site: "tenant.example.com", Doctype: "Invoice", DocName: "INV-2", Operation: EventInsert, Timestamp: now.Add(time.Second), ModifiedBy: "a@test"})
	_ = bus.Publish(ChangeEvent{Site: "tenant.example.com", Doctype: "_kora_script", DocName: "SCR-1", Operation: EventInsert, Timestamp: now.Add(2 * time.Second), ModifiedBy: "a@test"})

	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if len(got) != 1 {
		t.Fatalf("expected 1 relayed event, got %d", len(got))
	}
	if got[0]["kind"] != "first_record" {
		t.Fatalf("kind = %#v", got[0]["kind"])
	}
	props, ok := got[0]["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties type = %T", got[0]["properties"])
	}
	if props["doctype"] != "Invoice" {
		t.Fatalf("doctype = %#v", props["doctype"])
	}
}

func TestLoadCloudRelayConfigRequiresURLAndAccount(t *testing.T) {
	t.Setenv("KORA_CLOUD_EVENTS_URL", "")
	t.Setenv("KORA_CLOUD_ACCOUNT_ID", "")
	if cfg := LoadCloudRelayConfig(); cfg != nil {
		t.Fatalf("expected nil config, got %#v", cfg)
	}

	t.Setenv("KORA_CLOUD_EVENTS_URL", "https://cloud.example.com/api/cloud/events")
	t.Setenv("KORA_CLOUD_ACCOUNT_ID", "acct-1")
	t.Setenv("KORA_CLOUD_SITE_ID", "site-1")
	t.Setenv("KORA_CLOUD_EVENTS_TOKEN", "secret")
	cfg := LoadCloudRelayConfig()
	if cfg == nil {
		t.Fatal("expected config")
	}
	if cfg.SiteID != "site-1" || cfg.EventsToken != "secret" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestSendCloudProductEventDedupes(t *testing.T) {
	t.Parallel()

	cloudEventOnceMu.Lock()
	cloudEventOnce = map[string]bool{}
	cloudEventOnceMu.Unlock()

	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	cfg := CloudRelayConfig{
		EventsURL:   server.URL,
		SiteID:      "site-1",
		AccountID:   "acct-1",
		EventsToken: "secret",
	}
	client := &http.Client{Timeout: 2 * time.Second}

	SendCloudProductEvent(client, cfg, CloudProductEventDTO{
		SiteID:    "site-1",
		AccountID: "acct-1",
		Kind:      "first_login",
		Properties: FirstLoginPropertiesDTO{
			User:  "a@test",
			Site:  "site-1",
			Login: "password",
		},
	}, "first_login:site-1:a@test")
	SendCloudProductEvent(client, cfg, CloudProductEventDTO{
		SiteID:    "site-1",
		AccountID: "acct-1",
		Kind:      "first_login",
		Properties: FirstLoginPropertiesDTO{
			User:  "a@test",
			Site:  "site-1",
			Login: "password",
		},
	}, "first_login:site-1:a@test")

	if count != 1 {
		t.Fatalf("expected 1 request, got %d", count)
	}
}
