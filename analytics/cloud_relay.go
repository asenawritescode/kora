package analytics

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	cloudEventOnceMu sync.Mutex
	cloudEventOnce   = map[string]bool{}
)

type CloudRelayConfig struct {
	EventsURL   string
	SiteID      string
	AccountID   string
	EventsToken string
}

func LoadCloudRelayConfig() *CloudRelayConfig {
	cfg := &CloudRelayConfig{
		EventsURL:   strings.TrimSpace(os.Getenv("KORA_CLOUD_EVENTS_URL")),
		SiteID:      strings.TrimSpace(os.Getenv("KORA_CLOUD_SITE_ID")),
		AccountID:   strings.TrimSpace(os.Getenv("KORA_CLOUD_ACCOUNT_ID")),
		EventsToken: strings.TrimSpace(os.Getenv("KORA_CLOUD_EVENTS_TOKEN")),
	}
	if cfg.EventsURL == "" || cfg.AccountID == "" {
		return nil
	}
	return cfg
}

type CloudRelay struct {
	Bus        *MultiBus
	Site       string
	Config     CloudRelayConfig
	client     *http.Client
	closeCh    chan struct{}
	listenerCh chan ChangeEvent

	mu                sync.Mutex
	firstRecordByType map[string]bool
}

func NewCloudRelay(bus *MultiBus, site string, cfg CloudRelayConfig) *CloudRelay {
	return &CloudRelay{
		Bus:    bus,
		Site:   site,
		Config: cfg,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:    20,
				IdleConnTimeout: 60 * time.Second,
			},
		},
		closeCh:           make(chan struct{}),
		firstRecordByType: map[string]bool{},
	}
}

func (r *CloudRelay) Start() {
	r.listenerCh = make(chan ChangeEvent, 1000)
	r.Bus.AddListener(r.listenerCh)

	go func() {
		for {
			select {
			case <-r.closeCh:
				return
			case event := <-r.listenerCh:
				r.handleEvent(event)
			}
		}
	}()

	slog.Info("cloud telemetry relay started", "site", r.Site, "events_url", r.Config.EventsURL)
}

func (r *CloudRelay) Stop() {
	if r.listenerCh != nil {
		r.Bus.RemoveListener(r.listenerCh)
	}
	close(r.closeCh)
}

func (r *CloudRelay) handleEvent(event ChangeEvent) {
	if event.Operation != EventInsert {
		return
	}
	if strings.HasPrefix(event.Doctype, "_kora_") {
		return
	}
	if !r.markFirstRecord(event.Doctype) {
		return
	}

	SendCloudProductEvent(r.client, r.Config, CloudProductEventDTO{
		SiteID:    r.siteID(),
		AccountID: r.Config.AccountID,
		Kind:      "first_record",
		Properties: FirstRecordPropertiesDTO{
			DocType:    event.Doctype,
			DocName:    event.DocName,
			Operation:  string(event.Operation),
			ModifiedBy: event.ModifiedBy,
			Site:       event.Site,
		},
	}, "")
}

func (r *CloudRelay) markFirstRecord(doctype string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.firstRecordByType[doctype] {
		return false
	}
	r.firstRecordByType[doctype] = true
	return true
}

func (r *CloudRelay) siteID() string {
	if r.Config.SiteID != "" {
		return r.Config.SiteID
	}
	if r.Site != "" {
		return r.Site
	}
	return "unknown-site"
}

type CloudProductEventDTO struct {
	SiteID     string `json:"site_id"`
	AccountID  string `json:"account_id"`
	Kind       string `json:"kind"`
	Properties any    `json:"properties,omitempty"`
}

type FirstRecordPropertiesDTO struct {
	DocType    string `json:"doctype"`
	DocName    string `json:"doc_name"`
	Operation  string `json:"operation"`
	ModifiedBy string `json:"modified_by"`
	Site       string `json:"site"`
}

type FirstLoginPropertiesDTO struct {
	User  string `json:"user"`
	Site  string `json:"site"`
	Login string `json:"login"`
}

type FirstDocTypePropertiesDTO struct {
	DocType string `json:"doctype"`
	Site    string `json:"site"`
	Status  string `json:"status"`
}

func SendCloudProductEvent(client *http.Client, cfg CloudRelayConfig, event CloudProductEventDTO, dedupeKey string) {
	if cfg.EventsURL == "" || event.AccountID == "" {
		return
	}
	if dedupeKey != "" && !markCloudEventOnce(dedupeKey) {
		return
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	now := time.Now().UTC()
	payload := map[string]any{
		"id":          "evt-" + now.Format("20060102150405.000000000"),
		"site_id":     event.SiteID,
		"account_id":  event.AccountID,
		"kind":        event.Kind,
		"occurred_at": now,
		"properties":  event.Properties,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("cloud telemetry relay marshal failed", "site_id", event.SiteID, "kind", event.Kind, "error", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, cfg.EventsURL, bytes.NewReader(body))
	if err != nil {
		slog.Warn("cloud telemetry relay request build failed", "site_id", event.SiteID, "kind", event.Kind, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.EventsToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.EventsToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("cloud telemetry relay delivery failed", "site_id", event.SiteID, "kind", event.Kind, "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		slog.Warn("cloud telemetry relay non-2xx response", "site_id", event.SiteID, "kind", event.Kind, "status", resp.StatusCode)
	}
}

func markCloudEventOnce(key string) bool {
	cloudEventOnceMu.Lock()
	defer cloudEventOnceMu.Unlock()
	if cloudEventOnce[key] {
		return false
	}
	cloudEventOnce[key] = true
	return true
}
