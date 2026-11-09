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
	"sync/atomic"
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
	Bus                  *MultiBus
	Site                 string
	Config               CloudRelayConfig
	client               *http.Client
	closeCh              chan struct{}
	listenerCh           chan ChangeEvent
	productEvents        chan queuedCloudProductEvent
	startOnce            sync.Once
	stopOnce             sync.Once
	done                 chan struct{}
	productEventsDropped atomic.Uint64

	mu                sync.Mutex
	firstRecordByType map[string]bool
}

const (
	cloudProductEventQueueCapacity = 32
	maxCloudProductEventBytes      = 16 << 10
)

type queuedCloudProductEvent struct {
	event     CloudProductEventDTO
	dedupeKey string
}

type ProductEventQueueSnapshot struct {
	Queued   int
	Capacity int
	Dropped  uint64
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
		done:              make(chan struct{}),
		productEvents:     make(chan queuedCloudProductEvent, cloudProductEventQueueCapacity),
		firstRecordByType: map[string]bool{},
	}
}

// EnqueueProductEvent adds optional Cloud telemetry to the tenant-owned relay
// worker. It never performs network I/O on the caller's request path. The
// bounded queue drops excess or oversized telemetry rather than delaying
// tenant operations.
func (r *CloudRelay) EnqueueProductEvent(event CloudProductEventDTO, dedupeKey string) bool {
	if r == nil || event.Kind == "" {
		return false
	}
	if event.AccountID == "" {
		event.AccountID = r.Config.AccountID
	}
	if event.SiteID == "" {
		event.SiteID = r.siteID()
	}
	if event.AccountID == "" {
		return false
	}
	if !cloudProductEventWithinLimit(event) {
		r.productEventsDropped.Add(1)
		return false
	}
	payload, err := json.Marshal(event)
	if err != nil || len(payload) > maxCloudProductEventBytes {
		r.productEventsDropped.Add(1)
		return false
	}
	queued := queuedCloudProductEvent{event: event, dedupeKey: dedupeKey}
	select {
	case <-r.closeCh:
		return false
	default:
	}
	select {
	case <-r.closeCh:
		return false
	case r.productEvents <- queued:
		return true
	default:
		r.productEventsDropped.Add(1)
		return false
	}
}

// cloudProductEventWithinLimit rejects unknown payload types and bounds all
// string data before JSON encoding. The conservative six-times expansion
// accounts for JSON escaping of control characters.
func cloudProductEventWithinLimit(event CloudProductEventDTO) bool {
	stringBytes := len(event.SiteID) + len(event.AccountID) + len(event.Kind)
	add := func(value string) {
		stringBytes += len(value)
	}
	switch event.Kind {
	case "first_login":
		properties, ok := event.Properties.(FirstLoginPropertiesDTO)
		if !ok {
			return false
		}
		add(properties.User)
		add(properties.Site)
		add(properties.Login)
	case "first_doctype":
		properties, ok := event.Properties.(FirstDocTypePropertiesDTO)
		if !ok {
			return false
		}
		add(properties.DocType)
		add(properties.Site)
		add(properties.Status)
	case "first_record":
		properties, ok := event.Properties.(FirstRecordPropertiesDTO)
		if !ok {
			return false
		}
		add(properties.DocType)
		add(properties.DocName)
		add(properties.Operation)
		add(properties.ModifiedBy)
		add(properties.Site)
	default:
		return false
	}
	return stringBytes*6+256 <= maxCloudProductEventBytes
}

// ProductEventsDropped reports optional telemetry events discarded because
// they exceeded the payload limit or the bounded queue was full.
func (r *CloudRelay) ProductEventsDropped() uint64 {
	if r == nil {
		return 0
	}
	return r.productEventsDropped.Load()
}

func (r *CloudRelay) ProductEventQueueSnapshot() ProductEventQueueSnapshot {
	if r == nil {
		return ProductEventQueueSnapshot{}
	}
	return ProductEventQueueSnapshot{Queued: len(r.productEvents), Capacity: cap(r.productEvents), Dropped: r.productEventsDropped.Load()}
}

func (r *CloudRelay) Start() {
	r.startOnce.Do(func() {
		select {
		case <-r.closeCh:
			close(r.done)
			return
		default:
		}
		r.listenerCh = r.Bus.NewListenerQueue(false)
		if err := r.Bus.TryAddListener(r.listenerCh); err != nil {
			slog.Warn("cloud telemetry relay subscription rejected", "site", r.Site, "error", err)
			r.stopOnce.Do(func() { close(r.closeCh) })
			close(r.done)
			return
		}
		go func() {
			defer close(r.done)
			for {
				select {
				case <-r.closeCh:
					return
				default:
				}
				select {
				case <-r.closeCh:
					return
				case event := <-r.productEvents:
					SendCloudProductEvent(r.client, r.Config, event.event, event.dedupeKey)
				case event, ok := <-r.listenerCh:
					if !ok {
						return
					}
					r.handleEvent(event)
				}
			}
		}()
	})

	slog.Info("cloud telemetry relay started", "site", r.Site, "events_url", r.Config.EventsURL)
}

func (r *CloudRelay) Stop() {
	r.stopOnce.Do(func() {
		close(r.closeCh)
		r.startOnce.Do(func() { close(r.done) })
		if r.listenerCh != nil {
			r.Bus.RemoveListener(r.listenerCh)
		}
	})
	<-r.done
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
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
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
