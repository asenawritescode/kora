package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/notifications"
	"github.com/asenawritescode/kora/internal/outbox"
)

// AnalyticsSiteWALDir returns the legacy route-name-keyed WAL partition. New
// Engine writers use AnalyticsTenantWALDir; this helper remains for migration
// and source compatibility with existing tests/tools.
func AnalyticsSiteWALDir(baseDir, siteName string) string {
	cleanSite := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(siteName))
	if cleanSite == "" || cleanSite == "." || cleanSite == ".." {
		cleanSite = "unknown-site"
	}
	return filepath.Join(baseDir, cleanSite)
}

// AnalyticsTenantWALDir returns a path-safe WAL partition keyed by stable
// tenant identity. Hashing avoids leaking IDs into paths and prevents route
// names, aliases, or untrusted path characters from changing the partition.
func AnalyticsTenantWALDir(baseDir, tenantID string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(tenantID)))
	return filepath.Join(baseDir, "tenant-"+hex.EncodeToString(hash[:]))
}

func RunRealtimeBridge(ctx context.Context, siteName string, bus analytics.EventBus, provider *natsprovider.Provider) {
	ch, err := bus.Subscribe()
	if err != nil {
		slog.Warn("realtime bridge subscribe failed", "site", siteName, "error", err)
		return
	}
	subjectPrefix := provider.Config().SubjectPrefix + ".realtime." + siteName + ".changes"
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(map[string]any{
				"id":          event.ID,
				"type":        "change",
				"transport":   "nats",
				"site":        event.Site,
				"resource":    "doctype:" + event.Doctype,
				"doctype":     event.Doctype,
				"doc_name":    event.DocName,
				"operation":   event.Operation,
				"occurred_at": event.Timestamp,
				"payload":     event.Data,
			})
			if err != nil {
				slog.Warn("realtime bridge marshal failed", "site", siteName, "error", err)
				continue
			}
			_ = provider.PublishSubject(ctx, subjectPrefix, payload, contract.NewEventID())
		}
	}
}

func RunAnalyticsRebuildConsumer(ctx context.Context, siteName string, provider *natsprovider.Provider, runner *analytics.RebuildRunner) {
	cfg := provider.Config()
	cfg.ConsumerName = siteName + "-analytics-rebuilds"
	cfg.ConsumerSubject = cfg.SubjectPrefix + ".tasks.analytics-rebuild"
	consumer, err := natsprovider.NewConsumer(provider, cfg)
	if err != nil {
		slog.Warn("analytics rebuild consumer init failed", "site", siteName, "error", err)
		return
	}
	handler := func(ctx context.Context, delivery contract.Delivery) error {
		var request struct {
			JobID string `json:"job_id"`
			Site  string `json:"site"`
		}
		if err := json.Unmarshal(delivery.Data, &request); err != nil {
			return err
		}
		if request.JobID == "" || request.Site != siteName {
			return fmt.Errorf("invalid analytics rebuild job payload")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		runner.Notify()
		return nil
	}
	slog.Info("analytics rebuild consumer started", "site", siteName, "consumer", cfg.ConsumerName)
	RunNATSConsumerWithBackoff(ctx, siteName, cfg.ConsumerName, func(runCtx context.Context) error {
		return consumer.Run(runCtx, handler)
	})
}

func RunNATSOutboxSideEffects(ctx context.Context, siteName string, provider *natsprovider.Provider, bus analytics.EventBus) {
	cfg := provider.Config()
	cfg.ConsumerName = siteName + "-sideeffects"
	// The shared stream also carries browser realtime payloads, Cloud events,
	// tasks, and dead letters. Consuming the entire prefix feeds realtime
	// messages back into the analytics bus, which republishes them forever.
	// Outbox event types are canonically prefixed with "kora." before the
	// provider adds its own subject prefix, so subscribe only to that namespace.
	cfg.ConsumerSubject = OutboxSideEffectSubject(cfg.SubjectPrefix)
	cfg.MaxDeliver = 5
	consumer, err := natsprovider.NewConsumer(provider, cfg)
	if err != nil {
		slog.Warn("nats side-effect bridge init failed", "site", siteName, "error", err)
		return
	}

	handler := func(ctx context.Context, delivery contract.Delivery) error {
		event, err := DecodeOutboxDelivery(delivery)
		if err != nil {
			return err
		}
		// Every site has its own durable consumer on the shared stream. A
		// consumer must acknowledge events for other sites without publishing
		// them into its local bus.
		if event.Site != siteName {
			return nil
		}
		if bus != nil {
			return bus.Publish(event)
		}
		return nil
	}

	slog.Info("nats side-effect bridge started", "site", siteName, "consumer", cfg.ConsumerName)
	RunNATSConsumerWithBackoff(ctx, siteName, cfg.ConsumerName, func(runCtx context.Context) error {
		return consumer.Run(runCtx, handler)
	})
}

// RunNATSConsumerWithBackoff restarts a durable consumer after transport or
// settlement errors. The JetStream durable retains unsettled messages while
// the generation-owned worker waits; cancellation stops the retry timer and
// lets the runtime join this worker before closing its provider.
func RunNATSConsumerWithBackoff(ctx context.Context, siteName, consumerName string, run func(context.Context) error) {
	delay := 250 * time.Millisecond
	const maxDelay = 10 * time.Second
	for ctx.Err() == nil {
		err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("consumer returned without runtime cancellation")
		}
		slog.Warn("durable NATS consumer stopped; retrying", "site", siteName, "consumer", consumerName, "retry_in", delay, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func OutboxSideEffectSubject(prefix string) string {
	return prefix + "." + prefix + ".>"
}

func DecodeOutboxDelivery(delivery contract.Delivery) (analytics.ChangeEvent, error) {
	var envelope contract.EventEnvelope
	if err := json.Unmarshal(delivery.Data, &envelope); err != nil {
		return analytics.ChangeEvent{}, err
	}
	if err := envelope.Validate(); err != nil {
		return analytics.ChangeEvent{}, fmt.Errorf("invalid outbox event envelope: %w", err)
	}

	var payload struct {
		Data    map[string]any `json:"data"`
		OldData map[string]any `json:"old_data"`
	}
	if len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, &payload); err != nil {
			return analytics.ChangeEvent{}, err
		}
	}

	return analytics.ChangeEvent{
		ID:         envelope.ID,
		Sequence:   string(envelope.Sequence),
		Site:       envelope.Site,
		Doctype:    envelope.AggregateType,
		DocName:    envelope.AggregateID,
		Operation:  OutboxEventOperation(envelope.Type),
		Timestamp:  envelope.OccurredAt,
		ModifiedBy: envelope.Source,
		Data:       payload.Data,
		OldData:    payload.OldData,
	}, nil
}

func OutboxEventOperation(eventType string) analytics.EventOp {
	switch {
	case strings.HasSuffix(eventType, ".after_insert"):
		return analytics.EventInsert
	case strings.HasSuffix(eventType, ".after_delete"):
		return analytics.EventDelete
	case strings.HasSuffix(eventType, ".after_submit"):
		return analytics.EventSubmit
	case strings.HasSuffix(eventType, ".after_cancel"):
		return analytics.EventCancel
	default:
		return analytics.EventUpdate
	}
}

// RunOutboxPublisher repeatedly publishes bounded due batches from the durable
// tenant outbox through its configured destination.
func RunOutboxPublisher(ctx context.Context, p *outbox.Publisher, admission *dbaccess.Admission, backlog ...*outbox.BacklogMetrics) {
	if ctx == nil || p == nil {
		return
	}
	p.WorkAdmission = admission
	var snapshot *outbox.BacklogMetrics
	if len(backlog) > 0 {
		snapshot = backlog[0]
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var refreshAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if _, err := p.PublishDue(publishCtx, 100); err != nil && !errors.Is(err, context.Canceled) {
				recordOutboxPublishError(snapshot, err)
				slog.Warn("outbox publisher: publish due failed", "error", err)
			}
			if snapshot != nil && (refreshAt.IsZero() || !time.Now().Before(refreshAt)) {
				if err := snapshot.Refresh(publishCtx, p); err != nil && !errors.Is(err, context.Canceled) {
					slog.Warn("outbox publisher: backlog metrics refresh failed", "error", err)
				}
				refreshAt = time.Now().Add(30 * time.Second)
			}
			cancel()
		}
	}
}

func recordOutboxPublishError(snapshot *outbox.BacklogMetrics, err error) {
	if snapshot != nil && errors.Is(err, outbox.ErrLeaseLost) {
		snapshot.RecordLeaseLost()
	}
}

// RunWebhookOutboxDispatcher consumes durable document events and keeps the
// event-level receipt behind successful external delivery. Its batch and
// active-work admission are bounded; idle tenants retain no polling goroutine
// after their runtime generation is retired.
func RunWebhookOutboxDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher, admission *dbaccess.Admission) {
	if ctx == nil || dispatcher == nil {
		return
	}
	// The dispatcher admits its SQL selection, receipt and local transaction
	// units individually. External webhook HTTP calls must not hold a database
	// work permit while waiting on extension servers.
	dispatcher.WorkAdmission = admission
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := dispatcher.Run(workCtx, 25); err != nil && ctx.Err() == nil {
			slog.Warn("webhook outbox: dispatch failed", "error", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunRequestNotificationDispatcher durably plans request notifications from
// tenant outbox rows. Failed planning leaves the event unreceipted for retry.
func RunRequestNotificationDispatcher(ctx context.Context, dispatcher *outbox.Dispatcher) {
	if ctx == nil || dispatcher == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		processed, err := dispatcher.Run(workCtx, 25)
		if err != nil && ctx.Err() == nil {
			slog.Warn("request notification outbox: dispatch failed", "error_type", fmt.Sprintf("%T", err))
		}
		cancel()
		if processed > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunRequestNotificationWorker retries transient database and local-bus
// failures while the durable delivery row remains available for recovery.
func RunRequestNotificationWorker(ctx context.Context, worker *notifications.Worker) {
	if ctx == nil || worker == nil {
		return
	}
	delay := time.Second
	for ctx.Err() == nil {
		workCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		accepted, err := worker.RunOnce(workCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("request notification delivery: attempt failed", "error_type", fmt.Sprintf("%T", err))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
			continue
		}
		delay = time.Second
		if accepted > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
