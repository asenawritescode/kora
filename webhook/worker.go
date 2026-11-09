package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/eventstream"
	"github.com/asenawritescode/kora/internal/netguard"
	"github.com/oklog/ulid/v2"
)

// Worker delivers webhooks to extension endpoints.
// It subscribes to the analytics event bus and dispatches matching events.
type Worker struct {
	DB      *sql.DB
	Dialect db.QueryDialect
	Bus     interface {
		AddListener(chan eventstream.ChangeEvent)
		RemoveListener(chan eventstream.ChangeEvent)
	}
	Site          string
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	client  *http.Client
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	started bool
	stopped bool

	// listenerCh is the channel registered with the MultiBus for fan-out.
	listenerCh chan eventstream.ChangeEvent

	// RetrySchedule defines the backoff for failed deliveries.
	RetrySchedule []time.Duration
	// MaxConsecutiveFailures before auto-disabling an extension.
	MaxConsecutiveFailures int
}

const defaultEventQueueSize = 16

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
func NewWorker(database *sql.DB, bus interface {
	AddListener(chan eventstream.ChangeEvent)
	RemoveListener(chan eventstream.ChangeEvent)
}, site string, dialects ...db.QueryDialect) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	var dialect db.QueryDialect = db.Resolve("mysql")
	if len(dialects) > 0 && dialects[0] != nil {
		dialect = dialects[0]
	}
	return &Worker{
		DB:      database,
		Dialect: dialect,
		Bus:     bus,
		Site:    site,
		done:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext:     netguard.PublicDialer(10*time.Second, 30*time.Second).DialContext,
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
	w.StartContext(context.Background())
}

// StartContext begins listening under a tenant-owned context. Stop cancels
// in-flight database and HTTP operations and waits for the worker to exit.
func (w *Worker) StartContext(parent context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.stopped {
		return
	}
	if parent != nil {
		w.cancel()
		w.ctx, w.cancel = context.WithCancel(parent)
	}
	queueCapacity := defaultEventQueueSize
	if bounded, ok := w.Bus.(interface{ ListenerQueueCapacity(bool) int }); ok {
		if configured := bounded.ListenerQueueCapacity(false); configured > 0 && configured < queueCapacity {
			queueCapacity = configured
		}
	}
	w.listenerCh = make(chan eventstream.ChangeEvent, queueCapacity)
	if w.Bus != nil {
		w.Bus.AddListener(w.listenerCh)
	}
	w.started = true

	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.ctx.Done():
				return
			case event, ok := <-w.listenerCh:
				if !ok {
					return
				}
				if err := w.processLiveEvent(w.ctx, event); err != nil && w.ctx.Err() == nil {
					slog.Error("webhook: processing admitted live event", "event_id", event.ID, "error", err)
				}
			}
		}
	}()

	slog.Info("webhook worker started", "site", w.Site)
}

// Stop shuts down the worker and removes its listener from the event bus.
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		if w.listenerCh != nil && w.Bus != nil {
			w.Bus.RemoveListener(w.listenerCh)
		}
		w.cancel()
		slog.Info("webhook worker stopped", "site", w.Site)
	}
	started := w.started
	done := w.done
	w.mu.Unlock()
	if started {
		<-done
	}
	w.client.CloseIdleConnections()
}

func (w *Worker) processEvent(ctx context.Context, event eventstream.ChangeEvent) error {
	return w.processEventWithReceiptCheck(ctx, event, false)
}

// processLiveEvent retains a live event in the single worker while waiting for
// tenant active-work capacity. Outbox dispatch already owns one permit around
// its durable batch and calls processEvent directly to avoid nested admission.
func (w *Worker) processLiveEvent(ctx context.Context, event eventstream.ChangeEvent) error {
	// Individual SQL phases below acquire admission. Keeping one permit around
	// processEvent would also hold it across extension HTTP requests.
	return w.processEvent(ctx, event)
}

func (w *Worker) processEventWithReceiptCheck(ctx context.Context, event eventstream.ChangeEvent, checkDeliveryReceipts bool) error {
	if event.Operation == eventstream.EventNotification {
		return nil
	}
	// Map analytics operation to webhook event name.
	eventName := mapOpToEvent(event.Operation, event.Doctype)

	// Find matching extensions.
	extensions, err := w.loadMatchingExtensions(ctx, event.Doctype, eventName)
	if err != nil {
		return fmt.Errorf("load matching extensions: %w", err)
	}

	var deliveryErrors []error
	for _, ext := range extensions {
		if ctx.Err() != nil {
			return errors.Join(append(deliveryErrors, ctx.Err())...)
		}
		if checkDeliveryReceipts {
			completed, err := w.deliveryAlreadyFinalized(ctx, ext.Name, event.ID)
			if err != nil {
				deliveryErrors = append(deliveryErrors, fmt.Errorf("check prior delivery for extension %s: %w", ext.Name, err))
				continue
			}
			if completed {
				continue
			}
		}
		if err := w.deliver(ctx, ext, event, eventName); err != nil {
			deliveryErrors = append(deliveryErrors, fmt.Errorf("extension %s: %w", ext.Name, err))
		}
	}
	return errors.Join(deliveryErrors...)
}

func (w *Worker) deliveryAlreadyFinalized(ctx context.Context, extensionName, eventID string) (bool, error) {
	if w == nil || w.DB == nil || w.Dialect == nil || extensionName == "" || eventID == "" {
		return false, fmt.Errorf("webhook delivery receipt lookup requires database, dialect, extension and event")
	}
	release, err := w.acquireDBWork(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	var count int
	err = w.DB.QueryRowContext(ctx, db.Rebind(w.Dialect,
		`SELECT COUNT(*) FROM _kora_webhook_delivery
		 WHERE extension_name = ? AND event_id = ? AND status IN ('delivered', 'dead_lettered')`),
		extensionName, eventID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query delivery receipt: %w", err)
	}
	return count > 0, nil
}

// deliver sends a single webhook delivery and handles retries.
func (w *Worker) deliver(ctx context.Context, ext Extension, event eventstream.ChangeEvent, eventName string) error {
	// Build event envelope.
	eventID := event.ID
	if eventID == "" {
		eventID = ulid.Make().String()
	}
	envelope := map[string]any{
		"id":          eventID,
		"source":      "kora",
		"event":       eventName,
		"version":     "1",
		"occurred_at": event.Timestamp.Format(time.RFC3339Nano),
		"site":        event.Site,
		"data": map[string]any{
			"doctype":  event.Doctype,
			"name":     event.DocName,
			"document": event.Data,
			"old_data": event.OldData,
		},
	}
	body, _ := json.Marshal(envelope)
	if len(body) > 1<<20 {
		return fmt.Errorf("webhook payload exceeds 1 MiB delivery limit")
	}

	// Compute signature using the raw signing secret.
	timestamp := time.Now().Unix()
	secret := ext.secret()
	sig := Sign(secret, body, timestamp)

	// POST to extension endpoint.
	req, err := http.NewRequestWithContext(ctx, "POST", ext.EndpointURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("webhook: building request", "extension", ext.Name, "error", err)
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kora-Event", eventName)
	req.Header.Set("X-Kora-Site", event.Site)
	req.Header.Set("X-Kora-Signature", sig)
	req.Header.Set("X-Kora-Delivery", envelope["id"].(string))
	req.Header.Set("Idempotency-Key", eventID+"."+ext.Name)

	start := time.Now()
	resp, err := w.client.Do(req)
	duration := time.Since(start)

	// Log delivery.
	deliveryID := ulid.Make().String()
	status := "delivered"
	var respStatus int
	var respBody string
	var errMsg string
	var deliveryErr error

	if err != nil {
		status = "failed"
		errMsg = err.Error()
		deliveryErr = fmt.Errorf("send webhook request: %w", err)
	} else {
		respStatus = resp.StatusCode
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxStoredResponseBody))
		resp.Body.Close()
		respBody = string(bodyBytes)
		if respStatus >= 500 || respStatus == http.StatusTooManyRequests {
			status = "retrying"
			deliveryErr = fmt.Errorf("webhook endpoint returned retryable HTTP %d", respStatus)
		} else if respStatus >= 400 && respStatus != 429 {
			status = "dead_lettered" // non-retryable
		}
	}

	logCtx, cancelLog := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancelLog()
	if err := w.logDelivery(logCtx, deliveryID, ext.Name, envelope["id"].(string), eventName, ext.EndpointURL,
		status, 1, respStatus, respBody, errMsg, int(duration.Milliseconds())); err != nil {
		deliveryErr = errors.Join(deliveryErr, err)
	}

	// Update extension stats.
	w.updateExtensionStats(logCtx, ext.Name, status)
	return deliveryErr
}

// loadMatchingExtensions returns active extensions that subscribe to the given doctype event.
func (w *Worker) loadMatchingExtensions(ctx context.Context, doctype, eventName string) ([]Extension, error) {
	release, err := w.acquireDBWork(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := w.DB.QueryContext(ctx,
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
		if err := rows.Scan(&ext.Name, &ext.EndpointURL, &ext.Secret, &subsJSON, &ext.TimeoutSec, &ext.ConsecutiveFailures); err != nil {
			continue
		}
		// Parse subscriptions JSON.
		var subs []Subscription
		if err := json.Unmarshal([]byte(subsJSON), &subs); err != nil {
			slog.Warn("webhook: extension subscriptions are not valid JSON; skipping extension", "extension", ext.Name, "error", err)
			continue
		}
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
func (w *Worker) logDelivery(ctx context.Context, id, extName, eventID, eventType, endpointURL, status string, attempt, respStatus int, respBody, errMsg string, durationMs int) error {
	release, err := w.acquireDBWork(ctx)
	if err != nil {
		return err
	}
	defer release()
	_, err = w.DB.ExecContext(ctx,
		`INSERT INTO _kora_webhook_delivery (id, extension_name, event_id, event_type, endpoint_url,
		 status, attempt, response_status, response_body, error_message, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))`,
		id, extName, eventID, eventType, endpointURL, status, attempt, respStatus, respBody, errMsg, durationMs)
	if err != nil {
		return fmt.Errorf("persist webhook delivery attempt: %w", err)
	}
	return nil
}

// updateExtensionStats updates delivery statistics for an extension.
func (w *Worker) updateExtensionStats(ctx context.Context, extName, status string) {
	release, err := w.acquireDBWork(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("webhook: extension stats deferred by database work admission", "extension", extName, "error", err)
		}
		return
	}
	defer release()
	switch status {
	case "delivered":
		_, _ = w.DB.ExecContext(ctx, `UPDATE _kora_extension SET consecutive_failures = 0, last_delivery_at = NOW(6), last_error = NULL WHERE name = ?`, extName)
	case "dead_lettered", "failed":
		_, _ = w.DB.ExecContext(ctx, `UPDATE _kora_extension SET consecutive_failures = consecutive_failures + 1, last_error = ? WHERE name = ?`, status, extName)
		// Auto-disable after max consecutive failures.
		var failures int
		_ = w.DB.QueryRowContext(ctx, `SELECT consecutive_failures FROM _kora_extension WHERE name = ?`, extName).Scan(&failures)
		if failures >= w.MaxConsecutiveFailures {
			_, _ = w.DB.ExecContext(ctx, `UPDATE _kora_extension SET is_active = 0 WHERE name = ?`, extName)
			slog.Warn("webhook: extension auto-disabled after consecutive failures",
				"extension", extName, "failures", failures)
		}
	}
}

func (w *Worker) acquireDBWork(ctx context.Context) (func(), error) {
	if w == nil || w.WorkAdmission == nil {
		return func() {}, nil
	}
	if ctx == nil {
		return nil, fmt.Errorf("webhook database work requires a context")
	}
	for {
		release, err := w.WorkAdmission.Acquire(ctx)
		if err == nil {
			return release, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// maxStoredResponseBody bounds how much of an endpoint's reply is kept in the
// delivery log, which tenant administrators can read back.
const maxStoredResponseBody = 2048

// Extension represents a registered webhook extension.
type Extension struct {
	Name                string
	EndpointURL         string
	Secret              string
	TimeoutSec          int
	ConsecutiveFailures int
}

func (e Extension) secret() string { return e.Secret }

// Subscription defines an event filter for an extension.
type Subscription struct {
	Event  string             `json:"event"`
	Filter SubscriptionFilter `json:"filter,omitempty"`
}

// SubscriptionFilter narrows which events are delivered.
type SubscriptionFilter struct {
	DocType      string   `json:"doctype,omitempty"`
	ToState      []string `json:"to_state,omitempty"`
	FieldChanged []string `json:"field_changed,omitempty"`
}

// mapOpToEvent converts an analytics EventOp to a webhook-style event name.
func mapOpToEvent(op eventstream.EventOp, doctype string) string {
	snakeName := toSnake(doctype)
	switch op {
	case eventstream.EventInsert:
		return fmt.Sprintf("kora.%s.after_insert", snakeName)
	case eventstream.EventUpdate:
		return fmt.Sprintf("kora.%s.after_save", snakeName)
	case eventstream.EventDelete:
		return fmt.Sprintf("kora.%s.after_delete", snakeName)
	case eventstream.EventSubmit:
		return fmt.Sprintf("kora.%s.after_submit", snakeName)
	case eventstream.EventCancel:
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
	return w.RetryDeadLettersContext(context.Background())
}

// RetryDeadLettersContext schedules due retries without holding the source
// result set open while writes execute, and stops promptly when its owner drains.
func (w *Worker) RetryDeadLettersContext(ctx context.Context) error {
	if w == nil || w.DB == nil || w.Dialect == nil {
		return fmt.Errorf("webhook retry requires database and dialect")
	}
	if ctx == nil {
		return fmt.Errorf("webhook retry requires a context")
	}
	releaseQuery, err := w.acquireDBWork(ctx)
	if err != nil {
		return fmt.Errorf("admit dead-letter webhook query: %w", err)
	}
	rows, err := w.DB.QueryContext(ctx, db.Rebind(w.Dialect,
		`SELECT id, extension_name, event_id, event_type, endpoint_url, attempt
		 FROM _kora_webhook_delivery
		 WHERE status = 'dead_lettered' AND attempt < ?
		 ORDER BY created_at LIMIT 100`), len(w.RetrySchedule))
	if err != nil {
		releaseQuery()
		return fmt.Errorf("query dead-lettered webhook deliveries: %w", err)
	}
	type retry struct {
		id      string
		attempt int
		next    time.Time
	}
	retries := make([]retry, 0)
	for rows.Next() {
		var id, extName, eventID, eventType, endpointURL string
		var attempt int
		if err := rows.Scan(&id, &extName, &eventID, &eventType, &endpointURL, &attempt); err != nil {
			_ = rows.Close()
			releaseQuery()
			return fmt.Errorf("scan dead-lettered webhook delivery: %w", err)
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
		retries = append(retries, retry{id: id, attempt: attempt, next: time.Now().Add(jitter)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		releaseQuery()
		return fmt.Errorf("iterate dead-lettered webhook deliveries: %w", err)
	}
	if err := rows.Close(); err != nil {
		releaseQuery()
		return fmt.Errorf("close dead-lettered webhook deliveries: %w", err)
	}
	releaseQuery()
	retried := 0
	for _, item := range retries {
		releaseUpdate, err := w.acquireDBWork(ctx)
		if err != nil {
			return fmt.Errorf("admit webhook retry %s: %w", item.id, err)
		}
		result, err := w.DB.ExecContext(ctx, db.Rebind(w.Dialect,
			`UPDATE _kora_webhook_delivery SET status = 'retrying', attempt = ?, next_retry_at = ?
			 WHERE id = ? AND status = 'dead_lettered' AND attempt = ?`),
			item.attempt+1, item.next, item.id, item.attempt)
		if err != nil {
			releaseUpdate()
			return fmt.Errorf("schedule webhook retry %s: %w", item.id, err)
		}
		changed, err := result.RowsAffected()
		releaseUpdate()
		if err != nil {
			return fmt.Errorf("check webhook retry %s: %w", item.id, err)
		}
		retried += int(changed)
	}
	slog.Info("webhook: retried dead letters", "count", retried)
	return nil
}
