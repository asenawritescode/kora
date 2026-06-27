package webhook

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/analytics"
	"github.com/oklog/ulid/v2"
)

// Worker delivers webhooks to extension endpoints.
// It subscribes to the analytics event bus and dispatches matching events.
type Worker struct {
	DB      *sql.DB
	Bus     *analytics.MultiBus
	Site    string
	client  *http.Client
	closeCh chan struct{}

	// listenerCh is the channel registered with the MultiBus for fan-out.
	listenerCh chan analytics.ChangeEvent

	// RetrySchedule defines the backoff for failed deliveries.
	RetrySchedule []time.Duration
	// MaxConsecutiveFailures before auto-disabling an extension.
	MaxConsecutiveFailures int
}

// DefaultRetrySchedule returns the standard 8-attempt retry schedule.
func DefaultRetrySchedule() []time.Duration {
	return []time.Duration{
		0,                // attempt 1: immediate
		30 * time.Second, // attempt 2
		2 * time.Minute,  // attempt 3
		10 * time.Minute, // attempt 4
		30 * time.Minute, // attempt 5
		2 * time.Hour,    // attempt 6
		8 * time.Hour,    // attempt 7
		24 * time.Hour,   // attempt 8 (final)
	}
}

// NewWorker creates a webhook delivery worker.
func NewWorker(db *sql.DB, bus *analytics.MultiBus, site string) *Worker {
	return &Worker{
		DB:      db,
		Bus:     bus,
		Site:    site,
		closeCh: make(chan struct{}),
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:    20,
				IdleConnTimeout: 60 * time.Second,
			},
		},
		RetrySchedule:          DefaultRetrySchedule(),
		MaxConsecutiveFailures: 15,
	}
}

// Start begins listening for events and delivering webhooks.
func (w *Worker) Start() {
	w.listenerCh = make(chan analytics.ChangeEvent, 1000)
	w.Bus.AddListener(w.listenerCh)

	go func() {
		for {
			select {
			case <-w.closeCh:
				return
			case event := <-w.listenerCh:
				w.handleEvent(event)
			}
		}
	}()

	slog.Info("webhook worker started", "site", w.Site)
}

// Stop shuts down the worker and removes its listener from the event bus.
func (w *Worker) Stop() {
	if w.listenerCh != nil {
		w.Bus.RemoveListener(w.listenerCh)
	}
	close(w.closeCh)
	slog.Info("webhook worker stopped", "site", w.Site)
}

// handleEvent processes a single change event and delivers to matching extensions.
func (w *Worker) handleEvent(event analytics.ChangeEvent) {
	// Map analytics operation to webhook event name.
	eventName := mapOpToEvent(event.Operation, event.Doctype)

	// Find matching extensions.
	extensions, err := w.loadMatchingExtensions(event.Doctype, eventName)
	if err != nil {
		slog.Error("webhook: loading extensions", "error", err)
		return
	}

	for _, ext := range extensions {
		w.deliver(ext, event, eventName)
	}
}

// deliver sends a single webhook delivery and handles retries.
func (w *Worker) deliver(ext Extension, event analytics.ChangeEvent, eventName string) {
	// Build event envelope.
	envelope := map[string]any{
		"id":             ulid.Make().String(),
		"source":         "kora",
		"event":          eventName,
		"version":        "1",
		"occurred_at":    event.Timestamp.Format(time.RFC3339Nano),
		"site":           event.Site,
		"data": map[string]any{
			"doctype":  event.Doctype,
			"name":     event.DocName,
			"document": event.Data,
			"old_data": event.OldData,
		},
	}
	body, _ := json.Marshal(envelope)

	// Compute signature using the raw signing secret.
	timestamp := time.Now().Unix()
	secret := ext.secret()
	sig := Sign(secret, body, timestamp)

	// POST to extension endpoint.
	req, err := http.NewRequest("POST", ext.EndpointURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("webhook: building request", "extension", ext.Name, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kora-Event", eventName)
	req.Header.Set("X-Kora-Site", event.Site)
	req.Header.Set("X-Kora-Signature", sig)
	req.Header.Set("X-Kora-Delivery", envelope["id"].(string))

	start := time.Now()
	resp, err := w.client.Do(req)
	duration := time.Since(start)

	// Log delivery.
	deliveryID := ulid.Make().String()
	status := "delivered"
	var respStatus int
	var respBody string
	var errMsg string

	if err != nil {
		status = "failed"
		errMsg = err.Error()
	} else {
		respStatus = resp.StatusCode
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		respBody = string(bodyBytes)
		if respStatus >= 500 {
			status = "retrying"
		} else if respStatus >= 400 && respStatus != 429 {
			status = "dead_lettered" // non-retryable
		}
	}

	w.logDelivery(deliveryID, ext.Name, envelope["id"].(string), eventName, ext.EndpointURL,
		status, 1, respStatus, respBody, errMsg, int(duration.Milliseconds()))

	// Update extension stats.
	w.updateExtensionStats(ext.Name, status)
}

// loadMatchingExtensions returns active extensions that subscribe to the given doctype event.
func (w *Worker) loadMatchingExtensions(doctype, eventName string) ([]Extension, error) {
	rows, err := w.DB.Query(
		`SELECT name, endpoint_url, secret, subscriptions, timeout_sec, consecutive_failures
		 FROM _kora_extension WHERE site = ? AND is_active = 1`, w.Site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var extensions []Extension
	for rows.Next() {
		var ext Extension
		var subsJSON string
		if err := rows.Scan(&ext.Name, &ext.EndpointURL, 		&ext.Secret, &subsJSON, &ext.TimeoutSec, &ext.ConsecutiveFailures); err != nil {
			continue
		}
		// Parse subscriptions JSON.
		var subs []Subscription
		json.Unmarshal([]byte(subsJSON), &subs)
		for _, sub := range subs {
			if sub.Event == eventName {
				if sub.Filter.DocType == "" || sub.Filter.DocType == doctype {
					extensions = append(extensions, ext)
					break
				}
			}
		}
	}
	return extensions, rows.Err()
}

// logDelivery records a delivery attempt.
func (w *Worker) logDelivery(id, extName, eventID, eventType, endpointURL, status string, attempt, respStatus int, respBody, errMsg string, durationMs int) {
	_, err := w.DB.Exec(
		`INSERT INTO _kora_webhook_delivery (id, extension_name, event_id, event_type, endpoint_url,
		 status, attempt, response_status, response_body, error_message, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))`,
		id, extName, eventID, eventType, endpointURL, status, attempt, respStatus, respBody, errMsg, durationMs)
	if err != nil {
		slog.Warn("webhook: logging delivery", "error", err)
	}
}

// updateExtensionStats updates delivery statistics for an extension.
func (w *Worker) updateExtensionStats(extName, status string) {
	switch status {
	case "delivered":
		w.DB.Exec(`UPDATE _kora_extension SET consecutive_failures = 0, last_delivery_at = NOW(6), last_error = NULL WHERE name = ?`, extName)
	case "dead_lettered", "failed":
		w.DB.Exec(`UPDATE _kora_extension SET consecutive_failures = consecutive_failures + 1, last_error = ? WHERE name = ?`, status, extName)
		// Auto-disable after max consecutive failures.
		var failures int
		w.DB.QueryRow(`SELECT consecutive_failures FROM _kora_extension WHERE name = ?`, extName).Scan(&failures)
		if failures >= w.MaxConsecutiveFailures {
			w.DB.Exec(`UPDATE _kora_extension SET is_active = 0 WHERE name = ?`, extName)
			slog.Warn("webhook: extension auto-disabled after consecutive failures",
				"extension", extName, "failures", failures)
		}
	}
}

// Extension represents a registered webhook extension.
type Extension struct {
	Name                 string
	EndpointURL          string
	Secret               string
	TimeoutSec           int
	ConsecutiveFailures  int
}

func (e Extension) secret() string { return e.Secret }

// Subscription defines an event filter for an extension.
type Subscription struct {
	Event  string            `json:"event"`
	Filter SubscriptionFilter `json:"filter,omitempty"`
}

// SubscriptionFilter narrows which events are delivered.
type SubscriptionFilter struct {
	DocType      string   `json:"doctype,omitempty"`
	ToState      []string `json:"to_state,omitempty"`
	FieldChanged []string `json:"field_changed,omitempty"`
}

// mapOpToEvent converts an analytics EventOp to a webhook-style event name.
func mapOpToEvent(op analytics.EventOp, doctype string) string {
	snakeName := toSnake(doctype)
	switch op {
	case analytics.EventInsert:
		return fmt.Sprintf("kora.%s.after_insert", snakeName)
	case analytics.EventUpdate:
		return fmt.Sprintf("kora.%s.after_save", snakeName)
	case analytics.EventDelete:
		return fmt.Sprintf("kora.%s.after_delete", snakeName)
	case analytics.EventSubmit:
		return fmt.Sprintf("kora.%s.after_submit", snakeName)
	case analytics.EventCancel:
		return fmt.Sprintf("kora.%s.after_cancel", snakeName)
	default:
		return fmt.Sprintf("kora.%s.%s", snakeName, string(op))
	}
}

func toSnake(name string) string {
	b := []byte(name)
	for i := 0; i < len(b); i++ {
		if b[i] == ' ' {
			b[i] = '_'
		} else if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

// RetryDeadLetters reattempts delivery of dead-lettered webhooks.
// Called by the scheduler's JobWebhook handler.
func (w *Worker) RetryDeadLetters() error {
	rows, err := w.DB.Query(
		`SELECT id, extension_name, event_id, event_type, endpoint_url, attempt
		 FROM _kora_webhook_delivery
		 WHERE status = 'dead_lettered' AND attempt < ?
		 ORDER BY created_at LIMIT 100`, len(w.RetrySchedule))
	if err != nil {
		return err
	}
	defer rows.Close()

	retried := 0
	for rows.Next() {
		var id, extName, eventID, eventType, endpointURL string
		var attempt int
		if err := rows.Scan(&id, &extName, &eventID, &eventType, &endpointURL, &attempt); err != nil {
			continue
		}
		// Schedule retry with backoff.
		nextAttempt := attempt + 1
		if nextAttempt >= len(w.RetrySchedule) {
			continue // exhausted all retries
		}
		delay := w.RetrySchedule[nextAttempt]
		jitter := time.Duration(float64(delay) * (0.5 + float64(time.Now().UnixNano()%1000)/2000.0))
		if jitter < 0 {
			jitter = delay
		}

		w.DB.Exec(`UPDATE _kora_webhook_delivery SET status = 'retrying', attempt = ?, next_retry_at = ? WHERE id = ?`,
			nextAttempt, time.Now().Add(jitter), id)
		retried++
	}
	slog.Info("webhook: retried dead letters", "count", retried)
	return nil
}

